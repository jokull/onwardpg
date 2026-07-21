// Package draftflow plans one explicitly selected, Git-free feature bundle
// from the replayed onwardpg history head to the current declarative schema.
package draftflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/semantichint"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/targetlock"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
	"github.com/jokull/onwardpg/pgschema"
)

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// AcceptedStep inventories one artifact in base history that arrived beneath
// the selected feature plan. It is evidence only: onwardpg never replays these
// bytes into a caller-owned development database. Generated structural phases
// are listed separately from work whose data or operational effects cannot be
// proven from a catalog snapshot.
type AcceptedStep struct {
	BundleID       string `json:"bundle_id"`
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Reason         string `json:"reason"`
	RequiresReview bool   `json:"requires_review"`
}

// DevelopmentPostcondition is an explicitly opted-in boolean assertion from
// newly accepted history. SQL stays internal to the command handoff; the
// public report exposes only its evaluated evidence. It is run only in a
// read-only transaction against a caller-owned development database.
type DevelopmentPostcondition struct {
	BundleID string
	Path     string
	ID       string
	SQL      string
}

type Report struct {
	Outcome                   string                     `json:"status"`
	Target                    string                     `json:"target"`
	BundleID                  string                     `json:"bundle_id"`
	PlanID                    string                     `json:"plan_id,omitempty"`
	Path                      string                     `json:"path,omitempty"`
	Generation                int                        `json:"generation,omitempty"`
	HistoryHead               string                     `json:"base_history_head,omitempty"`
	BaseBundle                string                     `json:"base_bundle,omitempty"`
	BaseRef                   string                     `json:"base_ref,omitempty"`
	AssertedBaseRef           string                     `json:"asserted_base_ref,omitempty"`
	PreviousParent            string                     `json:"previous_parent,omitempty"`
	ParentChanged             bool                       `json:"parent_changed,omitempty"`
	AcceptedSteps             []AcceptedStep             `json:"accepted_steps_requiring_review,omitempty"`
	DevelopmentPostconditions []DevelopmentPostcondition `json:"-"`
	CurrentFingerprint        string                     `json:"current_fingerprint,omitempty"`
	DesiredFingerprint        string                     `json:"desired_fingerprint,omitempty"`
	Plan                      *protocol.Result           `json:"generated_plan,omitempty"`
	Decisions                 []protocol.Decision        `json:"decisions,omitempty"`
	DeferredHints             []protocol.Hint            `json:"deferred_hints,omitempty"`
	EditFiles                 []string                   `json:"edit_files,omitempty"`
	EditRequirements          []EditRequirement          `json:"edit_requirements,omitempty"`
	AnswerRebind              *protocol.RebindReport     `json:"answer_rebind,omitempty"`
	EditReconciliation        *bundle.EditReconciliation `json:"edit_reconciliation,omitempty"`
	Verification              *verify.Report             `json:"verification,omitempty"`
	Findings                  []Finding                  `json:"findings,omitempty"`
	RemovedBundle             bool                       `json:"removed_bundle,omitempty"`
	CreatedBundle             bool                       `json:"created_bundle,omitempty"`
	WrittenReceipts           []string                   `json:"written_receipts,omitempty"`
}

type EditRequirement struct {
	Path          string `json:"path"`
	PocketID      string `json:"pocket_id"`
	Purpose       string `json:"purpose"`
	Phase         string `json:"phase"`
	ExecutionMode string `json:"execution_mode"`
	RequiredProof string `json:"required_proof"`
}

type Input struct {
	Root            string
	ConfigPath      string
	Config          workspace.Config
	TargetName      string
	Target          workspace.Target
	AdminURL        string
	BundleID        string
	PlanID          string
	AfterRef        string
	InferBase       bool
	Create          bool
	BuildVersion    string
	BuildIdentity   *bundle.BuildIdentity
	Purpose         string
	Hints           []protocol.Hint
	HintsGiven      bool
	Ignores         []string
	RequiredIgnores []string
	PlannerOptions  graphplan.Options
}

