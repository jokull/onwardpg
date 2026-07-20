package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/devflow"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/workspace"
)

func TestReadHintsAcceptsPredictableInlineIntent(t *testing.T) {
	hints, err := readHints([]string{`{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}`}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0].Kind != "rename" || hints[0].To[2] != "display_name" {
		t.Fatalf("hints = %#v", hints)
	}
}

func TestReadHintsAcceptsAheadOfTimeDecisionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.hints.json")
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), `[
		{"kind":"rollout","name":["public","events","occurred_on"],"strategy":"staged_with_backfill"},
		{"kind":"manual_sql","object":"column","name":["public","events","occurred_on"],"action":"backfill_not_null"}
	]`)
	hints, err := readHints(nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 2 || hints[0].Kind != "rollout" || hints[1].Action != "backfill_not_null" {
		t.Fatalf("hints = %#v", hints)
	}
}

func TestWriteDraftDecisionJSONContainsOnlyIrreducibleExchange(t *testing.T) {
	report := draftflow.Report{
		ProtocolVersion:    draftflow.Version,
		Outcome:            string(protocol.NeedsInput),
		Target:             "primary",
		BundleID:           "customer-profile",
		CurrentFingerprint: "sha256:" + strings.Repeat("a", 64),
		DesiredFingerprint: "sha256:" + strings.Repeat("b", 64),
		Decisions: []protocol.Decision{{
			Choices: []protocol.DecisionChoice{{
				Hint:    protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "legacy"}},
				Hazards: []string{"data_loss"},
			}},
		}},
	}
	var output bytes.Buffer
	if err := writeDraftReport(&output, report, "json"); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 4 || string(document["protocol_version"]) != `"onwardpg.draft/v5"` || string(document["status"]) != `"needs_decisions"` || string(document["next_action"]) != `"rerun_same_command_with_hints"` {
		t.Fatalf("document = %s", output.String())
	}
	var decisions []protocol.Decision
	if err := json.Unmarshal(document["decisions"], &decisions); err != nil || len(decisions) != 1 || len(decisions[0].Choices) != 1 || len(decisions[0].Choices[0].Argv) != 4 || !reflect.DeepEqual(decisions[0].Choices[0].Argv[:3], []string{"onwardpg", "draft", "--hint"}) {
		t.Fatalf("decision argv = %#v, err=%v", decisions, err)
	}
	if _, exists := document["target"]; exists {
		t.Fatalf("decision exchange echoed inferable target: %s", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte("fingerprint")) || bytes.Contains(output.Bytes(), []byte("customer-profile")) {
		t.Fatalf("decision exchange leaked receipt-only fields: %s", output.String())
	}
}

