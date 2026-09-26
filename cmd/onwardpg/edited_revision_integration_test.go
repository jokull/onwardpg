package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
)

func TestEditedPlanRevisionCarriesSequentialHintsWithoutChangingSQLOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, display_name text NOT NULL);\n")
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}

	rename := protocol.Hint{Kind: "rename", Object: "column", From: []string{"app", "customers", "display_name"}, To: []string{"app", "customers", "full_name"}}
	initialHints := []protocol.Hint{
		rename,
		{Kind: "rename_backfill", Name: []string{"app", "customers", "display_name"}, Strategy: "manual_sql"},
		{Kind: "manual_sql", Object: "column", Name: []string{"app", "customers", "display_name"}, Action: "rename_backfill"},
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, full_name text NOT NULL, account_status text);\n")
	initialArgs := []string{"customer-profile", "--target", "primary"}
	for _, hint := range initialHints {
		encoded, err := json.Marshal(hint)
		if err != nil {
			t.Fatal(err)
		}
		initialArgs = append(initialArgs, "--hint", string(encoded))
	}
	initial := captureStdout(t, func() int { return runWorkflowPlanAt(initialArgs, repository) })
	var initialReport workflowPlanReport
	if err := json.Unmarshal([]byte(initial.stdout), &initialReport); err != nil {
		t.Fatal(err)
	}
	if initial.code != 2 || initialReport.Durable.Outcome != string(protocol.NeedsSQLEdits) {
		t.Fatalf("initial manual SQL handoff = %d, %s", initial.code, initial.stdout)
	}
	bundlePath := filepath.Join(repository, "onward-bundles", "primary", "customer-profile")
	artifact, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, bundlePath, "phases/expand.sql", replaceFirstEditPocket(t, string(artifact.Files["phases/expand.sql"]), "UPDATE app.customers SET full_name = display_name;"))
	verified := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-profile"}, repository)
	})
	if verified.code != 0 {
		t.Fatalf("verify edited bundle = %d, %s", verified.code, verified.stdout)
	}
	edited, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Manifest.PhaseSource != "edited" {
		t.Fatalf("edited bundle phase source = %q", edited.Manifest.PhaseSource)
	}
	oldExpand := string(edited.Files["phases/expand.sql"])
	oldContract := string(edited.Files["phases/contract.sql"])

	// A second same-type column changes the old rename question's scope. The
	// revised edited bundle waits for decisions without replacing owned SQL.
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, full_name text NOT NULL, account_status text NOT NULL);\n")
	call := func(args []string) workflowPlanReport {
		t.Helper()
		output := captureStdout(t, func() int { return runWorkflowPlanAt(args, repository) })
		var report workflowPlanReport
		if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
			t.Fatalf("decode plan = %d, %s: %v", output.code, output.stdout, err)
		}
		if output.code != 2 {
			t.Fatalf("plan = %d, %s", output.code, output.stdout)
		}
		return report
	}
	assertOldSQL := func() {
		t.Helper()
		current, err := bundle.Read(bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(current.Files["phases/expand.sql"]) != oldExpand || string(current.Files["phases/contract.sql"]) != oldContract || current.Manifest.PhaseSource != "edited" {
			t.Fatal("edited SQL or phase source changed while decisions were pending")
		}
	}
	revised := call([]string{"--target", "primary"})
	if revised.Durable.Outcome != string(protocol.NeedsInput) || !hasWorkflowFinding(revised, "answer_invalidated") {
		t.Fatalf("revised plan did not invalidate old rename: %#v", revised)
	}
	assertOldSQL()

	// Execute one emitted durable argv at a time. The first must require a
	// conscious rename choice; later argv must retain that reconfirmation.
	selected := nextDurableChoice(t, revised, rename)
	if got := argvHints(t, selected.Argv); !reflect.DeepEqual(got, []protocol.Hint{rename}) {
		t.Fatalf("invalidated rename was prefilled: %#v", got)
	}
	var final workflowPlanReport
	for step := 0; step < 6; step++ {
		final = call(selected.Argv[2:])
		if final.Durable.Outcome == string(protocol.NeedsSQLEdits) {
			break
		}
		if final.Durable.Outcome != string(protocol.NeedsInput) {
			t.Fatalf("unexpected revised outcome: %#v", final)
		}
		assertOldSQL()
		selected = nextRevisionChoice(t, final)
		hints := argvHints(t, selected.Argv)
		carriedRename := false
		for _, hint := range hints {
			carriedRename = carriedRename || reflect.DeepEqual(hint, rename)
		}
		if len(hints) < 2 || !carriedRename {
			t.Fatalf("reconfirmed rename omitted from sequential argv: %#v", hints)
		}
	}
	if final.Durable.Outcome != string(protocol.NeedsSQLEdits) || len(final.Durable.Edits) == 0 {
		t.Fatalf("sequential argv did not reach new SQL edits: %#v", final)
	}
}

func hasWorkflowFinding(report workflowPlanReport, code string) bool {
	for _, finding := range report.Durable.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func nextDurableChoice(t *testing.T, report workflowPlanReport, wanted protocol.Hint) workflowActionChoice {
	t.Helper()
	for _, action := range report.NextActions {
		if action.Scope != "durable" || action.Kind != "semantic_hint" {
			continue
		}
		for _, choice := range action.Choices {
			if reflect.DeepEqual(choice.Hint, wanted) {
				return choice
			}
		}
	}
	t.Fatalf("durable choice %#v missing from %#v", wanted, report.NextActions)
	return workflowActionChoice{}
}

func nextRevisionChoice(t *testing.T, report workflowPlanReport) workflowActionChoice {
	t.Helper()
	for _, action := range report.NextActions {
		if action.Scope != "durable" || action.Kind != "semantic_hint" {
			continue
		}
		for _, choice := range action.Choices {
			if choice.Hint.Kind == "reconcile" && choice.Hint.Strategy == "manual_sql" ||
				choice.Hint.Kind == "rename_backfill" && choice.Hint.Strategy == "manual_sql" ||
				choice.Hint.Kind == "manual_sql" && strings.HasSuffix(choice.Hint.Action, "_sql") ||
				choice.Hint.Kind == "manual_sql" && choice.Hint.Action == "rename_backfill" {
				return choice
			}
		}
	}
	t.Fatalf("next revised SQL choice missing from %#v", report.NextActions)
	return workflowActionChoice{}
}

func argvHints(t *testing.T, argv []string) []protocol.Hint {
	t.Helper()
	if len(argv) < 6 || !reflect.DeepEqual(argv[:4], []string{"onwardpg", "plan", "--target", "primary"}) || len(argv)%2 != 0 {
		t.Fatalf("invalid plan argv: %#v", argv)
	}
	var hints []protocol.Hint
	for index := 4; index < len(argv); index += 2 {
		if argv[index] != "--hint" {
			t.Fatalf("unexpected plan argv: %#v", argv)
		}
		var hint protocol.Hint
		if err := json.Unmarshal([]byte(argv[index+1]), &hint); err != nil {
			t.Fatal(err)
		}
		hints = append(hints, hint)
	}
	return hints
}