// Run replaces one explicitly selected draft while treating every other entry
// as base history. Verification receipts do not freeze the selected draft: it
// remains mutable for the life of the feature. All SQL execution happens only
// in disposable databases created through AdminURL.
func Run(ctx context.Context, input Input) (Report, error) {
	report := Report{
		Outcome:  "error",
		Target:   input.TargetName,
		BundleID: input.BundleID,
		PlanID:   input.PlanID,
	}
	if input.Root == "" || !filepath.IsAbs(input.Root) {
		return report, fmt.Errorf("draft root must be absolute")
	}
	if input.ConfigPath == "" || !filepath.IsAbs(input.ConfigPath) {
		return report, fmt.Errorf("draft configuration path must be absolute")
	}
	if input.AdminURL == "" {
		return report, fmt.Errorf("disposable database admin URL is required")
	}
	if input.BundleID == "" {
		return report, fmt.Errorf("bundle id is required")
	}
	if input.BuildVersion == "" {
		return report, fmt.Errorf("planner build version is required")
	}
	if input.Purpose == "" {
		input.Purpose = "feature"
	}
	report.AssertedBaseRef = input.AfterRef
	var assertedBundle, assertedDigest string
	if input.AfterRef == "" && !input.InferBase {
		statusCommand := "onwardpg history status --target " + input.TargetName
		if !input.Create {
			statusCommand += " --bundle " + input.BundleID
		}
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "base_anchor_required",
			Message:     "draft requires the coding agent to identify the exact accepted history head",
			Remediation: "run " + statusCommand + " in the intended base checkout, then pass its head_ref to --after",
		}}
		return report, nil
	}
	if input.AfterRef != "" {
		var parseErr error
		assertedBundle, assertedDigest, parseErr = history.ParseHeadRef(input.AfterRef)
		if parseErr == nil {
			// The explicit anchor remains a supported expert/CI assertion.
			// Ordinary `onwardpg plan` sets InferBase and derives this same head
			// after excluding its selected mutable bundle.
		} else {
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code:        "invalid_base_anchor",
				Message:     parseErr.Error(),
				Remediation: "copy head_ref exactly from onwardpg history status --target " + input.TargetName,
			}}
			return report, nil
		}
	}
	lock, err := targetlock.Acquire(input.ConfigPath, input.TargetName)
	if err != nil {
		if errors.Is(err, targetlock.ErrBusy) {
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code:        "history_update_in_progress",
				Message:     err.Error(),
				Remediation: "wait for the other command and retry; the operating system releases this lock automatically if its owner exits",
			}}
			return report, nil
		}
		return report, err
	}
	defer lock.Release()
	postgresMajor, err := source.PostgresMajor(ctx, input.AdminURL)
	if err != nil {
		return report, err
	}

	destination, err := input.Config.BundlePath(input.Root, input.TargetName, input.BundleID)
	if err != nil {
		return report, err
	}
	if full, fullErr := history.Load(input.Root, input.Config.BundleRoot, input.TargetName); fullErr == nil {
		for index, entry := range full.Entries {
			if entry.Directory != input.BundleID || index == len(full.Entries)-1 {
				continue
			}
			report.Outcome = "blocked"
			report.HistoryHead = full.HeadDigest
			report.BaseBundle = full.Entries[len(full.Entries)-1].Directory
			report.BaseRef = history.HeadRef(full)
			report.Findings = []Finding{{
				Code:        "selected_bundle_not_head",
				Message:     fmt.Sprintf("selected bundle %s is accepted history with a successor; current head is %s", input.BundleID, report.BaseBundle),
				Remediation: "do not rewrite historical entries; choose a new feature bundle ID and create it after the current exact head_ref",
			}}
			return report, nil
		}
	}
	chain, selected, err := history.LoadExcluding(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
	if err != nil {
		strictErr := err
		chain, selected, err = history.LoadEditedDraft(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
		if err != nil {
			code := "invalid_history"
			message := fmt.Sprintf("strict history validation failed: %v; editable bundle preparation failed: %v", strictErr, err)
			remediation := "restore one complete hash-chained base and keep only the PR-owned bundle selected for replacement"
			if strings.Contains(err.Error(), "ONWARDPG TODO") {
				code = "unresolved_sql_todo"
				message = err.Error()
				remediation = "replace the remaining TODO at the reported phase line, then rerun onwardpg plan"
			}
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code: code, Message: message, Remediation: remediation,
			}}
			return report, nil
		}
	}
	if len(chain.Entries) == 0 {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "history_not_initialized",
			Message:     fmt.Sprintf("target %s has no replayable history ground floor", input.TargetName),
			Remediation: "run onwardpg init --target " + input.TargetName,
		}}
		return report, nil
	}
	if input.Create && selected != nil {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "selected_bundle_already_exists",
			Message:     fmt.Sprintf("bundle %s already exists; --create is only valid for its first draft invocation", input.BundleID),
			Remediation: "rerun the same draft command without --create so onwardpg must preserve and refresh the existing bundle",
		}}
		return report, nil
	}
	if !input.Create && selected == nil {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "selected_bundle_missing",
			Message:     fmt.Sprintf("bundle %s does not exist, but this invocation requested a refresh", input.BundleID),
			Remediation: "if this is intentionally the first invocation for the feature, rerun once with --create; otherwise restore the missing bundle from Git before restacking",
		}}
		return report, nil
	}
	if selected != nil && input.PlanID != "" {
		if selected.Artifact.Manifest.PlanID == "" {
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code:        "selected_bundle_has_no_plan_id",
				Message:     fmt.Sprintf("selected bundle %s predates active-plan identity", input.BundleID),
				Remediation: "continue it with onwardpg draft, or create a new onwardpg plan with a new feature name",
			}}
			return report, nil
		}
		if selected.Artifact.Manifest.PlanID != input.PlanID {
			report.Outcome = "blocked"
			report.Findings = []Finding{{
				Code:        "active_plan_identity_mismatch",
				Message:     fmt.Sprintf("local active plan %s does not match selected bundle plan id %s", input.PlanID, selected.Artifact.Manifest.PlanID),
				Remediation: "select the correct feature plan explicitly or remove the stale local active-plan anchor",
			}}
			return report, nil
		}
	}
	for _, entry := range chain.Entries {
		recorded := entry.Artifact.Manifest.DesiredSource.PostgresMajor
		if recorded != 0 && recorded != postgresMajor {
			return report, fmt.Errorf("history bundle %s targets PostgreSQL %d but the scratch server is PostgreSQL %d", entry.Directory, recorded, postgresMajor)
		}
	}
	report.HistoryHead = chain.HeadDigest
	if len(chain.Entries) > 0 {
		report.BaseBundle = chain.Entries[len(chain.Entries)-1].Directory
	}
	report.BaseRef = history.HeadRef(chain)
	if input.AfterRef != "" && (report.BaseBundle != assertedBundle || chain.HeadDigest != assertedDigest) {
		report.Outcome = "blocked"
		report.Findings = []Finding{{
			Code:        "base_anchor_mismatch",
			Message:     fmt.Sprintf("asserted base head %q does not match the replayable base head %q", input.AfterRef, report.BaseRef),
			Remediation: "use Git to identify the accepted main-branch history, make the working tree contain that chain plus only this feature bundle, then pass the accepted checkout's exact head_ref to --after",
		}}
		return report, nil
	}
	if selected != nil {
		report.PreviousParent = selected.Artifact.Manifest.History.ParentDigest
		report.ParentChanged = report.PreviousParent != chain.HeadDigest
		if report.ParentChanged {
			report.AcceptedSteps = acceptedStepsSince(chain, report.PreviousParent)
			report.DevelopmentPostconditions, err = developmentPostconditionsSince(chain, report.PreviousParent)
			if err != nil {
				return report, fmt.Errorf("inventory accepted development postconditions: %w", err)
			}
		}
	}

	replay, err := chain.Replay()
	if err != nil {
		return report, fmt.Errorf("render base history: %w", err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return report, fmt.Errorf("replay base history: %w", err)
	}
	compiled, err := workspace.CompileDDL(ctx, input.Root, input.TargetName, input.Target)
	if err != nil {
		return report, fmt.Errorf("compile desired schema: %w", err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return report, fmt.Errorf("materialize desired schema: %w", err)
	}
	if err := source.ValidateIgnoreSelectors(input.RequiredIgnores, current, desired); err != nil {
		return report, err
	}
	activeIgnores, err := source.ActiveIgnoreSelectors(input.Ignores, current, desired)
	if err != nil {
		return report, err
	}
	input.Ignores = activeIgnores

	plan, rebind, answerReceipt, questions, hints, err := buildPlan(current, desired, input, selected)
	if err != nil {
		return report, err
	}
	if plan.Status == protocol.NeedsInput {
		report.DeferredHints = suppliedHintsNotReceipted(input.Hints, hints)
		report.Decisions, err = semantichint.Decisions(plan.Questions, current, desired)
		if err != nil {
			return report, fmt.Errorf("render semantic decisions: %w", err)
		}
	}
	report.Plan = &plan
	report.AnswerRebind = rebind
	report.CurrentFingerprint = plan.CurrentFingerprint
	report.DesiredFingerprint = plan.DesiredFingerprint
	report.Outcome = string(plan.Status)
	if plan.Status == protocol.Unsupported {
		// Unsupported state is actionable output, not durable migration history.
		// In particular, do not replace an existing selected draft with an
		// incomplete artifact while the agent changes the schema or strategy.
		// Answer carry-forward cannot be judged reliably until the structural
		// blocker is removed, so do not mislabel valid answers as invalidated.
		if report.AnswerRebind != nil {
			report.AnswerRebind = nil
			report.Findings = append(report.Findings, Finding{
				Code:        "answer_rebind_deferred",
				Message:     "existing decisions were not evaluated because structural planning stopped as unsupported",
				Remediation: "resolve the unsupported schema shape and rerun draft; still-applicable decisions will then be carried forward",
			})
		}
		for _, reason := range plan.Unsupported {
			if strings.HasPrefix(reason, "column_physical_order:") {
				report.Findings = append(report.Findings, Finding{
					Code:        "column_physical_order",
					Message:     reason,
					Remediation: "move newly added columns after retained upstream columns in the declarative CREATE TABLE order, or author and review a replacement-table migration",
				})
			}
		}
		return report, nil
	}
	if plan.Status == protocol.Planned && len(plan.Statements) == 0 {
		if selected == nil {
			report.Outcome = "no_changes"
			return report, nil
		}
		if selected.Artifact.Manifest.PhaseSource != "edited" {
			if err := ensureInputsUnchanged(ctx, input, lock, chain, selected, plan.DesiredFingerprint); err != nil {
				return historyChangedReport(report, err), nil
			}
			if err := bundle.RemoveDraft(destination, selected.Artifact); err != nil {
				return report, fmt.Errorf("remove upstream-absorbed draft: %w", err)
			}
			relative, pathErr := filepath.Rel(input.Root, destination)
			if pathErr != nil {
				return report, pathErr
			}
			report.Path = filepath.ToSlash(relative)
			report.Generation = selected.Artifact.Manifest.Generation
			report.Outcome = "absorbed"
			report.RemovedBundle = true
			report.Findings = []Finding{{
				Code:        "feature_absorbed_by_base",
				Message:     "the accepted base already produces the desired schema; the generated-only PR bundle was removed instead of recording an empty migration",
				Remediation: "remove the now-empty migration change from the PR and continue without stacking a replacement",
			}}
			return report, nil
		}
	}

	metadata := bundle.Metadata{
		BundleID: input.BundleID,
		PlanID:   input.PlanID,
		Target:   input.TargetName,
		Purpose:  input.Purpose,
		BaselineSource: bundle.SourceReceipt{
			Kind: "onwardpg_history", Description: replay.Provenance,
			Fingerprint: plan.CurrentFingerprint, PostgresMajor: postgresMajor,
		},
		DesiredSource: bundle.SourceReceipt{
			Kind: "ddl_export", Description: compiled.Provenance,
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
		HistoryParentDigest: chain.HeadDigest,
	}
	generation, attempt := 1, 1
	if selected != nil {
		generation, attempt, err = bundle.NextCoordinatesFromArtifact(selected.Artifact, metadata, plan)
	}
	if err != nil {
		return report, err
	}
	metadata.Generation = generation
	artifact, err := bundle.Build(bundle.Input{
		Metadata: metadata, Result: plan, Answers: answerReceipt,
		Hints: hints, Questions: questions, Attempt: attempt,
	})
	if err != nil {
		return report, fmt.Errorf("build draft bundle: %w", err)
	}
	if plan.Status == protocol.NeedsSQLEdits {
		report.EditRequirements = pendingEditRequirements(plan)
		for name, body := range artifact.Files {
			if strings.HasPrefix(name, "phases/") && strings.Contains(string(body), "ONWARDPG TODO") {
				report.EditFiles = append(report.EditFiles, name)
			}
		}
		sort.Strings(report.EditFiles)
	}
	if selected != nil && selected.Artifact.Manifest.PhaseSource == "edited" {
		if plan.Status != protocol.Planned && plan.Status != protocol.NeedsSQLEdits {
			relative, pathErr := filepath.Rel(input.Root, destination)
			if pathErr != nil {
				return report, pathErr
			}
			report.Path = filepath.ToSlash(relative)
			report.Generation = selected.Artifact.Manifest.Generation
			report.Findings = append(report.Findings, Finding{
				Code:        "edited_draft_waiting_for_decisions",
				Message:     "the cumulative plan needs decisions before developer-owned SQL can be reconciled",
				Remediation: "answer the reported questions and rerun draft with the same bundle id; the existing SQL has not been changed",
			})
			return report, nil
		}
		reconciled, reconciliation, err := bundle.ReconcileEditedDraft(selected.Artifact, artifact)
		if err != nil {
			return report, fmt.Errorf("reconcile developer-owned SQL: %w", err)
		}
		report.EditReconciliation = &reconciliation
		if reconciliation.Outcome == "conflict" {
			preserve := append([]string(nil), reconciliation.Preserved...)
			for _, conflict := range reconciliation.Conflicts {
				preserve = append(preserve, conflict.Path)
			}
			if err := ensureInputsUnchanged(ctx, input, lock, chain, selected, plan.DesiredFingerprint); err != nil {
				return historyChangedReport(report, err), nil
			}
			if err := bundle.Write(destination, artifact, bundle.WriteOptions{ReplaceDraft: true, PreserveEdited: preserve, ExpectedPrevious: &selected.Artifact}); err != nil {
				return report, fmt.Errorf("write conflict handoff: %w", err)
			}
			relative, pathErr := filepath.Rel(input.Root, destination)
			if pathErr != nil {
				return report, pathErr
			}
			report.Path = filepath.ToSlash(relative)
			report.Generation = selected.Artifact.Manifest.Generation
			report.Outcome = "blocked"
			report.Findings = append(report.Findings, Finding{
				Code:        "edited_phase_conflict",
				Message:     "the agent and the regenerated structural plan both changed the same phase; the new plan receipts are installed while the current SQL is preserved unreceipted",
				Remediation: "merge the reported current_sql and new_generated_sql into each conflict path, then run onwardpg verify --target " + input.TargetName + " --bundle " + input.BundleID,
			})
			return report, nil
		}
		artifact = reconciled
		report.Outcome = artifact.Manifest.State
		if artifact.Manifest.State == string(protocol.Planned) {
			report.EditFiles = nil
			report.EditRequirements = nil
		}
	}
	if selected != nil && artifact.Manifest.Generation == selected.Artifact.Manifest.Generation {
		artifact, err = bundle.WithDecisionHistory(selected.Artifact, artifact)
		if err != nil {
			return report, fmt.Errorf("prepare retained decision history: %w", err)
		}
	}

	if artifact.Manifest.State == string(protocol.Planned) {
		proposed := chain
		proposed.Entries = append(append([]history.Entry(nil), chain.Entries...), history.Entry{
			Directory: input.BundleID,
			Artifact:  artifact,
		})
		proposed.HeadDigest = artifact.Manifest.History.EntryDigest
		expandVerification, err := verify.Run(ctx, verify.Input{
			AdminURL: input.AdminURL, Chain: proposed, BundleID: input.BundleID,
			ThroughPhase: protocol.PhaseExpand, Ignores: input.Ignores, Options: input.PlannerOptions,
		})
		if err != nil {
			return report, fmt.Errorf("verify generated expand checkpoint: %w", err)
		}
		if expandVerification.Outcome != "partial_verified" && expandVerification.Outcome != "verified" {
			report.Outcome = "blocked"
			report.Verification = &expandVerification
			report.Findings = append(report.Findings, Finding{
				Code: "generated_expand_checkpoint_failed", Message: "generated expand did not produce a valid disposable PostgreSQL checkpoint",
				Remediation: "inspect the expand verification failure; no bundle was written",
			})
			return report, nil
		}
		artifact, err = bundle.WithExpandCheckpoint(artifact, expandVerification.ObservedFingerprint)
		if err != nil {
			return report, fmt.Errorf("receipt generated expand checkpoint: %w", err)
		}
		proposed.Entries[len(proposed.Entries)-1].Artifact = artifact
		proposed.HeadDigest = artifact.Manifest.History.EntryDigest
		verification, err := verify.Run(ctx, verify.Input{
			AdminURL: input.AdminURL, Chain: proposed, BundleID: input.BundleID,
			ThroughPhase: "contract", Ignores: input.Ignores, Options: input.PlannerOptions,
		})
		if err != nil {
			return report, fmt.Errorf("verify generated draft: %w", err)
		}
		report.Verification = &verification
		if verification.Outcome != "verified" {
			report.Outcome = "blocked"
			report.Findings = append(report.Findings, Finding{
				Code:        "generated_draft_did_not_converge",
				Message:     "generated phases did not converge to the desired schema in disposable PostgreSQL",
				Remediation: "inspect the verification residual; no bundle was written",
			})
			return report, nil
		}
	}

	if err := ensureInputsUnchanged(ctx, input, lock, chain, selected, plan.DesiredFingerprint); err != nil {
		return historyChangedReport(report, err), nil
	}
	writeOptions := bundle.WriteOptions{ReplaceDraft: selected != nil}
	if selected != nil {
		writeOptions.ExpectedPrevious = &selected.Artifact
	}
	if err := bundle.Write(destination, artifact, writeOptions); err != nil {
		return report, fmt.Errorf("write draft bundle: %w", err)
	}
	relative, err := filepath.Rel(input.Root, destination)
	if err != nil {
		return report, err
	}
	report.Path = filepath.ToSlash(relative)
	report.Generation = generation
	if selected == nil {
		report.CreatedBundle = true
	}
	if report.Outcome == string(protocol.NeedsInput) || report.Outcome == string(protocol.NeedsSQLEdits) {
		report.WrittenReceipts = bundle.SortedFiles(artifact.Files)
	}
	return report, nil
}

func pendingEditRequirements(plan protocol.Result) []EditRequirement {
	gateByID := make(map[string]protocol.ContractGate, len(plan.ContractGates))
	for _, gate := range plan.ContractGates {
		gateByID[gate.ID] = gate
	}
	var result []EditRequirement
	for _, statement := range plan.Statements {
		if !strings.Contains(statement.SQL, "ONWARDPG TODO") {
			continue
		}
		requirement := EditRequirement{
			Path: filepath.ToSlash(filepath.Join("phases", statement.Phase+".sql")), PocketID: statement.ID,
			Purpose: "replace operator-owned SQL placeholder", Phase: statement.Phase,
			ExecutionMode: protocol.ManualTransactionalOnce,
			RequiredProof: "replace every TODO in this pocket; disposable verification must execute the edited plan to the desired catalog",
		}
		if statement.Manual != nil {
			requirement.Purpose = statement.Manual.Summary
			requirement.ExecutionMode = statement.Manual.ExecutionMode
		}
		for gateID, gate := range gateByID {
			if strings.Contains(statement.SQL, gateID) && strings.Contains(gate.BooleanSQL, "ONWARDPG TODO") {
				requirement.Purpose = "define the exact contract invariant: " + gate.Reason
				requirement.ExecutionMode = "read_only_boolean"
				requirement.RequiredProof = "one resolved read-only SELECT returning exactly one Boolean row; verify.sql cannot satisfy this contract gate"
			}
		}
		result = append(result, requirement)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].PocketID < result[j].PocketID
	})
	return result
}