func TestWriteDecisionEnvelopeIncludesPlannerAnalysis(t *testing.T) {
	var output bytes.Buffer
	analysis := []protocol.DecisionAnalysis{{
		Kind: "rename_table", From: "table:public.accounts", To: "table:public.customers",
		Outcome: "rejected", Reason: "child_identity_mismatch:constraint:public.accounts_pkey",
	}}
	guidance := []protocol.Guidance{{Kind: "partition_reconfiguration", Key: "table:app:events", Summary: "Build a shadow hierarchy."}}
	if err := writeDecisionEnvelope(&output, "onwardpg.plan/v4", []string{"onwardpg", "diff"}, "--hint", nil, analysis, guidance); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Analysis []protocol.DecisionAnalysis `json:"analysis"`
		Guidance []protocol.Guidance         `json:"guidance"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(document.Analysis, analysis) {
		t.Fatalf("analysis = %#v, want %#v", document.Analysis, analysis)
	}
	if !reflect.DeepEqual(document.Guidance, guidance) {
		t.Fatalf("guidance = %#v, want %#v", document.Guidance, guidance)
	}
}

func TestWriteUnsupportedTextIncludesNonExecutableGuidance(t *testing.T) {
	result := protocol.Result{
		Status:      protocol.Unsupported,
		Unsupported: []string{"two_application_deployments_required:column:app:orders:amount"},
		Guidance: []protocol.Guidance{{
			Kind: "split_plan", Key: "column:app:orders:amount", Summary: "Retain the old contract in Plan A.",
			Steps: []protocol.GuidanceStep{{Stage: "plan_a_expand_scaffold", SQL: `ALTER TABLE "app"."orders" ADD COLUMN "onwardpg_next_1234" bigint;`}},
		}},
	}
	var output bytes.Buffer
	if err := writeUnsupportedText(&output, result); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"unsupported", result.Unsupported[0], "split_plan guidance", "Retain the old contract", "plan_a_expand_scaffold", "ADD COLUMN"} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("unsupported guidance omitted %q:\n%s", fragment, output.String())
		}
	}
}

func TestWriteDecisionsTextProducesCopyableHints(t *testing.T) {
	var output bytes.Buffer
	err := writeDecisionsText(&output, "dev plan", []protocol.Decision{{Choices: []protocol.DecisionChoice{
		{Hint: protocol.Hint{Kind: "type_change", Name: []string{"public", "events", "occurred_on"}, Strategy: "manual_sql"}, Hazards: []string{"manual_sql"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "dev plan needs 1 decision(s)") ||
		!strings.Contains(output.String(), `--hint '{"kind":"type_change","name":["public","events","occurred_on"],"strategy":"manual_sql"}'`) ||
		!strings.Contains(output.String(), "Decision 1: choose exactly one") ||
		!strings.Contains(output.String(), "hazards: manual_sql") {
		t.Fatalf("text decisions = %s", output.String())
	}
}

func TestWriteDecisionsTextWithFlagScopesDevelopmentHints(t *testing.T) {
	var output bytes.Buffer
	err := writeDecisionsTextWithFlag(&output, "development workspace", "--dev-hint", []protocol.Decision{{Choices: []protocol.DecisionChoice{
		{Hint: protocol.Hint{Kind: "rename", Object: "table", From: []string{"app", "accounts"}, To: []string{"app", "customers"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "development workspace needs 1 decision(s)") ||
		!strings.Contains(output.String(), `--dev-hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}'`) ||
		strings.Contains(output.String(), "    --hint ") {
		t.Fatalf("scoped decision text = %s", output.String())
	}
}

func TestWorkflowPlanReportSeparatesDurableSuccessAndExecutableDevelopmentChoice(t *testing.T) {
	hint := protocol.Hint{Kind: "rename", Object: "table", From: []string{"app", "accounts"}, To: []string{"app", "customers"}}
	durable := draftflow.Report{Outcome: string(protocol.Planned), BundleID: "rename-accounts", Path: "onward-bundles/primary/rename-accounts"}
	development := devflow.Report{
		Status: protocol.NeedsInput,
		Decisions: []protocol.Decision{{
			Choices: []protocol.DecisionChoice{{Hint: hint, Hazards: []string{"table_lock"}}},
		}},
	}
	report := newWorkflowPlanReport(durable, development)
	if report.ProtocolVersion != "onwardpg.plan/v5" || report.Status != "needs_action" || report.Durable.Outcome != string(protocol.Planned) {
		t.Fatalf("workflow report = %#v", report)
	}
	if len(report.NextActions) != 1 || report.NextActions[0].Scope != "development" || len(report.NextActions[0].Choices) != 1 {
		t.Fatalf("next actions = %#v", report.NextActions)
	}
	choice := report.NextActions[0].Choices[0]
	if !reflect.DeepEqual(choice.Hint, hint) || !reflect.DeepEqual(choice.Argv[:3], []string{"onwardpg", "plan", "--dev-hint"}) {
		t.Fatalf("choice = %#v", choice)
	}
	var decoded protocol.Hint
	if err := json.Unmarshal([]byte(choice.Argv[3]), &decoded); err != nil || !reflect.DeepEqual(decoded, hint) {
		t.Fatalf("argv hint = %#v, err=%v", decoded, err)
	}
}

