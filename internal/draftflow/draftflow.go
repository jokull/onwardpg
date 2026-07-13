// Package draftflow plans one explicitly selected, Git-free feature bundle
// from the replayed onwardpg history head to the current declarative schema.
package draftflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/semantichint"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
	"github.com/jokull/onwardpg/pgschema"
)

const Version = "onwardpg/draft/2"

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	ProtocolVersion    string                     `json:"protocol_version"`
	Outcome            string                     `json:"outcome"`
	Target             string                     `json:"target"`
	BundleID           string                     `json:"bundle_id"`
	Path               string                     `json:"path,omitempty"`
	Generation         int                        `json:"generation,omitempty"`
	HistoryHead        string                     `json:"history_head,omitempty"`
	PreviousParent     string                     `json:"previous_parent,omitempty"`
	ParentChanged      bool                       `json:"parent_changed,omitempty"`
	CurrentFingerprint string                     `json:"current_fingerprint,omitempty"`
	DesiredFingerprint string                     `json:"desired_fingerprint,omitempty"`
	Plan               *protocol.Result           `json:"plan,omitempty"`
	Decisions          []protocol.Decision        `json:"decisions,omitempty"`
	EditFiles          []string                   `json:"edit_files,omitempty"`
	AnswerRebind       *protocol.RebindReport     `json:"answer_rebind,omitempty"`
	EditReconciliation *bundle.EditReconciliation `json:"edit_reconciliation,omitempty"`
	Verification       *verify.Report             `json:"verification,omitempty"`
	Findings           []Finding                  `json:"findings,omitempty"`
}

type Input struct {
	Root           string
	Config         workspace.Config
	TargetName     string
	Target         workspace.Target
	AdminURL       string
	BundleID       string
	BuildVersion   string
	Purpose        string
	Hints          []protocol.Hint
	HintsGiven     bool
	Ignores        []string
	PlannerOptions graphplan.Options
}

// Run replaces one explicitly selected draft while treating every other entry
// as base history. Verification receipts do not freeze the selected draft: it
// remains mutable for the life of the feature. All SQL execution happens only
// in disposable databases created through AdminURL.
func Run(ctx context.Context, input Input) (Report, error) {
	report := Report{
		ProtocolVersion: Version,
		Outcome:         "error",
		Target:          input.TargetName,
		BundleID:        input.BundleID,
	}
	if input.Root == "" || !filepath.IsAbs(input.Root) {
		return report, fmt.Errorf("draft root must be absolute")
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
	postgresMajor, err := source.PostgresMajor(ctx, input.AdminURL)
	if err != nil {
		return report, err
	}

	destination, err := input.Config.BundlePath(input.Root, input.TargetName, input.BundleID)
	if err != nil {
		return report, err
	}
	chain, selected, err := history.LoadExcluding(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
	if err != nil {
		strictErr := err
		chain, selected, err = history.LoadEditedDraft(input.Root, input.Config.BundleRoot, input.TargetName, input.BundleID)
		if err != nil {
			return report, fmt.Errorf("load history beneath selected bundle: %v; prepare selected edited draft: %w", strictErr, err)
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
	for _, entry := range chain.Entries {
		recorded := entry.Artifact.Manifest.DesiredSource.PostgresMajor
		if recorded != 0 && recorded != postgresMajor {
			return report, fmt.Errorf("history bundle %s targets PostgreSQL %d but the scratch server is PostgreSQL %d", entry.Directory, recorded, postgresMajor)
		}
	}
	report.HistoryHead = chain.HeadDigest
	if selected != nil {
		report.PreviousParent = selected.Artifact.Manifest.History.ParentDigest
		report.ParentChanged = report.PreviousParent != chain.HeadDigest
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
	if err := source.ValidateIgnoreSelectors(input.Ignores, current, desired); err != nil {
		return report, err
	}

	plan, rebind, answerReceipt, questions, hints, err := buildPlan(current, desired, input, selected)
	if err != nil {
		return report, err
	}
	if plan.Status == protocol.NeedsInput {
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

	metadata := bundle.Metadata{
		BundleID: input.BundleID,
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
			Options: bundle.PlannerOptions{
				ConcurrentIndexes: input.PlannerOptions.ConcurrentIndexes,
				IfNotExists:       input.PlannerOptions.IfNotExists,
				IfExists:          input.PlannerOptions.IfExists,
				CascadeDrops:      input.PlannerOptions.CascadeDrops,
				SchemaQualifier:   input.PlannerOptions.SchemaQualifier,
			},
			IgnoreSelectors: append([]string(nil), input.Ignores...),
		},
		HistoryParentDigest: chain.HeadDigest,
	}
	generation, attempt, err := bundle.NextCoordinates(destination, metadata, plan)
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
			if err := bundle.Write(destination, artifact, bundle.WriteOptions{ReplaceDraft: true, PreserveEdited: preserve}); err != nil {
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
		}
	}

	if artifact.Manifest.State == string(protocol.Planned) {
		proposed := chain
		proposed.Entries = append(append([]history.Entry(nil), chain.Entries...), history.Entry{
			Directory: input.BundleID,
			Artifact:  artifact,
		})
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

	if err := bundle.Write(destination, artifact, bundle.WriteOptions{ReplaceDraft: selected != nil}); err != nil {
		return report, fmt.Errorf("write draft bundle: %w", err)
	}
	relative, err := filepath.Rel(input.Root, destination)
	if err != nil {
		return report, err
	}
	report.Path = filepath.ToSlash(relative)
	report.Generation = generation
	return report, nil
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

	data, hasAnswers := selected.Artifact.Files["answers.json"]
	if !hasAnswers {
		plan, err := graphplan.Build(current, desired, protocol.Answers{}, input.PlannerOptions)
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
	plan, rebind, receipt, questions, err := buildWithReboundAnswers(current, desired, previous, previousQuestions, input.PlannerOptions)
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
	used := make(map[int]bool, len(allHints))
	for index := range previous {
		used[index] = true
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint,
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
			return plan, rebind, answersReceipt(answers), questions, usedHints(allHints, used), nil
		}
		matched, err := semantichint.MatchQuestions(plan.Questions, allHints, current, desired)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, nil, err
		}
		if len(matched.Answers) == 0 {
			if err := rejectUnusedSuppliedHints(allHints, used, len(previous)); err != nil {
				return protocol.Result{}, nil, nil, nil, nil, err
			}
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
	report := protocol.RebindReport{ProtocolVersion: protocol.RebindVersion}
	var carried protocol.Answers
	var questions []protocol.Question
	for iteration := 0; iteration <= len(previous.Answers); iteration++ {
		plan, err := graphplan.Build(current, desired, carried, options)
		if err != nil {
			return protocol.Result{}, nil, nil, nil, err
		}
		carried.ProtocolVersion = protocol.Version
		carried.CurrentFingerprint = plan.CurrentFingerprint
		carried.DesiredFingerprint = plan.DesiredFingerprint
		questions = mergeQuestions(questions, plan.Questions)
		if plan.Status != protocol.NeedsInput {
			finalizeRebind(&report, carried, remaining, nil, true)
			report.Questions = questions
			return plan, &report, answersReceipt(carried), questions, nil
		}

		candidates := protocol.Answers{
			ProtocolVersion:    previous.ProtocolVersion,
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