func acceptedStepsSince(chain history.Chain, previousParent string) []AcceptedStep {
	found := previousParent == chain.RootDigest
	seen := make(map[string]bool)
	var steps []AcceptedStep
	for _, entry := range chain.Entries {
		if !found {
			if entry.Artifact.Manifest.History != nil && entry.Artifact.Manifest.History.EntryDigest == previousParent {
				found = true
			}
			continue
		}
		manifest := entry.Artifact.Manifest
		for _, phase := range orderedPhaseNames(manifest.Phases) {
			receipt := manifest.Phases[phase]
			step := classifyAcceptedPhase(entry.Directory, phase, receipt.Path, manifest.PhaseSource)
			key := step.Path + "/" + step.Kind
			if !seen[key] {
				seen[key] = true
				steps = append(steps, step)
			}
		}
		if manifest.VerificationDigest != "" {
			step := AcceptedStep{
				BundleID:       entry.Directory,
				Path:           entry.Directory + "/verify.sql",
				Kind:           "verification_assertions",
				Reason:         "assertions_are_not_evidence_that_historical_data_work_ran_in_development",
				RequiresReview: true,
			}
			key := step.Path + "/" + step.Kind
			if !seen[key] {
				seen[key] = true
				steps = append(steps, step)
			}
		}
	}
	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Path != steps[j].Path {
			return steps[i].Path < steps[j].Path
		}
		return steps[i].Kind < steps[j].Kind
	})
	return steps
}

