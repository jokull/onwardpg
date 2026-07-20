// Package historyinit creates the verified root entry for an onwardpg-owned
// migration history. It materializes declarative DDL and executes the proposed
// genesis only in disposable databases; it has no caller-database apply path.
package historyinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jokull/onwardpg/internal/activeplan"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/targetlock"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
)

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	Outcome            string                `json:"status"`
	Target             string                `json:"target"`
	BundleID           string                `json:"bundle_id"`
	Path               string                `json:"path,omitempty"`
	HistoryHead        string                `json:"history_head,omitempty"`
	DesiredFingerprint string                `json:"desired_fingerprint,omitempty"`
	Plan               *protocol.Result      `json:"plan,omitempty"`
	Verification       *verify.Report        `json:"verification,omitempty"`
	LocalState         activeplan.GitHygiene `json:"local_state"`
	Findings           []Finding             `json:"findings,omitempty"`
}

type Input struct {
	Root            string
	ConfigPath      string
	Config          workspace.Config
	TargetName      string
	Target          workspace.Target
	AdminURL        string
	BundleID        string
	BuildVersion    string
	BuildIdentity   *bundle.BuildIdentity
	Ignores         []string
	RequiredIgnores []string
	PlannerOptions  graphplan.Options
}

// Run creates one content-addressed baseline entry from an empty PostgreSQL
// catalog to the configured declarative schema. The artifact is clone-verified
// before it is installed in the real bundle root.
func Run(ctx context.Context, input Input) (Report, error) {
	report := Report{
		Outcome:  "error",
		Target:   input.TargetName,
		BundleID: input.BundleID,
	}
	if input.Root == "" || !filepath.IsAbs(input.Root) {
		return report, fmt.Errorf("history init root must be absolute")
	}
	if input.ConfigPath == "" || !filepath.IsAbs(input.ConfigPath) {
		return report, fmt.Errorf("history init configuration path must be absolute")
	}
	if input.AdminURL == "" {
		return report, fmt.Errorf("disposable database admin URL is required")
	}
	if input.BundleID == "" {
		return report, fmt.Errorf("baseline bundle id is required")
	}
	if input.BuildVersion == "" {
		return report, fmt.Errorf("planner build version is required")
	}
	localState, err := activeplan.EnsureGitExclude(input.Root)
	report.LocalState = localState
	if err != nil {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code: "local_state_git_collision", Message: err.Error(),
			Remediation: "keep .onwardpg as untracked worktree-local state; durable migration bundles belong under the configured bundle root",
		}}
		return report, nil
	}
	postgresMajor, err := source.PostgresMajor(ctx, input.AdminURL)
	if err != nil {
		return report, err
	}

	chain, err := history.Load(input.Root, input.Config.BundleRoot, input.TargetName)
	if err != nil {
		return report, err
	}
	if len(chain.Entries) != 0 {
		report.Outcome = "blocked"
		report.HistoryHead = chain.HeadDigest
		report.Findings = []Finding{{
			Code:        "history_already_initialized",
			Message:     fmt.Sprintf("target %s already has %d history entries", input.TargetName, len(chain.Entries)),
			Remediation: "use onwardpg draft; init is only for an empty target history",
		}}
		return report, nil
	}

	compiled, err := workspace.CompileDDL(ctx, input.Root, input.TargetName, input.Target)
	if err != nil {
		return report, fmt.Errorf("compile declarative schema: %w", err)
	}
	empty, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-postgresql", input.AdminURL, input.Ignores)
	if err != nil {
		return report, fmt.Errorf("inspect empty PostgreSQL baseline: %w", err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return report, fmt.Errorf("inspect declarative schema: %w", err)
	}
	if err := source.ValidateIgnoreSelectors(input.RequiredIgnores, empty, desired); err != nil {
		return report, err
	}
	activeIgnores, err := source.ActiveIgnoreSelectors(input.Ignores, empty, desired)
	if err != nil {
		return report, err
	}
	input.Ignores = activeIgnores
	plan, err := graphplan.Build(empty, desired, protocol.Answers{}, input.PlannerOptions)
	if err != nil {
		return report, fmt.Errorf("plan baseline: %w", err)
	}
	report.DesiredFingerprint = plan.DesiredFingerprint
	if plan.Status != protocol.Planned {
		report.Plan = &plan
		report.Outcome = string(plan.Status)
		return report, nil
	}
	plan = authoritativeBaselinePlan(plan, compiled.DDL)
	report.Plan = &plan

	metadata := bundle.Metadata{
		BundleID:   input.BundleID,
		Generation: 1,
		Target:     input.TargetName,
		Purpose:    "baseline",
		BaselineSource: bundle.SourceReceipt{
			Kind: "ddl", Description: "empty PostgreSQL catalog",
			Fingerprint: plan.CurrentFingerprint, PostgresMajor: postgresMajor,
		},
		DesiredSource: bundle.SourceReceipt{
			Kind: "ddl_export", Description: "baseline declarative " + compiled.Provenance,
			Fingerprint: plan.DesiredFingerprint, PostgresMajor: postgresMajor,
		},
		Planner: bundle.PlannerReceipt{
			Version: input.BuildVersion,
			Build:   input.BuildIdentity,
			Options: bundle.PlannerOptions{
				ConcurrentIndexes:       input.PlannerOptions.ConcurrentIndexes,
				IfNotExists:             input.PlannerOptions.IfNotExists,
				IfExists:                input.PlannerOptions.IfExists,
				CascadeDrops:            input.PlannerOptions.CascadeDrops,
				SchemaQualifier:         input.PlannerOptions.SchemaQualifier,
				IgnoreExtensionVersions: append([]string(nil), input.PlannerOptions.IgnoreExtensionVersions...),
			},
			IgnoreSelectors: append([]string(nil), input.Ignores...),
		},
		HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{
		Metadata:  metadata,
		Result:    plan,
		Questions: plan.Questions,
		Attempt:   1,
	})
	if err != nil {
		return report, fmt.Errorf("build baseline bundle: %w", err)
	}
	lock, err := targetlock.Acquire(input.ConfigPath, input.TargetName)
	if err != nil {
		if errors.Is(err, targetlock.ErrBusy) {
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code:        "history_init_in_progress",
				Message:     "another history lifecycle command owns the target lock",
				Remediation: "wait for the other command and retry; the operating system releases this lock automatically if its owner exits",
			}}
			return report, nil
		}
		return report, err
	}
	defer lock.Release()
	chain, err = history.Load(input.Root, input.Config.BundleRoot, input.TargetName)
	if err != nil {
		return report, err
	}
	if len(chain.Entries) != 0 {
		report.Outcome = "blocked"
		report.HistoryHead = chain.HeadDigest
		report.Findings = []Finding{{
			Code:        "history_changed_during_init",
			Message:     "target history changed before the baseline lock was acquired",
			Remediation: "inspect the new history head and do not overwrite it",
		}}
		return report, nil
	}

	stagingRoot, err := os.MkdirTemp("", "onwardpg-history-init-")
	if err != nil {
		return report, fmt.Errorf("create history init staging root: %w", err)
	}
	defer os.RemoveAll(stagingRoot)
	stagingDestination, err := input.Config.BundlePath(stagingRoot, input.TargetName, input.BundleID)
	if err != nil {
		return report, err
	}
	if err := bundle.Write(stagingDestination, artifact, bundle.WriteOptions{}); err != nil {
		return report, fmt.Errorf("stage baseline bundle: %w", err)
	}
	stagedChain, err := history.Load(stagingRoot, input.Config.BundleRoot, input.TargetName)
	if err != nil {
		return report, fmt.Errorf("load staged baseline history: %w", err)
	}
	verification, err := verify.Run(ctx, verify.Input{
		AdminURL: input.AdminURL, Chain: stagedChain, BundleID: input.BundleID,
		ThroughPhase: "contract", Ignores: input.Ignores, Options: input.PlannerOptions,
	})
	if err != nil {
		return report, fmt.Errorf("verify baseline bundle: %w", err)
	}
	report.Verification = &verification
	if verification.Outcome != "verified" {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "baseline_non_convergent",
			Message:     fmt.Sprintf("generated baseline clone verification finished with status %s", verification.Outcome),
			Remediation: "inspect verification.residual and correct the declarative DDL, ignore boundary, or planner before retrying init",
		}}
		return report, nil
	}

	chain, err = history.Load(input.Root, input.Config.BundleRoot, input.TargetName)
	if err != nil {
		return report, err
	}
	if len(chain.Entries) != 0 {
		report.Outcome = "blocked"
		report.HistoryHead = chain.HeadDigest
		report.Findings = []Finding{{
			Code:        "history_changed_during_init",
			Message:     "target history changed while the baseline was being verified",
			Remediation: "inspect the new history head and do not overwrite it",
		}}
		return report, nil
	}
	if err := lock.ValidatePath(); err != nil {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "configuration_changed_during_init",
			Message:     err.Error(),
			Remediation: "rerun init against the current stable configuration; no baseline was installed",
		}}
		return report, nil
	}
	if err := workspace.RequireUnchanged(input.ConfigPath, input.Config); err != nil {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "configuration_changed_during_init",
			Message:     err.Error(),
			Remediation: "rerun init against the current stable configuration; no baseline was installed",
		}}
		return report, nil
	}
	compiledAfter, err := workspace.CompileDDL(ctx, input.Root, input.TargetName, input.Target)
	if err != nil {
		return report, fmt.Errorf("recompile desired schema after baseline verification: %w", err)
	}
	desiredAfter, err := source.LoadDDLGraphForComparison(ctx, compiledAfter.DDL, compiledAfter.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return report, fmt.Errorf("rematerialize desired schema after baseline verification: %w", err)
	}
	desiredAfterFingerprint, err := graphplan.Fingerprint(desiredAfter, input.PlannerOptions)
	if err != nil {
		return report, fmt.Errorf("fingerprint desired schema after baseline verification: %w", err)
	}
	if desiredAfterFingerprint != plan.DesiredFingerprint {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "desired_schema_changed_during_init",
			Message:     fmt.Sprintf("configured desired schema changed from %s to %s during baseline verification", plan.DesiredFingerprint, desiredAfterFingerprint),
			Remediation: "rerun init against the current stable DDL; no baseline was installed",
		}}
		return report, nil
	}
	destination, err := input.Config.BundlePath(input.Root, input.TargetName, input.BundleID)
	if err != nil {
		return report, err
	}
	if err := bundle.Write(destination, artifact, bundle.WriteOptions{}); err != nil {
		return report, fmt.Errorf("install baseline bundle: %w", err)
	}
	relative, err := filepath.Rel(input.Root, destination)
	if err != nil {
		return report, err
	}
	report.Outcome = "initialized"
	report.Path = filepath.ToSlash(relative)
	report.HistoryHead = artifact.Manifest.History.EntryDigest
	// The complete authoritative DDL is receipted in the installed bundle.
	// Repeating it inside a successful init response makes large-schema JSON
	// needlessly enormous; unsuccessful inventory and verification reports keep
	// their plan details for diagnosis.
	report.Plan = nil
	return report, nil
}

