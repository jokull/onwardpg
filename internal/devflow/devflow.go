// Package devflow plans a caller-owned development catalog toward exported
// working DDL. It never writes bundles or executes SQL. In workspace mode it
// preserves surplus state so branch switches cannot turn absence from DDL into
// a destructive local cleanup proposal.
package devflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/contractcheck"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/semantichint"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/workspace"
)

type Input struct {
	Root            string
	TargetName      string
	Target          workspace.Target
	DevURL          string
	AdminURL        string
	Hints           []protocol.Hint
	Ignores         []string
	RequiredIgnores []string
	PlannerOptions  graphplan.Options
	Postconditions  []Postcondition
}

// Postcondition is a deliberately opted-in boolean assertion from accepted
// history. SQL is supplied only by a receipted verify.sql file and is executed
// under PostgreSQL's read-only transaction mode.
type Postcondition struct {
	BundleID string
	Path     string
	ID       string
	SQL      string
}

type PostconditionResult struct {
	BundleID string `json:"bundle_id"`
	Path     string `json:"path"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

type Report struct {
	Status         protocol.Status       `json:"status"`
	NextAction     string                `json:"next_action,omitempty"`
	Result         protocol.Result       `json:"result"`
	Decisions      []protocol.Decision   `json:"decisions,omitempty"`
	AppliedHints   []protocol.Hint       `json:"applied_hints,omitempty"`
	DeferredHints  []protocol.Hint       `json:"deferred_hints,omitempty"`
	Postconditions []PostconditionResult `json:"accepted_postconditions,omitempty"`
}

func Run(ctx context.Context, input Input) (Report, error) {
	if input.Root == "" {
		return Report{}, fmt.Errorf("development plan root is required")
	}
	if input.DevURL == "" || input.AdminURL == "" {
		return Report{}, fmt.Errorf("development and disposable database URLs are required")
	}
	developmentMajor, err := source.PostgresMajor(ctx, input.DevURL)
	if err != nil {
		return Report{}, fmt.Errorf("inspect development PostgreSQL major: %w", err)
	}
	scratchMajor, err := source.PostgresMajor(ctx, input.AdminURL)
	if err != nil {
		return Report{}, fmt.Errorf("inspect scratch PostgreSQL major: %w", err)
	}
	if err := validatePostgresMajors(developmentMajor, scratchMajor); err != nil {
		return Report{}, err
	}
	compiled, err := workspace.CompileDDL(ctx, input.Root, input.TargetName, input.Target)
	if err != nil {
		return Report{}, fmt.Errorf("compile desired schema: %w", err)
	}
	current, observer, observerFinding, err := contractcheck.InspectObserverCatalog(ctx, input.DevURL, input.Ignores, 30*time.Second)
	if err != nil {
		return Report{}, fmt.Errorf("inspect development catalog: %w", err)
	}
	if observerFinding != nil {
		return Report{Status: protocol.Unsupported, Result: protocol.Result{
			Status:      protocol.Unsupported,
			Unsupported: []string{"development_observer_" + observerFinding.Code + ":" + observerFinding.Message},
		}}, nil
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return Report{}, fmt.Errorf("materialize desired schema: %w", err)
	}
	if err := source.ValidateIgnoreSelectors(input.RequiredIgnores, current, desired); err != nil {
		return Report{}, err
	}
	options := input.PlannerOptions
	options.PreserveSurplus = input.Target.WorkspaceMode()
	options.DirectColumnRenames = true
	resolution, err := semantichint.Resolve(current, desired, input.Hints, options)
	if err != nil {
		return Report{}, fmt.Errorf("plan development reconciliation: %w", err)
	}
	report := Report{
		Status: resolution.Result.Status, Result: resolution.Result,
		AppliedHints: resolution.Hints, DeferredHints: resolution.Deferred,
	}
	for _, projected := range observer.ProjectedAccess {
		report.Result.Compatibility = append(report.Result.Compatibility, "observer_access_projected:"+projected)
	}
	if report.Status == protocol.NeedsInput {
		report.NextAction = "rerun_same_command_with_hints"
		report.Decisions, err = semantichint.Decisions(report.Result.Questions, current, desired)
		if err != nil {
			return Report{}, fmt.Errorf("render development decisions: %w", err)
		}
	}
	if len(input.Postconditions) > 0 {
		report.Postconditions, err = evaluatePostconditions(ctx, input.DevURL, input.Postconditions)
		if err != nil {
			return Report{}, fmt.Errorf("evaluate development postconditions: %w", err)
		}
	}
	return report, nil
}

func validatePostgresMajors(developmentMajor, scratchMajor int) error {
	if developmentMajor != scratchMajor {
		return fmt.Errorf("development PostgreSQL major %d does not match scratch PostgreSQL major %d", developmentMajor, scratchMajor)
	}
	return nil
}

func evaluatePostconditions(ctx context.Context, databaseURL string, checks []Postcondition) ([]PostconditionResult, error) {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer connection.Close(ctx)
	results := make([]PostconditionResult, 0, len(checks))
	for _, check := range checks {
		result := PostconditionResult{BundleID: check.BundleID, Path: check.Path, ID: check.ID, Status: "failed"}
		transaction, beginErr := connection.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if beginErr != nil {
			return nil, beginErr
		}
		var passed bool
		if err := transaction.QueryRow(ctx, check.SQL).Scan(&passed); err != nil {
			result.Message = "read-only assertion query failed: " + err.Error()
		} else if !passed {
			result.Message = "assertion returned false"
		} else {
			result.Status = "passed"
		}
		if rollbackErr := transaction.Rollback(ctx); rollbackErr != nil {
			return nil, rollbackErr
		}
		results = append(results, result)
	}
	return results, nil
}