func orderedPhaseNames(phases map[string]bundle.PhaseArtifact) []string {
	names := make([]string, 0, len(phases))
	for phase := range phases {
		names = append(names, phase)
	}
	rank := map[string]int{protocol.PhaseExpand: 0, protocol.PhaseContract: 1}
	sort.Slice(names, func(i, j int) bool {
		left, leftKnown := rank[names[i]]
		right, rightKnown := rank[names[j]]
		if leftKnown && rightKnown {
			return left < right
		}
		if leftKnown != rightKnown {
			return leftKnown
		}
		return names[i] < names[j]
	})
	return names
}

func classifyAcceptedPhase(bundleID, phase, path, phaseSource string) AcceptedStep {
	step := AcceptedStep{BundleID: bundleID, Path: bundleID + "/" + path}
	switch {
	case phaseSource == "edited":
		step.Kind = "agent_authored_phase"
		step.Reason = "agent_authored_sql_may_include_data_or_operational_work_not_provable_from_catalog"
		step.RequiresReview = true
	case phase == protocol.PhaseContract:
		step.Kind = "generated_contract_phase"
		step.Reason = "contract_may_include_post_drain_validation_cleanup_or_data_effects_not_provable_from_catalog"
		step.RequiresReview = true
	default:
		step.Kind = "generated_structural_phase"
		step.Reason = "structural_destination_is_accounted_for_by_development_catalog_reconciliation"
	}
	return step
}