func TestWorkflowPlanDevelopmentChoiceArgvCarriesAppliedHints(t *testing.T) {
	carried := protocol.Hint{Kind: "preserve", Object: "column", Name: []string{"app", "accounts", "branch_note"}}
	choice := protocol.Hint{Kind: "rename", Object: "column", From: []string{"app", "accounts", "display_name"}, To: []string{"app", "accounts", "full_name"}}
	report := newWorkflowPlanReport(
		draftflow.Report{Outcome: string(protocol.Planned)},
		devflow.Report{Status: protocol.NeedsInput, AppliedHints: []protocol.Hint{carried}, Decisions: []protocol.Decision{{Choices: []protocol.DecisionChoice{{Hint: choice}}}}},
	)
	if len(report.NextActions) != 1 || len(report.NextActions[0].Choices) != 1 {
		t.Fatalf("next actions = %#v", report.NextActions)
	}
	argv := report.NextActions[0].Choices[0].Argv
	if len(argv) != 6 || !reflect.DeepEqual(argv[:3], []string{"onwardpg", "plan", "--dev-hint"}) || argv[4] != "--dev-hint" {
		t.Fatalf("cumulative argv = %#v", argv)
	}
	var first, second protocol.Hint
	if err := json.Unmarshal([]byte(argv[3]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(argv[5]), &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, carried) || !reflect.DeepEqual(second, choice) {
		t.Fatalf("cumulative hints = %#v, %#v", first, second)
	}
}

func TestWorkflowPlanLabelsPreEditPlannerResultAsGeneratedPlan(t *testing.T) {
	generated := protocol.Result{Status: protocol.NeedsSQLEdits}
	durable := draftflow.Report{Outcome: string(protocol.Planned), Plan: &generated}
	development := devflow.Report{Status: protocol.Status("not_available")}
	var output bytes.Buffer
	if err := writeWorkflowPlanReport(&output, "json", durable, development); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"generated_plan":{"status":"needs_sql_edits"`) || strings.Contains(output.String(), `"plan":{"status"`) {
		t.Fatalf("ambiguous generated plan JSON = %s", output.String())
	}
}

func TestWorkflowPlanReportExposesRebaseWorkspaceFastForward(t *testing.T) {
	durable := draftflow.Report{Outcome: string(protocol.Planned), BundleID: "booking-dates", ParentChanged: true}
	devHint := protocol.Hint{Kind: "preserve", Object: "column", Name: []string{"app", "accounts", "parallel_branch_note"}}
	development := devflow.Report{Status: protocol.Planned, Result: protocol.Result{
		Statements: []protocol.Statement{{SQL: `ALTER TABLE "app"."accounts" ADD COLUMN "timezone" text;`, Phase: protocol.PhaseExpand}},
		Preserved:  []string{"column:app:accounts:parallel_branch_note"},
	}, AppliedHints: []protocol.Hint{devHint}}
	report := newWorkflowPlanReport(durable, development)
	if len(report.NextActions) != 1 {
		t.Fatalf("next actions = %#v", report.NextActions)
	}
	action := report.NextActions[0]
	if action.Kind != "workspace_fast_forward" || action.Scope != "development" || action.Reason != "accepted_history_changed" || action.StatementCount != 1 {
		t.Fatalf("fast-forward action = %#v", action)
	}
	if len(action.Argv) != 6 || !reflect.DeepEqual(action.Argv[:3], []string{"onwardpg", "plan", "--dev-hint"}) || !reflect.DeepEqual(action.Argv[4:], []string{"--output", "sql"}) || !strings.Contains(action.SQL, `ADD COLUMN "timezone"`) {
		t.Fatalf("fast-forward handoff = %#v", action)
	}
	var replayed protocol.Hint
	if err := json.Unmarshal([]byte(action.Argv[3]), &replayed); err != nil || !reflect.DeepEqual(replayed, devHint) {
		t.Fatalf("fast-forward dev hint = %#v, err=%v", replayed, err)
	}
	if !reflect.DeepEqual(action.Preserved, development.Result.Preserved) {
		t.Fatalf("preserved parallel work = %#v", action.Preserved)
	}
}

func TestWorkspaceFastForwardReplayNamesPlanWithoutDurableBundle(t *testing.T) {
	development := devflow.Report{Status: protocol.Planned, Result: protocol.Result{
		Statements: []protocol.Statement{{SQL: `CREATE SCHEMA "app";`, Phase: protocol.PhaseExpand}},
	}}

	for _, durable := range []draftflow.Report{
		{Outcome: "no_changes", BundleID: "bootstrap"},
		{Outcome: "absorbed", BundleID: "booking-dates", Path: "onward-bundles/primary/booking-dates", RemovedBundle: true},
	} {
		report := newWorkflowPlanReport(durable, development)
		if len(report.NextActions) != 1 {
			t.Fatalf("next actions for %s = %#v", durable.Outcome, report.NextActions)
		}
		want := []string{"onwardpg", "plan", durable.BundleID, "--output", "sql"}
		if !reflect.DeepEqual(report.NextActions[0].Argv, want) {
			t.Fatalf("replay argv for %s = %#v, want %#v", durable.Outcome, report.NextActions[0].Argv, want)
		}
	}
}

func TestWorkflowSQLWarnsOnlyForUnprovableAcceptedWork(t *testing.T) {
	durable := draftflow.Report{
		Outcome: string(protocol.Planned),
		AcceptedSteps: []draftflow.AcceptedStep{
			{Path: "upstream/phases/expand.sql", Kind: "generated_structural_phase"},
			{Path: "upstream/phases/contract.sql", Kind: "agent_authored_phase", RequiresReview: true},
		},
	}
	development := devflow.Report{Status: protocol.Planned, Result: protocol.Result{
		Statements: []protocol.Statement{{SQL: `CREATE TABLE "app"."events" ();`, Phase: "expand", Safety: "safe"}},
	}}
	var output bytes.Buffer
	if err := writeWorkflowPlanReport(&output, "sql", durable, development); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "agent_authored_phase") {
		t.Fatalf("missing review warning: %s", text)
	}
	if strings.Contains(text, "generated_structural_phase") {
		t.Fatalf("structural phase should not be rendered as an unprovable warning: %s", text)
	}
}

func TestResolveConfiguredTargetDefaultsOnlyWhenUnambiguous(t *testing.T) {
	selected := ""
	target, err := resolveConfiguredTarget(workspace.Config{Targets: map[string]workspace.Target{"app": {SchemaFile: "schema.sql"}}}, &selected)
	if err != nil || selected != "app" || target.SchemaFile != "schema.sql" {
		t.Fatalf("single target resolution = %q %#v err=%v", selected, target, err)
	}
	selected = ""
	_, err = resolveConfiguredTarget(workspace.Config{Targets: map[string]workspace.Target{"accounts": {}, "events": {}}}, &selected)
	if err == nil || !strings.Contains(err.Error(), "accounts, events") {
		t.Fatalf("multi-target ambiguity = %v", err)
	}
}

func TestWorkflowPostconditionFailureNeedsDevelopmentReview(t *testing.T) {
	development := devflow.Report{Status: protocol.Planned, Postconditions: []devflow.PostconditionResult{{
		BundleID: "upstream", Path: "upstream/verify.sql", ID: "backfill", Status: "failed", Message: "assertion returned false",
	}}}
	if status := workflowPlanStatus(string(protocol.Planned), development); status != "needs_action" {
		t.Fatalf("status = %q", status)
	}
	if code := workflowPlanExitCode(string(protocol.Planned), development); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	var output bytes.Buffer
	if err := writeWorkflowPlanReport(&output, "sql", draftflow.Report{Outcome: string(protocol.Planned)}, development); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "postcondition failed") || !strings.Contains(output.String(), "assertion returned false") {
		t.Fatalf("SQL evidence = %s", output.String())
	}
}

func TestShellQuoteProtectsAgentCopyableHints(t *testing.T) {
	if quoted := shellQuote("it's"); quoted != `'it'"'"'s'` {
		t.Fatalf("single-quote escaping = %s", quoted)
	}
	if quoted := shellQuote("$(touch /tmp/nope) `$HOME`"); quoted != "'$(touch /tmp/nope) `$HOME`'" {
		t.Fatalf("expansion escaping = %s", quoted)
	}
}

func TestDevPlanRejectsUnknownOutputBeforeReadingConfiguration(t *testing.T) {
	output := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary", "--output", "yaml"}, t.TempDir())
	})
	if output.code != 1 || !strings.Contains(output.stdout, "dev plan --output must be text or json") {
		t.Fatalf("output = %#v", output)
	}
}

func TestVersionedDiagnosticContract(t *testing.T) {
	diagnostic := protocol.ErrorDiagnostic("invalid_invocation", errors.New("bad flags"))
	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), protocol.DiagnosticVersion) || !contains(string(data), `"code":"invalid_invocation"`) {
		t.Fatalf("diagnostic = %s", data)
	}
}

func TestHelpIsSuccessfulForEveryCommandSurface(t *testing.T) {
	tests := []struct {
		name string
		run  func() int
	}{
		{name: "init", run: func() int { return runInitAt([]string{"--help"}, t.TempDir()) }},
		{name: "status", run: func() int { return runStatusAt([]string{"--help"}, t.TempDir()) }},
		{name: "history-group", run: func() int { return runHistoryStatusAt([]string{"--help"}, t.TempDir()) }},
		{name: "history-status", run: func() int { return runHistoryStatusAt([]string{"status", "--help"}, t.TempDir()) }},
		{name: "dev-group", run: func() int { return runDevAt([]string{"--help"}, t.TempDir()) }},
		{name: "dev-plan", run: func() int { return runDevAt([]string{"plan", "--help"}, t.TempDir()) }},
		{name: "draft", run: func() int { return runDraftAt([]string{"--help"}, t.TempDir()) }},
		{name: "verify", run: func() int { return runVerifyAt([]string{"--help"}, t.TempDir()) }},
		{name: "drift-group", run: func() int { return runDriftAt([]string{"--help"}, t.TempDir()) }},
		{name: "drift-check", run: func() int { return runDriftAt([]string{"check", "--help"}, t.TempDir()) }},
		{name: "plan", run: func() int { return runPlan([]string{"--help"}) }},
		{name: "config-group", run: func() int { return runConfig([]string{"--help"}) }},
		{name: "config-check", run: func() int { return runConfig([]string{"check", "--help"}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := test.run(); code != 0 {
				t.Fatalf("help exit = %d", code)
			}
		})
	}
}

func TestRootHelpExplainsTheNoApplyBoundary(t *testing.T) {
	previous := os.Args
	defer func() { os.Args = previous }()
	os.Args = []string{"onwardpg", "--help"}
	output := captureStdout(t, run)
	if output.code != 0 || !strings.Contains(output.stdout, "history status") || !strings.Contains(output.stdout, "never applies them to caller databases") {
		t.Fatalf("root help = %#v", output)
	}
}

func TestPlanRequiresAnActivePlanOrInitialNameBeforeDatabaseAccess(t *testing.T) {
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_UNUSED_SCRATCH_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app;\n")
	output := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"--target", "primary"}, repository)
	})
	if output.code != 4 || !strings.Contains(output.stdout, `"code":"active_plan_required"`) {
		t.Fatalf("plan without active anchor = %d, %s", output.code, output.stdout)
	}
}

func TestVerifyUsesActivePlanOnlyWhenOneExists(t *testing.T) {
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_UNUSED_SCRATCH_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app;\n")
	output := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary"}, repository)
	})
	if output.code != 1 || !strings.Contains(output.stdout, `"code":"active_plan_required"`) {
		t.Fatalf("verify without active anchor = %d, %s", output.code, output.stdout)
	}
}

func TestStatusReportsNoActivePlanWithoutDatabaseAccess(t *testing.T) {
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_UNUSED_SCRATCH_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app;\n")
	output := captureStdout(t, func() int {
		return runStatusAt([]string{"--target", "primary"}, repository)
	})
	if output.code != 0 || !strings.Contains(output.stdout, `"status":"absent"`) {
		t.Fatalf("status without active anchor = %d, %s", output.code, output.stdout)
	}
}

func TestConfigCheckReportsMissingDevelopmentURLBeforeConnecting(t *testing.T) {
	repository := t.TempDir()
	t.Setenv("ONWARDPG_MISSING_DEV_URL", "")
	t.Setenv("ONWARDPG_MISSING_SCRATCH_URL", "")
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_MISSING_DEV_URL"
scratch_database_env = "ONWARDPG_MISSING_SCRATCH_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE public.accounts (id bigint);\n")
	output := captureStdout(t, func() int {
		return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
	})
	if output.code != 1 || !strings.Contains(output.stdout, `"code":"source_error"`) || !strings.Contains(output.stdout, "ONWARDPG_MISSING_DEV_URL") {
		t.Fatalf("config missing development URL = %#v", output)
	}
}

func TestVersionCommandReportsEmbeddedBuildVersion(t *testing.T) {
	previousArgs, previousVersion, previousCommit, previousTime := os.Args, buildVersion, buildCommit, buildTime
	defer func() {
		os.Args, buildVersion, buildCommit, buildTime = previousArgs, previousVersion, previousCommit, previousTime
	}()
	os.Args = []string{"onwardpg", "version"}
	buildVersion = "v0.1.0-preview.1"
	buildCommit = strings.Repeat("a", 40)
	buildTime = "2026-07-20T12:00:00Z"
	output := captureStdout(t, run)
	var document struct {
		ProtocolVersion string        `json:"protocol_version"`
		Status          string        `json:"status"`
		Build           buildIdentity `json:"build"`
	}
	if err := json.Unmarshal([]byte(output.stdout), &document); output.code != 0 || err != nil {
		t.Fatalf("version output = %#v", output)
	}
	if document.ProtocolVersion != "onwardpg.version/v1" || document.Status != "ok" || document.Build.Version != buildVersion || document.Build.Commit != buildCommit || document.Build.BuildTime != buildTime || document.Build.GoVersion == "" || !reflect.DeepEqual(document.Build.SupportedPostgresMajors, []int{15, 16, 17, 18}) {
		t.Fatalf("version document = %#v", document)
	}
}

func TestSelectBuildVersionUsesTaggedModuleForGoInstall(t *testing.T) {
	for _, test := range []struct {
		name          string
		linkerVersion string
		moduleVersion string
		want          string
	}{
		{name: "release archive linker version wins", linkerVersion: "v0.2.0", moduleVersion: "v0.1.0", want: "v0.2.0"},
		{name: "tagged module", linkerVersion: "dev", moduleVersion: "v0.1.0-preview.1", want: "v0.1.0-preview.1"},
		{name: "clean local pseudo version", linkerVersion: "dev", moduleVersion: "v0.0.0-20260715113614-b59714b741dd", want: "dev"},
		{name: "dirty local pseudo version", linkerVersion: "dev", moduleVersion: "v0.0.0-20260715113614-b59714b741dd+dirty", want: "dev"},
		{name: "local build", linkerVersion: "dev", moduleVersion: "(devel)", want: "dev"},
		{name: "missing build info", linkerVersion: "dev", moduleVersion: "", want: "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectBuildVersion(test.linkerVersion, test.moduleVersion); got != test.want {
				t.Fatalf("selectBuildVersion(%q, %q) = %q, want %q", test.linkerVersion, test.moduleVersion, got, test.want)
			}
		})
	}
}

func writeTestFile(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