// authoritativeBaselinePlan keeps init a ground-floor operation rather than
// pretending the catalog graph can reconstruct session scaffolding from the
// exported DDL. The graph-derived plan above is still the safety inventory: it
// must be fully supported (or narrowly ignored) before this replay plan is
// accepted. Feature migrations continue to be rendered from the typed graph.
func authoritativeBaselinePlan(inventory protocol.Result, ddl []byte) protocol.Result {
	if len(inventory.Statements) == 0 {
		return inventory
	}
	item := protocol.Statement{
		SQL: string(ddl), Safety: "review", Hazards: []string{"baseline_replay"},
		Phase: protocol.PhaseExpand, NonTransactional: true,
	}
	item.ID = protocol.StableStatementID(item)
	inventory.Statements = []protocol.Statement{item}
	inventory.Batches = []protocol.Batch{{
		ID: "batch-expand-001", Phase: protocol.PhaseExpand,
		Transactional: false, Statements: []protocol.Statement{item},
	}}
	// Contract gates describe the graph planner's staged route from an empty
	// catalog. The authoritative baseline replaces that route with one exact DDL
	// replay, so retaining those now-unreferenced gates would misrepresent init
	// and make the baseline bundle invalid.
	inventory.ContractGates = nil
	inventory.Reconciliations = nil
	return inventory
}