func developmentPostconditionsSince(chain history.Chain, previousParent string) ([]DevelopmentPostcondition, error) {
	found := previousParent == chain.RootDigest
	var checks []DevelopmentPostcondition
	for _, entry := range chain.Entries {
		if !found {
			if entry.Artifact.Manifest.History != nil && entry.Artifact.Manifest.History.EntryDigest == previousParent {
				found = true
			}
			continue
		}
		manifest := entry.Artifact.Manifest
		if manifest.VerificationDigest == "" {
			continue
		}
		assertions, err := bundle.ParseAssertions(entry.Artifact.Files["verify.sql"])
		if err != nil {
			return nil, fmt.Errorf("parse %s/verify.sql: %w", entry.Directory, err)
		}
		for _, assertion := range assertions {
			if !assertion.DevSafePostcondition {
				continue
			}
			checks = append(checks, DevelopmentPostcondition{
				BundleID: entry.Directory, Path: entry.Directory + "/verify.sql", ID: assertion.ID, SQL: assertion.SQL,
			})
		}
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].BundleID != checks[j].BundleID {
			return checks[i].BundleID < checks[j].BundleID
		}
		return checks[i].ID < checks[j].ID
	})
	return checks, nil
}

func ensureInputsUnchanged(ctx context.Context, input Input, lock *targetlock.Lock, expected history.Chain, selected *history.Entry, desiredFingerprint string) error {
	if err := lock.ValidatePath(); err != nil {
		return err
	}
	if err := workspace.RequireUnchanged(input.ConfigPath, input.Config); err != nil {
		return fmt.Errorf("reload configuration before lifecycle write: %w", err)
	}
	chain, current, err := history.LoadExcluding(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
	if err != nil {
		chain, current, err = history.LoadEditedDraft(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
	}
	if err != nil {
		return fmt.Errorf("target history no longer has one replayable base: %w", err)
	}
	if chain.HeadDigest != expected.HeadDigest {
		return fmt.Errorf("base head changed from %s to %s while the draft was being prepared", expected.HeadDigest, chain.HeadDigest)
	}
	if (selected == nil) != (current == nil) {
		return fmt.Errorf("selected bundle presence changed while the draft was being prepared")
	}
	if selected != nil && !reflect.DeepEqual(selected.Artifact, current.Artifact) {
		return fmt.Errorf("selected bundle changed while the draft was being prepared")
	}
	compiled, err := workspace.CompileDDL(ctx, input.Root, input.TargetName, input.Target)
	if err != nil {
		return fmt.Errorf("recompile desired schema before lifecycle write: %w", err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, input.AdminURL, input.Ignores)
	if err != nil {
		return fmt.Errorf("rematerialize desired schema before lifecycle write: %w", err)
	}
	currentFingerprint, err := graphplan.Fingerprint(desired, input.PlannerOptions)
	if err != nil {
		return fmt.Errorf("fingerprint desired schema before lifecycle write: %w", err)
	}
	if currentFingerprint != desiredFingerprint {
		return fmt.Errorf("desired schema changed from %s to %s while the draft was being prepared", desiredFingerprint, currentFingerprint)
	}
	return nil
}

func historyChangedReport(report Report, err error) Report {
	report.Outcome = "blocked"
	report.Findings = append(report.Findings, Finding{
		Code:        "inputs_changed_during_draft",
		Message:     err.Error(),
		Remediation: "inspect history status and the current exported DDL, then rerun draft; no lifecycle write was performed",
	})
	return report
}

func buildPlan(current, desired *pgschema.Snapshot, input Input, selected *history.Entry) (protocol.Result, *protocol.RebindReport, *protocol.Answers, []protocol.Question, []protocol.Hint, error) {
	if input.HintsGiven {
		base := input
		base.Hints, base.HintsGiven = nil, false
		plan, rebind, receipt, questions, previousHints, err := buildPlan(current, desired, base, selected)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		return buildWithSemanticHints(current, desired, plan, rebind, receipt, questions, previousHints, input.Hints, input.PlannerOptions)
	}

	var previousHints []protocol.Hint
	if selected != nil {
		var err error
		previousHints, err = bundle.SemanticHints(selected.Artifact)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
	}
	if selected == nil {
		plan, err := graphplan.Build(current, desired, protocol.Answers{}, input.PlannerOptions)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		questions := append([]protocol.Question(nil), plan.Questions...)
		if selected != nil {
			previousQuestions, err := bundle.DecisionQuestions(selected.Artifact)
			if err != nil {
				return protocol.Result{}, nil, nil, nil, nil, err
			}
			for _, question := range previousQuestions {
				if question.CurrentFingerprint == plan.CurrentFingerprint && question.DesiredFingerprint == plan.DesiredFingerprint {
					questions = mergeQuestions(questions, []protocol.Question{question})
				}
			}
		}
		return plan, nil, nil, questions, receiptHints(previousHints, questions, nil, current, desired), nil
	}
	plannerOptions := input.PlannerOptions
	if _, err := semantichint.ApplyIdentityHints(current, desired, previousHints, &plannerOptions); err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}

	data, hasAnswers := selected.Artifact.Files["answers.json"]
	if !hasAnswers {
		plan, err := graphplan.Build(current, desired, protocol.Answers{}, plannerOptions)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		return plan, nil, nil, plan.Questions, nil, nil
	}
	var previous protocol.Answers
	if err := json.Unmarshal(data, &previous); err != nil {
		return protocol.Result{}, nil, nil, nil, nil, fmt.Errorf("decode bundled answers: %w", err)
	}
	previousQuestions, err := bundle.DecisionQuestions(selected.Artifact)
	if err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}
	plan, rebind, receipt, questions, err := buildWithReboundAnswers(current, desired, previous, previousQuestions, plannerOptions)
	if err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}
	return plan, rebind, receipt, questions, receiptHints(previousHints, questions, receipt, current, desired), nil
}

func buildWithSemanticHints(current, desired *pgschema.Snapshot, plan protocol.Result, rebind *protocol.RebindReport, receipt *protocol.Answers, questions []protocol.Question, previous, supplied []protocol.Hint, options graphplan.Options) (protocol.Result, *protocol.RebindReport, *protocol.Answers, []protocol.Question, []protocol.Hint, error) {
	if err := protocol.ValidateHints(previous); err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}
	if err := protocol.ValidateHints(supplied); err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}
	allHints := append([]protocol.Hint(nil), previous...)
	known := make(map[string]bool, len(previous))
	for _, hint := range previous {
		key, _ := hint.CanonicalKey()
		known[key] = true
	}
	for _, hint := range supplied {
		key, _ := hint.CanonicalKey()
		if known[key] {
			continue
		}
		known[key] = true
		allHints = append(allHints, hint)
	}
	identityUsed, err := semantichint.ApplyIdentityHints(current, desired, allHints, &options)
	if err != nil {
		return protocol.Result{}, nil, nil, nil, nil, err
	}
	if len(identityUsed) > 0 {
		// Identity changes candidacy itself, so rebuild the initial question set
		// before trying to bind ordinary semantic answers.
		plan, err = graphplan.Build(current, desired, protocol.Answers{}, options)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		rebind, receipt, questions = nil, nil, append([]protocol.Question(nil), plan.Questions...)
	}
	used := make(map[int]bool, len(allHints))
	for index := range previous {
		used[index] = true
	}
	for index := range identityUsed {
		used[index] = true
	}
	answers := protocol.Answers{
		CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint,
	}
	if receipt != nil {
		answers = *receipt
		answers.Answers = append([]protocol.Answer(nil), receipt.Answers...)
	}
	for iteration := 0; iteration <= len(allHints)*2+1; iteration++ {
		if plan.Status != protocol.NeedsInput {
			if err := rejectUnusedSuppliedHints(allHints, used, len(previous)); err != nil {
				return protocol.Result{}, nil, nil, nil, nil, err
			}
			refreshRebindAfterHints(rebind, answers, plan)
			return plan, rebind, answersReceipt(answers), questions, usedHints(allHints, used), nil
		}
		matched, err := semantichint.MatchQuestions(plan.Questions, allHints, current, desired)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		if len(matched.Answers) == 0 {
			var unused []protocol.Hint
			for index := len(previous); index < len(allHints); index++ {
				if !used[index] {
					unused = append(unused, allHints[index])
				}
			}
			if _, err := semantichint.DeferredHints(current, desired, unused, options); err != nil {
				return protocol.Result{}, nil, nil, nil, nil, err
			}
			refreshRebindAfterHints(rebind, answers, plan)
			return plan, rebind, answersReceipt(answers), questions, usedHints(allHints, used), nil
		}
		for index := range matched.Used {
			used[index] = true
		}
		answers.Answers = append(answers.Answers, matched.Answers...)
		plan, err = graphplan.Build(current, desired, answers, options)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		questions = mergeQuestions(questions, plan.Questions)
	}
	return protocol.Result{}, nil, nil, nil, nil, fmt.Errorf("semantic hint planning did not converge")
}

func suppliedHintsNotReceipted(supplied, receipted []protocol.Hint) []protocol.Hint {
	used := make(map[string]bool, len(receipted))
	for _, hint := range receipted {
		key, _ := hint.CanonicalKey()
		used[key] = true
	}
	var result []protocol.Hint
	for _, hint := range supplied {
		key, _ := hint.CanonicalKey()
		if !used[key] {
			result = append(result, hint)
		}
	}
	result, _ = protocol.CanonicalHints(result)
	return result
}

func refreshRebindAfterHints(report *protocol.RebindReport, answers protocol.Answers, plan protocol.Result) {
	if report == nil {
		return
	}
	report.Answers = answers
	answered := make(map[string]bool, len(answers.Answers))
	for _, answer := range answers.Answers {
		answered[answer.Kind+":"+answer.Key] = true
	}
	remainingDeferred := report.Deferred[:0]
	for _, id := range report.Deferred {
		if !answered[id] {
			remainingDeferred = append(remainingDeferred, id)
		}
	}
	report.Deferred = remainingDeferred
	report.Unanswered = nil
	if plan.Status == protocol.NeedsInput {
		for _, question := range plan.Questions {
			report.Unanswered = append(report.Unanswered, question.Kind+":"+question.Key)
		}
		sort.Strings(report.Unanswered)
		return
	}
	invalidated := make(map[string]bool, len(report.Invalidated))
	for _, finding := range report.Invalidated {
		invalidated[finding.Decision] = true
	}
	for _, id := range report.Deferred {
		if !invalidated[id] {
			report.Invalidated = append(report.Invalidated, protocol.RebindFinding{Decision: id, Reason: "question_no_longer_present"})
		}
	}
	report.Deferred = nil
	sort.Slice(report.Invalidated, func(i, j int) bool {
		return report.Invalidated[i].Decision < report.Invalidated[j].Decision
	})
}

func rejectUnusedSuppliedHints(hints []protocol.Hint, used map[int]bool, suppliedAt int) error {
	var unused []string
	for index := suppliedAt; index < len(hints); index++ {
		if used[index] {
			continue
		}
		key, _ := hints[index].CanonicalKey()
		unused = append(unused, key)
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	return fmt.Errorf("unused semantic hints: %s", strings.Join(unused, ", "))
}

func usedHints(hints []protocol.Hint, used map[int]bool) []protocol.Hint {
	result := make([]protocol.Hint, 0, len(used))
	for index, hint := range hints {
		if used[index] {
			result = append(result, hint)
		}
	}
	result, _ = protocol.CanonicalHints(result)
	return result
}

func receiptHints(hints []protocol.Hint, questions []protocol.Question, receipt *protocol.Answers, current, desired *pgschema.Snapshot) []protocol.Hint {
	if len(hints) == 0 || receipt == nil {
		return nil
	}
	answers := make(map[string]protocol.Answer, len(receipt.Answers))
	for _, answer := range receipt.Answers {
		answers[answer.Kind+":"+answer.Key] = answer
	}
	var result []protocol.Hint
	for _, hint := range hints {
		matched, err := semantichint.MatchQuestions(questions, []protocol.Hint{hint}, current, desired)
		if err != nil || len(matched.Answers) == 0 {
			continue
		}
		valid := true
		for _, answer := range matched.Answers {
			stored, exists := answers[answer.Kind+":"+answer.Key]
			if !exists || stored.Value != answer.Value {
				valid = false
				break
			}
		}
		if valid {
			result = append(result, hint)
		}
	}
	result, _ = protocol.CanonicalHints(result)
	return result
}

func buildWithReboundAnswers(current, desired *pgschema.Snapshot, previous protocol.Answers, previousQuestions []protocol.Question, options graphplan.Options) (protocol.Result, *protocol.RebindReport, *protocol.Answers, []protocol.Question, error) {
	remaining := make(map[string]protocol.Answer, len(previous.Answers))
	for _, answer := range previous.Answers {
		id := answer.Kind + ":" + answer.Key
		if _, exists := remaining[id]; exists {
			return protocol.Result{}, nil, nil, nil, fmt.Errorf("duplicate previous answer %s", id)
		}
		remaining[id] = answer
	}
	report := protocol.RebindReport{}
	var carried protocol.Answers
	var questions []protocol.Question
	for iteration := 0; iteration <= len(previous.Answers); iteration++ {
		plan, err := graphplan.Build(current, desired, carried, options)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, err
		}
		carried.CurrentFingerprint = plan.CurrentFingerprint
		carried.DesiredFingerprint = plan.DesiredFingerprint
		questions = mergeQuestions(questions, plan.Questions)
		if plan.Status != protocol.NeedsInput {
			finalizeRebind(&report, carried, remaining, nil, true)
			report.Questions = questions
			return plan, &report, answersReceipt(carried), questions, nil
		}

		candidates := protocol.Answers{
			CurrentFingerprint: previous.CurrentFingerprint,
			DesiredFingerprint: previous.DesiredFingerprint,
		}
		for _, id := range sortedAnswerIDs(remaining) {
			candidates.Answers = append(candidates.Answers, remaining[id])
		}
		stage, err := protocol.RebindAnswers(candidates, previousQuestions, plan.Questions, plan.CurrentFingerprint, plan.DesiredFingerprint)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, err
		}
		for _, finding := range stage.Invalidated {
			if finding.Reason == "question_no_longer_present" {
				continue
			}
			report.Invalidated = append(report.Invalidated, finding)
			delete(remaining, finding.Decision)
		}
		if len(stage.Answers.Answers) == 0 {
			finalizeRebind(&report, carried, remaining, plan.Questions, false)
			report.Questions = questions
			return plan, &report, answersReceipt(carried), questions, nil
		}
		for _, answer := range stage.Answers.Answers {
			id := answer.Kind + ":" + answer.Key
			carried.Answers = append(carried.Answers, answer)
			report.Carried = append(report.Carried, id)
			delete(remaining, id)
		}
	}
	return protocol.Result{}, nil, nil, nil, fmt.Errorf("answer rebinding did not converge")
}

func answersReceipt(answers protocol.Answers) *protocol.Answers {
	if len(answers.Answers) == 0 {
		return nil
	}
	copy := answers
	return &copy
}

func finalizeRebind(report *protocol.RebindReport, carried protocol.Answers, remaining map[string]protocol.Answer, current []protocol.Question, final bool) {
	report.Answers = carried
	sort.Strings(report.Carried)
	if final {
		for _, id := range sortedAnswerIDs(remaining) {
			report.Invalidated = append(report.Invalidated, protocol.RebindFinding{Decision: id, Reason: "question_no_longer_present"})
		}
	} else {
		for _, question := range current {
			report.Unanswered = append(report.Unanswered, question.Kind+":"+question.Key)
		}
		report.Deferred = append(report.Deferred, sortedAnswerIDs(remaining)...)
		sort.Strings(report.Unanswered)
	}
	sort.Slice(report.Invalidated, func(i, j int) bool {
		return report.Invalidated[i].Decision < report.Invalidated[j].Decision
	})
}

func sortedAnswerIDs(answers map[string]protocol.Answer) []string {
	ids := make([]string, 0, len(answers))
	for id := range answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func mergeQuestions(first, second []protocol.Question) []protocol.Question {
	result := append([]protocol.Question(nil), first...)
	seen := make(map[string]bool, len(result))
	for _, question := range result {
		seen[question.Kind+":"+question.Key] = true
	}
	for _, question := range second {
		id := question.Kind + ":" + question.Key
		if !seen[id] {
			result = append(result, question)
			seen[id] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+":"+result[i].Key < result[j].Kind+":"+result[j].Key
	})
	return result
}

// ExistingArtifact reports whether a selected bundle currently exists. It is
// intentionally strict and is used only for diagnostics and tests.
func ExistingArtifact(root string, config workspace.Config, target, bundleID string) (bundle.Artifact, bool, error) {
	destination, err := config.BundlePath(root, target, bundleID)
	if err != nil {
		return bundle.Artifact{}, false, err
	}
	artifact, err := bundle.Read(destination)
	if os.IsNotExist(err) {
		return bundle.Artifact{}, false, nil
	}
	return artifact, err == nil, err
}
