package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/activeplan"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/historyinit"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/targetlock"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
)

// runTestDraftAt upgrades readable bundle-name anchors in scenario tests to
// the exact head references required by the public CLI. Production callers
// obtain the same value from `onwardpg history status`.
func runTestDraftAt(t *testing.T, arguments []string, repository string) int {
	t.Helper()
	args := append([]string(nil), arguments...)
	var target, selected string
	afterIndex := -1
	for index := 0; index+1 < len(args); index++ {
		switch args[index] {
		case "--target":
			target = args[index+1]
		case "--bundle":
			selected = args[index+1]
		case "--after":
			afterIndex = index + 1
		}
	}
	if target == "" || selected == "" || afterIndex < 0 || strings.Contains(args[afterIndex], "@") {
		return runDraftAt(args, repository)
	}
	config, err := workspace.Load(filepath.Join(repository, ".onwardpg.toml"))
	if err != nil {
		t.Fatal(err)
	}
	chain, existing, err := history.LoadExcluding(repository, config.BundleRoot, target, selected)
	if err != nil {
		chain, existing, err = history.LoadEditedDraft(repository, config.BundleRoot, target, selected)
	}
	if err != nil {
		return runDraftAt(args, repository)
	}
	assertedName := args[afterIndex]
	args[afterIndex] = assertedName + "@" + chain.HeadDigest
	if existing == nil {
		args = append(args, "--create")
	}
	return runDraftAt(args, repository)
}

func TestDriftCheckComparesReplayedHeadReadOnlyOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	ddl := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint, email text);\n"
	writeTestFile(t, repository, "schema.sql", ddl)
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	connection, err := pgx.Connect(context.Background(), liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), ddl+"CREATE INDEX users_email_manual_idx ON app.users (email);"); err != nil {
		t.Fatal(err)
	}

	driftedOutput := captureStdout(t, func() int {
		return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
	})
	if driftedOutput.code != 4 {
		t.Fatalf("drifted exit = %d, stdout = %s", driftedOutput.code, driftedOutput.stdout)
	}
	var drifted driftcheck.Report
	if err := json.Unmarshal([]byte(driftedOutput.stdout), &drifted); err != nil {
		t.Fatal(err)
	}
	if drifted.Outcome != "drifted" || len(drifted.Differences) == 0 {
		t.Fatalf("drift report = %#v", drifted)
	}
	foundIndex := false
	for _, difference := range drifted.Differences {
		if difference.Kind == "unexpected_in_actual" && strings.Contains(difference.ObjectID, "users_email_manual_idx") {
			foundIndex = true
		}
	}
	if !foundIndex {
		t.Fatalf("drift did not report the unexpected index: %#v", drifted.Differences)
	}
	var stillExists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass('app.users_email_manual_idx') IS NOT NULL`).Scan(&stillExists); err != nil {
		t.Fatal(err)
	}
	if !stillExists {
		t.Fatal("drift check modified the live database")
	}
	if _, err := connection.Exec(context.Background(), "DROP INDEX app.users_email_manual_idx;"); err != nil {
		t.Fatal(err)
	}
	cleanOutput := captureStdout(t, func() int {
		return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
	})
	if cleanOutput.code != 0 {
		t.Fatalf("clean drift exit = %d, stdout = %s", cleanOutput.code, cleanOutput.stdout)
	}
	var clean driftcheck.Report
	if err := json.Unmarshal([]byte(cleanOutput.stdout), &clean); err != nil {
		t.Fatal(err)
	}
	if clean.Outcome != "drift_free" || len(clean.Differences) != 0 {
		t.Fatalf("clean drift report = %#v", clean)
	}
}

func TestOrdinaryAdditiveDraftIsStableAndConvergesOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
`)
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.customer_profiles (
  id bigint PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES app.accounts (id),
  biography text
);
CREATE INDEX customer_profiles_account_id_idx ON app.customer_profiles (account_id);
`)
	baseChain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	baseRef := history.HeadRef(baseChain)
	held, err := targetlock.Acquire(filepath.Join(repository, ".onwardpg.toml"), "primary")
	if err != nil {
		t.Fatal(err)
	}
	concurrentDraft := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-profiles", "--after", baseRef, "--create"}, repository)
	})
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if concurrentDraft.code != 4 || !strings.Contains(concurrentDraft.stdout, "history_update_in_progress") {
		t.Fatalf("concurrent draft exit = %d, stdout = %s", concurrentDraft.code, concurrentDraft.stdout)
	}
	missingRefresh := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-profiles", "--after", baseRef}, repository)
	})
	if missingRefresh.code != 4 || !strings.Contains(missingRefresh.stdout, "selected_bundle_missing") {
		t.Fatalf("missing refresh exit = %d, stdout = %s", missingRefresh.code, missingRefresh.stdout)
	}
	drafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-profiles", "--after", "baseline"}, repository)
	})
	if drafted.code != 0 {
		t.Fatalf("additive draft exit = %d, stdout = %s", drafted.code, drafted.stdout)
	}
	var report draftflow.Report
	if err := json.Unmarshal([]byte(drafted.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != string(protocol.Planned) || len(report.Decisions) != 0 || report.Verification == nil || report.Verification.Outcome != "verified" {
		t.Fatalf("additive draft = %#v", report)
	}
	createdAgain := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-profiles", "--after", baseRef, "--create"}, repository)
	})
	if createdAgain.code != 4 || !strings.Contains(createdAgain.stdout, "selected_bundle_already_exists") {
		t.Fatalf("duplicate create exit = %d, stdout = %s", createdAgain.code, createdAgain.stdout)
	}
	destination := filepath.Join(repository, "onward-bundles", "primary", "customer-profiles")
	first, err := bundle.Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, phase := range []string{"expand", "migrate", "contract"} {
		if receipt, exists := first.Manifest.Phases[phase]; exists {
			joined += string(first.Files[receipt.Path])
		}
	}
	for _, fragment := range []string{`CREATE TABLE "app"."customer_profiles"`, `ADD CONSTRAINT`, `FOREIGN KEY`, `CREATE INDEX`} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("additive bundle missing %q:\n%s", fragment, joined)
		}
	}

	redrafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-profiles", "--after", "baseline"}, repository)
	})
	if redrafted.code != 0 {
		t.Fatalf("identical redraft exit = %d, stdout = %s", redrafted.code, redrafted.stdout)
	}
	second, err := bundle.Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical additive redraft changed the receipted bundle bytes or manifest")
	}
	checked := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-profiles", "--check"}, repository)
	})
	if checked.code != 0 {
		t.Fatalf("additive verify --check exit = %d, stdout = %s", checked.code, checked.stdout)
	}
}

func TestPlanAnchorsAndRestacksOneFeatureWithoutExplicitAfterOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanupDev := createTestDatabase(t, adminURL)
	defer cleanupDev()
	t.Setenv("ONWARDPG_PLAN_DEV_DATABASE_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_PLAN_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
dev_mode = "workspace"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}

	featureDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, booking_date date);\n"
	writeTestFile(t, repository, "schema.sql", featureDDL)
	first := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"booking-dates", "--target", "primary"}, repository)
	})
	if first.code != 0 {
		t.Fatalf("first plan = %d, %s", first.code, first.stdout)
	}
	var firstReport workflowPlanReport
	if err := json.Unmarshal([]byte(first.stdout), &firstReport); err != nil {
		t.Fatal(err)
	}
	if firstReport.Durable.Outcome != string(protocol.Planned) || firstReport.Development.Status != protocol.Planned {
		t.Fatalf("first workflow report = %#v", firstReport)
	}
	anchor, found, err := activeplan.Load(repository, "primary")
	if err != nil || !found {
		t.Fatalf("active plan = %#v, %v, %v", anchor, found, err)
	}
	featurePath := filepath.Join(repository, "onward-bundles", "primary", "booking-dates")
	firstArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if firstArtifact.Manifest.PlanID == "" || firstArtifact.Manifest.PlanID != anchor.PlanID {
		t.Fatalf("feature manifest/anchor identity mismatch: %#v / %#v", firstArtifact.Manifest, anchor)
	}
	// The developer deliberately applies only the separately rendered D -> W
	// SQL. onwardpg did not apply it; the next plan must observe that local
	// state and emit only the subsequent residual.
	firstLocalSQL := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"--target", "primary", "--output", "sql"}, repository)
	})
	if firstLocalSQL.code != 0 || !strings.Contains(firstLocalSQL.stdout, `CREATE TABLE "app"."accounts"`) {
		t.Fatalf("first workspace SQL = %d, %s", firstLocalSQL.code, firstLocalSQL.stdout)
	}
	devConnection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devConnection.Exec(context.Background(), firstLocalSQL.stdout); err != nil {
		devConnection.Close(context.Background())
		t.Fatalf("apply first workspace SQL externally: %v\n%s", err, firstLocalSQL.stdout)
	}
	if err := devConnection.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// An ordinary feature revision replaces the same bundle and keeps its local
	// logical identity.
	featureDDL = "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, booking_date date, note text);\n"
	writeTestFile(t, repository, "schema.sql", featureDDL)
	second := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"--target", "primary"}, repository) })
	if second.code != 0 {
		t.Fatalf("feature revision = %d, %s", second.code, second.stdout)
	}
	var secondReport workflowPlanReport
	if err := json.Unmarshal([]byte(second.stdout), &secondReport); err != nil {
		t.Fatal(err)
	}
	if len(secondReport.Development.Result.Statements) != 1 || !strings.Contains(secondReport.Development.Result.Statements[0].SQL, `ADD COLUMN "note"`) {
		t.Fatalf("feature revision should emit only the local note residual: %#v", secondReport.Development.Result)
	}
	secondLocalSQL := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"--target", "primary", "--output", "sql"}, repository)
	})
	if secondLocalSQL.code != 0 {
		t.Fatalf("second workspace SQL = %d, %s", secondLocalSQL.code, secondLocalSQL.stdout)
	}
	devConnection, err = pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devConnection.Exec(context.Background(), secondLocalSQL.stdout); err != nil {
		devConnection.Close(context.Background())
		t.Fatalf("apply second workspace SQL externally: %v\n%s", err, secondLocalSQL.stdout)
	}
	if err := devConnection.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if secondArtifact.Manifest.PlanID != anchor.PlanID || secondArtifact.Manifest.Generation <= firstArtifact.Manifest.Generation {
		t.Fatalf("feature revision did not preserve/advance identity: first=%#v second=%#v", firstArtifact.Manifest, secondArtifact.Manifest)
	}

	// Simulate Git bringing an accepted sibling bundle into the checkout. The
	// high-level command knows only its local selected plan; excluding it leaves
	// one accepted chain, so no copied head_ref is required.
	parked := filepath.Join(repository, "booking-dates.parked")
	if err := os.Rename(featurePath, parked); err != nil {
		t.Fatal(err)
	}
	upstreamDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text);\n"
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-timezone", upstreamDDL)
	// An accepted upstream bundle may provide explicit evidence for a data
	// effect. This marker is the only verification SQL onwardpg will inspect on
	// the caller-owned development database during the later restack.
	upstreamPath := filepath.Join(repository, "onward-bundles", "primary", "upstream-timezone")
	writeTestFile(t, upstreamPath, "verify.sql", "-- onwardpg:assert accepted_history_is_reviewed\n-- onwardpg:dev-postcondition\nSELECT true;\n")
	preparedUpstream, err := bundle.PrepareEdited(upstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.InstallReceipts(upstreamPath, preparedUpstream); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, featurePath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text, booking_date date, note text);\n")
	restacked := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"--target", "primary"}, repository) })
	if restacked.code != 0 {
		t.Fatalf("restack = %d, %s", restacked.code, restacked.stdout)
	}
	var restackedReport workflowPlanReport
	if err := json.Unmarshal([]byte(restacked.stdout), &restackedReport); err != nil {
		t.Fatal(err)
	}
	if !restackedReport.Durable.ParentChanged || restackedReport.Durable.PlanID != anchor.PlanID {
		t.Fatalf("restacked workflow report = %#v", restackedReport)
	}
	if len(restackedReport.Development.Postconditions) != 1 || restackedReport.Development.Postconditions[0].Status != "passed" || restackedReport.Development.Postconditions[0].ID != "accepted_history_is_reviewed" {
		t.Fatalf("restacked development evidence = %#v", restackedReport.Development.Postconditions)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 3 || chain.Entries[1].Directory != "upstream-timezone" || chain.Entries[2].Directory != "booking-dates" {
		t.Fatalf("restacked chain = %#v", chain)
	}
	// SQL mode is intentionally the direct development reconciliation, never
	// the accumulated feature bundle. It remains available after a restack.
	localSQL := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"--target", "primary", "--output", "sql"}, repository)
	})
	if localSQL.code != 0 || !strings.Contains(localSQL.stdout, "development workspace reconciliation") || !strings.Contains(localSQL.stdout, `ADD COLUMN "timezone"`) || strings.Contains(localSQL.stdout, `ADD COLUMN "booking_date"`) {
		t.Fatalf("workspace SQL after restack = %d, %s", localSQL.code, localSQL.stdout)
	}
	devConnection, err = pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devConnection.Exec(context.Background(), localSQL.stdout); err != nil {
		devConnection.Close(context.Background())
		t.Fatalf("apply restacked workspace SQL externally: %v\n%s", err, localSQL.stdout)
	}
	if err := devConnection.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	converged := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"--target", "primary"}, repository) })
	if converged.code != 0 {
		t.Fatalf("converged development plan = %d, %s", converged.code, converged.stdout)
	}
	var convergedReport workflowPlanReport
	if err := json.Unmarshal([]byte(converged.stdout), &convergedReport); err != nil {
		t.Fatal(err)
	}
	if len(convergedReport.Development.Result.Statements) != 0 || len(convergedReport.Development.Result.Preserved) != 0 || len(convergedReport.Development.Result.Compatibility) == 0 || !strings.Contains(strings.Join(convergedReport.Development.Result.Compatibility, "\n"), "column_physical_order") {
		t.Fatalf("development did not reach the expected workspace-compatible state after deliberate external SQL: %#v", convergedReport.Development.Result)
	}
}

func TestPlanAcceptsScopedDevelopmentHintsWithoutReusingDurableHintsOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanupDev := createTestDatabase(t, adminURL)
	defer cleanupDev()
	t.Setenv("ONWARDPG_SCOPED_HINT_DEV_DATABASE_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_SCOPED_HINT_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
dev_mode = "workspace"
`)
	// H already knows the desired customer table. D instead contains an older
	// same-shape accounts table, so only D -> W needs rename intent. H -> W has
	// an unrelated additive enum change and must not consume that local hint.
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint);\n")
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	connection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(context.Background(), "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);"); err != nil {
		connection.Close(context.Background())
		t.Fatal(err)
	}
	if err := connection.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TYPE app.event_kind AS ENUM ('created');\n")
	pendingOutput := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"customer-audit", "--target", "primary"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("plan with development decision = %d, %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending workflowPlanReport
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Durable.Outcome != string(protocol.Planned) || pending.Development.Status != protocol.NeedsInput || pending.Development.NextAction != "rerun_plan_with_dev_hints" {
		t.Fatalf("scoped decision report = %#v", pending)
	}
	var renameHint *protocol.Hint
	for _, decision := range pending.Development.Decisions {
		for _, choice := range decision.Choices {
			if choice.Hint.Kind == "rename" {
				hint := choice.Hint
				renameHint = &hint
			}
		}
	}
	if renameHint == nil {
		t.Fatalf("development rename decision = %#v", pending.Development.Decisions)
	}
	hintData, err := json.Marshal(renameHint)
	if err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"--target", "primary", "--dev-hint", string(hintData)}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("plan with scoped development hint = %d, %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned workflowPlanReport
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Durable.Outcome != string(protocol.Planned) || planned.Development.Status != protocol.Planned || planned.Development.NextAction != "" {
		t.Fatalf("scoped planned report = %#v", planned)
	}
	statements := protocol.RenderSQL(planned.Development.Result, "")
	if !strings.Contains(statements, `RENAME TO "customers"`) ||
		!strings.Contains(statements, `CREATE VIEW "app"."customers" WITH (security_invoker = true)`) ||
		!strings.Contains(statements, `DROP VIEW "app"."customers"`) ||
		!strings.Contains(statements, `CREATE TYPE "app"."event_kind" AS ENUM ('created')`) {
		t.Fatalf("scoped development SQL = %s", statements)
	}
}

func TestPlanSwitchesParkedWorktreePlansOnlyWhenActiveBundleIsAbsentOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanupDev := createTestDatabase(t, adminURL)
	defer cleanupDev()
	t.Setenv("ONWARDPG_SWITCH_DEV_DATABASE_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_SWITCH_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
dev_mode = "workspace"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n")
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}

	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, booking_date date);\n")
	if first := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"booking-dates", "--target", "primary"}, repository) }); first.code != 0 {
		t.Fatalf("first feature plan = %d, %s", first.code, first.stdout)
	}
	firstAnchor, found, err := activeplan.Load(repository, "primary")
	if err != nil || !found {
		t.Fatalf("first anchor = %#v, %v, %v", firstAnchor, found, err)
	}
	featureRoot := filepath.Join(repository, "onward-bundles", "primary")
	bookingPath := filepath.Join(featureRoot, "booking-dates")
	parkedBooking := filepath.Join(repository, "booking-dates.parked")
	if err := os.Rename(bookingPath, parkedBooking); err != nil {
		t.Fatal(err)
	}

	// This models a normal branch switch without asking onwardpg for Git state:
	// the old feature bundle is absent from the new checkout, so a named plan
	// may be created while the old local identity is parked for later return.
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, payment_date date);\n")
	if second := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"payment-dates", "--target", "primary"}, repository) }); second.code != 0 {
		t.Fatalf("second feature plan = %d, %s", second.code, second.stdout)
	}
	secondAnchor, found, err := activeplan.Load(repository, "primary")
	if err != nil || !found || secondAnchor.BundleID != "payment-dates" {
		t.Fatalf("second anchor = %#v, %v, %v", secondAnchor, found, err)
	}
	if saved, exists := secondAnchor.FindParked("booking-dates"); !exists || saved.PlanID != firstAnchor.PlanID {
		t.Fatalf("booking plan was not parked: %#v", secondAnchor)
	}

	paymentPath := filepath.Join(featureRoot, "payment-dates")
	parkedPayment := filepath.Join(repository, "payment-dates.parked")
	if err := os.Rename(paymentPath, parkedPayment); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parkedBooking, bookingPath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, booking_date date);\n")
	if restored := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"booking-dates", "--target", "primary"}, repository) }); restored.code != 0 {
		t.Fatalf("restored feature plan = %d, %s", restored.code, restored.stdout)
	}
	restoredAnchor, found, err := activeplan.Load(repository, "primary")
	if err != nil || !found || restoredAnchor.PlanID != firstAnchor.PlanID || restoredAnchor.BundleID != "booking-dates" {
		t.Fatalf("restored anchor = %#v, %v, %v", restoredAnchor, found, err)
	}
	if _, exists := restoredAnchor.FindParked("payment-dates"); !exists {
		t.Fatalf("payment plan was not parked on return: %#v", restoredAnchor)
	}
}

func TestPlanPersistsActiveAnchorWhenDevelopmentInspectionFailsAfterDurablePlanOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	t.Setenv("ONWARDPG_UNAVAILABLE_DEV_DATABASE_URL", "postgres://127.0.0.1:1/unavailable?connect_timeout=1")
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNAVAILABLE_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n")
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, note text);\n")
	output := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"booking-note", "--target", "primary"}, repository) })
	if output.code != 1 || !strings.Contains(output.stdout, "development_plan_error") {
		t.Fatalf("unavailable development database = %d, %s", output.code, output.stdout)
	}
	anchor, found, err := activeplan.Load(repository, "primary")
	if err != nil || !found || anchor.BundleID != "booking-note" {
		t.Fatalf("durable plan anchor = %#v, %v, %v", anchor, found, err)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "booking-note"))
	if err != nil || artifact.Manifest.PlanID != anchor.PlanID {
		t.Fatalf("durable plan bundle = %#v, %v", artifact.Manifest, err)
	}
}

func TestExplicitBaseAnchorPreventsAccidentalPRStackingOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n")
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	status := captureStdout(t, func() int {
		return runHistoryStatusAt([]string{"status", "--target", "primary"}, repository)
	})
	if status.code != 0 {
		t.Fatalf("history status exit = %d, stdout = %s", status.code, status.stdout)
	}
	var historyStatus history.StatusReport
	if err := json.Unmarshal([]byte(status.stdout), &historyStatus); err != nil {
		t.Fatal(err)
	}
	if historyStatus.Status != "valid" || historyStatus.HeadBundle != "baseline" || historyStatus.HeadRef == "" || len(historyStatus.Entries) != 1 {
		t.Fatalf("history status = %#v", historyStatus)
	}
	acceptedBaseRef := historyStatus.HeadRef

	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text);\n")
	missing := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone"}, repository)
	})
	if missing.code != 4 || !strings.Contains(missing.stdout, `"code":"base_anchor_required"`) {
		t.Fatalf("missing anchor = %d, %s", missing.code, missing.stdout)
	}
	wrong := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "not-main"}, repository)
	})
	if wrong.code != 4 || !strings.Contains(wrong.stdout, `"code":"base_anchor_mismatch"`) || !strings.Contains(wrong.stdout, `"base_bundle":"baseline"`) {
		t.Fatalf("wrong anchor = %d, %s", wrong.code, wrong.stdout)
	}
	forged := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline@sha256:" + strings.Repeat("0", 64), "--create"}, repository)
	})
	if forged.code != 4 || !strings.Contains(forged.stdout, `"code":"base_anchor_mismatch"`) {
		t.Fatalf("same-name rewritten anchor = %d, %s", forged.code, forged.stdout)
	}
	planned := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline"}, repository)
	})
	if planned.code != 0 {
		t.Fatalf("anchored draft = %d, %s", planned.code, planned.stdout)
	}
	selectedStatus := captureStdout(t, func() int {
		return runHistoryStatusAt([]string{"status", "--target", "primary", "--bundle", "account-timezone"}, repository)
	})
	if selectedStatus.code != 0 {
		t.Fatalf("selected status = %d, %s", selectedStatus.code, selectedStatus.stdout)
	}
	if err := json.Unmarshal([]byte(selectedStatus.stdout), &historyStatus); err != nil {
		t.Fatal(err)
	}
	if historyStatus.HeadBundle != "baseline" || historyStatus.Selected == nil || historyStatus.Selected.Relationship != "current" {
		t.Fatalf("selected history status = %#v", historyStatus)
	}

	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text, locale text);\n")
	stacked := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-locale", "--after", "baseline"}, repository)
	})
	if stacked.code != 4 || !strings.Contains(stacked.stdout, `"code":"base_anchor_mismatch"`) || !strings.Contains(stacked.stdout, `"base_bundle":"account-timezone"`) {
		t.Fatalf("accidental stack was not blocked = %d, %s", stacked.code, stacked.stdout)
	}
	writeHistoryTransitionFixture(t, repository, adminURL, "accepted-successor", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text); CREATE TABLE app.audit (id bigint);\n")
	nonHead := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "account-timezone", "--after", acceptedBaseRef}, repository)
	})
	if nonHead.code != 4 || !strings.Contains(nonHead.stdout, `"code":"selected_bundle_not_head"`) {
		t.Fatalf("historical non-head draft = %d, %s", nonHead.code, nonHead.stdout)
	}
}

func TestDraftReportsUnreachableColumnOrderBeforeWritingBundleOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, email text);\n")
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text, email text);\n")
	drafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline"}, repository)
	})
	if drafted.code != 3 || !strings.Contains(drafted.stdout, "column_physical_order:app.accounts") {
		t.Fatalf("unreachable order = %d, %s", drafted.code, drafted.stdout)
	}
	if _, err := os.Stat(filepath.Join(repository, "onward-bundles", "primary", "account-timezone")); !os.IsNotExist(err) {
		t.Fatalf("unsupported draft wrote a bundle: %v", err)
	}

	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, email text, zeta text, alpha text);\n")
	if appended := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline"}, repository)
	}); appended.code != 0 {
		t.Fatalf("appendable order = %d, %s", appended.code, appended.stdout)
	}
	destination := filepath.Join(repository, "onward-bundles", "primary", "account-timezone")
	before, err := bundle.Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text, email text, zeta text, alpha text);\n")
	rejected := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline"}, repository)
	})
	if rejected.code != 3 || !strings.Contains(rejected.stdout, "column_physical_order:app.accounts") {
		t.Fatalf("unreachable replacement = %d, %s", rejected.code, rejected.stdout)
	}
	after, err := bundle.Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unsupported redraft replaced the last complete selected bundle")
	}
}

func TestRestackRemovesGeneratedBundleAbsorbedByBaseOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n"
	desiredDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint, timezone text);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)
	if initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", desiredDDL)
	if drafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "baseline"}, repository)
	}); drafted.code != 0 {
		t.Fatalf("feature draft = %d, %s", drafted.code, drafted.stdout)
	}
	featurePath := filepath.Join(repository, "onward-bundles", "primary", "account-timezone")
	parkedPath := filepath.Join(repository, "account-timezone.parked")
	if err := os.Rename(featurePath, parkedPath); err != nil {
		t.Fatal(err)
	}
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-timezone", desiredDDL)
	if err := os.Rename(parkedPath, featurePath); err != nil {
		t.Fatal(err)
	}
	absorbed := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "account-timezone", "--after", "upstream-timezone"}, repository)
	})
	if absorbed.code != 0 {
		t.Fatalf("absorbed restack = %d, %s", absorbed.code, absorbed.stdout)
	}
	var report draftflow.Report
	if err := json.Unmarshal([]byte(absorbed.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "absorbed" || !report.RemovedBundle || len(report.Findings) != 1 || report.Findings[0].Code != "feature_absorbed_by_base" {
		t.Fatalf("absorbed report = %#v", report)
	}
	if _, err := os.Stat(featurePath); !os.IsNotExist(err) {
		t.Fatalf("absorbed bundle still exists: %v", err)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 2 || chain.Entries[1].Directory != "upstream-timezone" {
		t.Fatalf("absorbed chain = %#v", chain)
	}
}

func TestGitFreeInitDraftHintVerifyAndRestackOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanupDev := createTestDatabase(t, adminURL)
	defer cleanupDev()
	t.Setenv("ONWARDPG_ITERATION_DEV_DATABASE_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_ITERATION_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at timestamp);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)

	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	if _, err := os.Stat(filepath.Join(repository, ".git")); !os.IsNotExist(err) {
		t.Fatalf("workflow unexpectedly created or required Git metadata: %v", err)
	}

	featureDDL := "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at timestamp);\n"
	writeTestFile(t, repository, "schema.sql", featureDDL)
	pendingOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "baseline"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("pending draft exit = %d, stdout = %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending struct {
		ProtocolVersion string              `json:"protocol_version"`
		Status          string              `json:"status"`
		NextAction      string              `json:"next_action"`
		Path            string              `json:"path"`
		WrittenReceipts []string            `json:"written_receipts"`
		Decisions       []protocol.Decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.ProtocolVersion != draftflow.Version || pending.Status != "needs_decisions" || pending.NextAction != "rerun_without_create_with_hints" || pending.Path == "" || len(pending.WrittenReceipts) == 0 || len(pending.Decisions) == 0 {
		t.Fatalf("pending draft = %#v", pending)
	}
	var renameHint *protocol.Hint
	for _, decision := range pending.Decisions {
		for _, choice := range decision.Choices {
			if choice.Hint.Kind == "rename" && choice.Hint.Object == "table" {
				hint := choice.Hint
				renameHint = &hint
			}
		}
	}
	if renameHint == nil {
		t.Fatalf("draft did not offer a semantic table rename: %#v", pending.Decisions)
	}
	hintData, err := json.Marshal(renameHint)
	if err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "baseline", "--hint", string(hintData)}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("planned draft exit = %d, stdout = %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned draftflow.Report
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Outcome != string(protocol.Planned) || planned.Verification == nil || planned.Verification.Outcome != "verified" {
		t.Fatalf("planned draft = %#v", planned)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "customer-rename"))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest != planned.HistoryHead {
		t.Fatalf("draft manifest = %#v", artifact.Manifest)
	}
	if artifact.Manifest.AnswersDigest == "" || artifact.Manifest.QuestionsDigest == "" || artifact.Manifest.SemanticDigest == "" {
		t.Fatalf("draft did not retain semantic and fingerprint-bound decision receipts: %#v", artifact.Manifest)
	}
	receiptedHints, err := bundle.SemanticHints(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(receiptedHints) != 1 || receiptedHints[0].Kind != "rename" {
		t.Fatalf("semantic decisions = %#v", receiptedHints)
	}
	verified := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if verified.code != 0 {
		t.Fatalf("verify exit = %d, stdout = %s", verified.code, verified.stdout)
	}

	// The feature now needs a product-aware timestamp-to-date conversion on an
	// independent table. The existing table-rename receipt remains valid; only
	// the newly ambiguous conversion needs agent intent.
	featureDDL = "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at date);\n"
	writeTestFile(t, repository, "schema.sql", featureDDL)
	typePendingOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "baseline"}, repository)
	})
	if typePendingOutput.code != 2 {
		t.Fatalf("type decision exit = %d, stdout = %s", typePendingOutput.code, typePendingOutput.stdout)
	}
	var typePending struct {
		Decisions []protocol.Decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(typePendingOutput.stdout), &typePending); err != nil {
		t.Fatal(err)
	}
	if len(typePending.Decisions) != 1 || typePending.Decisions[0].Choices[0].Hint.Kind != "type_change" {
		t.Fatalf("independent edit did not preserve rename intent and ask only for the type decision: %#v", typePending)
	}
	manualTypeHint := protocol.Hint{Kind: "type_change", Name: []string{"app", "customer_events", "occurred_at"}, Strategy: "manual_sql"}
	manualHintData, err := json.Marshal(manualTypeHint)
	if err != nil {
		t.Fatal(err)
	}
	typeHandoffOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "baseline", "--hint", string(manualHintData)}, repository)
	})
	if typeHandoffOutput.code != 2 || !strings.Contains(typeHandoffOutput.stdout, "needs_sql_edits") {
		t.Fatalf("type SQL handoff exit = %d, stdout = %s", typeHandoffOutput.code, typeHandoffOutput.stdout)
	}
	featurePath := filepath.Join(repository, "onward-bundles", "primary", "customer-rename")
	conversionSQL := `-- Product conversion chosen from feature context.
ALTER TABLE "app"."customer_events"
  ALTER COLUMN "occurred_at" TYPE date
  USING "occurred_at"::date;
`
	conversionVerification := `-- onwardpg:assert occurred_at_is_date
SELECT data_type = 'date'
FROM information_schema.columns
WHERE table_schema = 'app'
  AND table_name = 'customer_events'
  AND column_name = 'occurred_at';
`
	writeTestFile(t, featurePath, "phases/migrate.sql", conversionSQL)
	writeTestFile(t, featurePath, "verify.sql", conversionVerification)
	unreceiptedBeforeRestack := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if unreceiptedBeforeRestack.code != 4 || !strings.Contains(unreceiptedBeforeRestack.stdout, "unreceipted_sql_edits") {
		t.Fatalf("unreceipted pre-restack check exit = %d, stdout = %s", unreceiptedBeforeRestack.code, unreceiptedBeforeRestack.stdout)
	}

	// Simulate an agent rebasing files: an upstream bundle now follows the old
	// baseline while the explicitly selected, still-unreceipted feature names
	// that baseline as its parent. No Git API is involved in detecting or
	// repairing the fork, and restacking must not require first parking upstream
	// again merely to install verification receipts.
	parkedPath := filepath.Join(repository, "customer-rename.parked")
	if err := os.Rename(featurePath, parkedPath); err != nil {
		t.Fatal(err)
	}
	upstreamDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at timestamp); CREATE TABLE app.audit_log (id bigint);\n"
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-audit", upstreamDDL)
	if err := os.Rename(parkedPath, featurePath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint);\n")

	restackedOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "upstream-audit"}, repository)
	})
	if restackedOutput.code != 0 {
		t.Fatalf("restacked draft exit = %d, stdout = %s", restackedOutput.code, restackedOutput.stdout)
	}
	var restacked draftflow.Report
	if err := json.Unmarshal([]byte(restackedOutput.stdout), &restacked); err != nil {
		t.Fatal(err)
	}
	if !restacked.ParentChanged || restacked.PreviousParent == restacked.HistoryHead || restacked.AnswerRebind == nil || len(restacked.AnswerRebind.Carried) == 0 {
		t.Fatalf("restacked draft = %#v", restacked)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 3 || chain.Entries[1].Directory != "upstream-audit" || chain.Entries[2].Directory != "customer-rename" {
		t.Fatalf("restacked chain = %#v", chain)
	}

	// A second independent base migration lands during the same feature. The
	// coding agent brings those files into the checkout; onwardpg again needs
	// only the selected bundle ID to distinguish the movable tip.
	if err := os.Rename(featurePath, parkedPath); err != nil {
		t.Fatal(err)
	}
	secondUpstreamDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at timestamp); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n"
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-settings", secondUpstreamDDL)
	if err := os.Rename(parkedPath, featurePath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")
	secondRestackOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "upstream-settings"}, repository)
	})
	if secondRestackOutput.code != 0 {
		t.Fatalf("second base restack exit = %d, stdout = %s", secondRestackOutput.code, secondRestackOutput.stdout)
	}
	var secondRestack draftflow.Report
	if err := json.Unmarshal([]byte(secondRestackOutput.stdout), &secondRestack); err != nil {
		t.Fatal(err)
	}
	if !secondRestack.ParentChanged || secondRestack.AnswerRebind == nil || len(secondRestack.AnswerRebind.Carried) == 0 {
		t.Fatalf("second base restack = %#v", secondRestack)
	}
	chain, err = history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 4 || chain.Entries[1].Directory != "upstream-audit" || chain.Entries[2].Directory != "upstream-settings" || chain.Entries[3].Directory != "customer-rename" {
		t.Fatalf("twice-restacked chain = %#v", chain)
	}

	// Ownership now moves to the coding agent. It adds product-specific SQL and
	// a boolean assertion directly to the migration folder. Read-only check mode
	// rejects the unreceipted edits; normal verification executes them only in
	// disposable PostgreSQL and receipts their exact bytes after convergence.
	secondRestackedArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	baseMigrate := string(secondRestackedArtifact.Files["phases/migrate.sql"])
	if !strings.Contains(baseMigrate, `"occurred_at"::date`) {
		t.Fatalf("restack lost product conversion SQL: %s", baseMigrate)
	}
	migrateSQL := baseMigrate + `
-- Product-aware work owned by the feature agent.
CREATE TEMP TABLE onwardpg_agent_receipt (value text NOT NULL);
INSERT INTO onwardpg_agent_receipt (value) VALUES ('customer-rename');
`
	writeTestFile(t, featurePath, "phases/migrate.sql", migrateSQL)
	writeTestFile(t, featurePath, "verify.sql", conversionVerification+`-- onwardpg:assert agent_sql_ran
SELECT count(*) = 1 FROM onwardpg_agent_receipt WHERE value = 'customer-rename';
`)
	unreceiptedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if unreceiptedCheck.code != 4 {
		t.Fatalf("read-only check exit = %d, want 4: %s", unreceiptedCheck.code, unreceiptedCheck.stdout)
	}
	if !strings.Contains(unreceiptedCheck.stdout, `"code":"unreceipted_sql_edits"`) {
		t.Fatalf("read-only check did not explain the receipt step: %s", unreceiptedCheck.stdout)
	}
	receiptedOutput := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if receiptedOutput.code != 0 {
		t.Fatalf("edited verification exit = %d, stdout = %s", receiptedOutput.code, receiptedOutput.stdout)
	}
	var receiptedReport verify.Report
	if err := json.Unmarshal([]byte(receiptedOutput.stdout), &receiptedReport); err != nil {
		t.Fatal(err)
	}
	if !receiptedReport.ReceiptsUpdated || receiptedReport.Outcome != "verified" || receiptedReport.SelectedBatches == 0 || receiptedReport.ExecutedBatches < receiptedReport.SelectedBatches || !reflect.DeepEqual(receiptedReport.VerifiedAssertions, []string{"occurred_at_is_date", "agent_sql_ran"}) {
		t.Fatalf("edited verification = %#v", receiptedReport)
	}
	receiptedArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if receiptedArtifact.Manifest.PhaseSource != "edited" || receiptedArtifact.Manifest.VerificationDigest == "" || string(receiptedArtifact.Files["phases/migrate.sql"]) != migrateSQL {
		t.Fatalf("receipted edited artifact = %#v", receiptedArtifact.Manifest)
	}
	checked := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if checked.code != 0 {
		t.Fatalf("receipted read-only check exit = %d, stdout = %s", checked.code, checked.stdout)
	}

	// A receipted bundle is not fresh merely because it still converges to its
	// own recorded plan. CI must compare it with the schema the working code now
	// exports and direct the agent back through the same draft loop.
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, unplanned text); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")
	staleCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if staleCheck.code != 4 {
		t.Fatalf("stale read-only check exit = %d, stdout = %s", staleCheck.code, staleCheck.stdout)
	}
	var staleReport verify.Report
	if err := json.Unmarshal([]byte(staleCheck.stdout), &staleReport); err != nil {
		t.Fatal(err)
	}
	if staleReport.Outcome != "stale" || len(staleReport.Findings) != 1 || staleReport.Findings[0].Code != "working_schema_changed" || staleReport.WorkingFingerprint == staleReport.DesiredFingerprint {
		t.Fatalf("stale read-only report = %#v", staleReport)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")

	// Applying the current migration to the developer database is not a history
	// transition. The feature may keep evolving, and the same explicit bundle
	// must remain the cumulative H → W draft.
	chain, err = history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	devConnection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	defer devConnection.Close(context.Background())
	for _, entry := range chain.Entries {
		for _, phase := range []string{"expand", "migrate", "contract"} {
			receipt, exists := entry.Artifact.Manifest.Phases[phase]
			if !exists {
				continue
			}
			if _, err := devConnection.Exec(context.Background(), string(entry.Artifact.Files[receipt.Path])); err != nil {
				t.Fatalf("apply %s/%s to disposable developer fixture: %v", entry.Directory, phase, err)
			}
		}
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint, note text); CREATE TABLE app.settings (id bigint);\n")

	localOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if localOutput.code != 0 {
		t.Fatalf("local residual plan exit = %d, stdout = %s", localOutput.code, localOutput.stdout)
	}
	var localPlan protocol.Result
	if err := json.Unmarshal([]byte(localOutput.stdout), &localPlan); err != nil {
		t.Fatal(err)
	}
	if len(localPlan.Statements) != 1 || !strings.Contains(localPlan.Statements[0].SQL, "note") {
		t.Fatalf("D → W should contain only the newly added column: %#v", localPlan.Statements)
	}

	redraftedOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "upstream-settings"}, repository)
	})
	if redraftedOutput.code != 0 {
		t.Fatalf("same-bundle redraft exit = %d, stdout = %s", redraftedOutput.code, redraftedOutput.stdout)
	}
	var redrafted draftflow.Report
	if err := json.Unmarshal([]byte(redraftedOutput.stdout), &redrafted); err != nil {
		t.Fatal(err)
	}
	if redrafted.EditReconciliation == nil || redrafted.EditReconciliation.Outcome != "reconciled" || len(redrafted.EditReconciliation.Conflicts) != 0 {
		t.Fatalf("same-bundle reconciliation = %#v", redrafted)
	}
	refreshedArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshedArtifact.Files["phases/migrate.sql"]) != migrateSQL || string(refreshedArtifact.Files["verify.sql"]) != string(receiptedArtifact.Files["verify.sql"]) {
		t.Fatalf("same-bundle redraft lost agent-owned SQL: %#v", refreshedArtifact.Manifest)
	}
	foundNote := false
	for _, receipt := range refreshedArtifact.Manifest.Phases {
		if strings.Contains(string(refreshedArtifact.Files[receipt.Path]), "note") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatal("same-bundle redraft did not include the new column")
	}
	refreshedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if refreshedCheck.code != 0 {
		t.Fatalf("redrafted read-only check exit = %d, stdout = %s", refreshedCheck.code, refreshedCheck.stdout)
	}

	// When both the agent and generator change expand, draft moves the bundle's
	// receipts to the new plan but preserves the current SQL as an unreceipted
	// three-way handoff. This makes the conflict resolvable through ordinary SQL
	// editing followed by verify, without a merge DSL or Git knowledge.
	expandReceipt, exists := refreshedArtifact.Manifest.Phases["expand"]
	if !exists {
		t.Fatal("fixture needs an expand phase for the conflict handoff")
	}
	agentExpand := string(refreshedArtifact.Files[expandReceipt.Path]) + "\n-- Agent-owned rollout note retained across regeneration.\n"
	writeTestFile(t, featurePath, expandReceipt.Path, agentExpand)
	receiptAgentExpand := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if receiptAgentExpand.code != 0 {
		t.Fatalf("receipt agent expand edit = %d, %s", receiptAgentExpand.code, receiptAgentExpand.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, timezone text); CREATE TABLE app.customer_events (id bigint, occurred_at date); CREATE TABLE app.audit_log (id bigint, note text); CREATE TABLE app.settings (id bigint);\n")
	conflictedOutput := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "customer-rename", "--after", "upstream-settings"}, repository)
	})
	if conflictedOutput.code != 4 {
		t.Fatalf("same-phase conflict exit = %d, stdout = %s", conflictedOutput.code, conflictedOutput.stdout)
	}
	var conflicted draftflow.Report
	if err := json.Unmarshal([]byte(conflictedOutput.stdout), &conflicted); err != nil {
		t.Fatal(err)
	}
	if conflicted.Outcome != "blocked" || conflicted.EditReconciliation == nil || len(conflicted.EditReconciliation.Conflicts) != 1 {
		t.Fatalf("same-phase conflict report = %#v", conflicted)
	}
	conflict := conflicted.EditReconciliation.Conflicts[0]
	if conflict.Path != expandReceipt.Path || conflict.OldGeneratedSQL == nil || conflict.CurrentSQL == nil || conflict.NewGeneratedSQL == nil || !strings.Contains(*conflict.CurrentSQL, "Agent-owned rollout note") || !strings.Contains(*conflict.NewGeneratedSQL, "timezone") {
		t.Fatalf("same-phase conflict evidence = %#v", conflict)
	}
	preservedExpand, err := os.ReadFile(filepath.Join(featurePath, filepath.FromSlash(conflict.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(preservedExpand), "Agent-owned rollout note") || strings.Contains(string(preservedExpand), "timezone") {
		t.Fatalf("conflict handoff overwrote current SQL: %s", preservedExpand)
	}
	mergedExpand := *conflict.NewGeneratedSQL + "\n-- Agent-owned rollout note retained across regeneration.\n"
	writeTestFile(t, featurePath, conflict.Path, mergedExpand)
	resolvedOutput := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if resolvedOutput.code != 0 {
		t.Fatalf("resolved same-phase conflict = %d, %s", resolvedOutput.code, resolvedOutput.stdout)
	}
	resolvedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if resolvedCheck.code != 0 {
		t.Fatalf("resolved conflict read-only check = %d, %s", resolvedCheck.code, resolvedCheck.stdout)
	}
}

func TestSemanticManualSQLHandoffRequiresEditedCloneVerificationOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
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
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.events (id bigint, occurred_on text);\n")
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.events (id bigint, occurred_on date);\n")
	hint := `{"kind":"type_change","name":["app","events","occurred_on"],"strategy":"manual_sql"}`
	drafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "event-date", "--after", "baseline", "--hint", hint}, repository)
	})
	if drafted.code != 2 {
		t.Fatalf("manual draft exit = %d, stdout = %s", drafted.code, drafted.stdout)
	}
	var handoff struct {
		ProtocolVersion string   `json:"protocol_version"`
		Status          string   `json:"status"`
		Path            string   `json:"path"`
		Edit            []string `json:"edit"`
	}
	if err := json.Unmarshal([]byte(drafted.stdout), &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.ProtocolVersion != draftflow.Version || handoff.Status != string(protocol.NeedsSQLEdits) || len(handoff.Edit) != 1 || handoff.Edit[0] != "phases/migrate.sql" {
		t.Fatalf("handoff = %#v", handoff)
	}
	bundlePath := filepath.Join(repository, filepath.FromSlash(handoff.Path))
	artifact, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.State != string(protocol.NeedsSQLEdits) || !strings.Contains(string(artifact.Files["phases/migrate.sql"]), "ONWARDPG TODO") {
		t.Fatalf("incomplete bundle = %#v", artifact.Manifest)
	}
	todoCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "event-date", "--check"}, repository)
	})
	if todoCheck.code != 4 || !strings.Contains(todoCheck.stdout, `"code":"unresolved_sql_todo"`) || !strings.Contains(todoCheck.stdout, "phases/migrate.sql") {
		t.Fatalf("TODO check = %d, %s", todoCheck.code, todoCheck.stdout)
	}
	writeTestFile(t, bundlePath, "phases/migrate.sql", "ALTER TABLE app.events ALTER COLUMN occurred_on TYPE date USING occurred_on::date;\n")
	verified := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "event-date"}, repository)
	})
	if verified.code != 0 {
		t.Fatalf("edited verification exit = %d, stdout = %s", verified.code, verified.stdout)
	}
	receipted, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if receipted.Manifest.State != string(protocol.Planned) || receipted.Manifest.PhaseSource != "edited" {
		t.Fatalf("receipted bundle = %#v", receipted.Manifest)
	}
}

func TestHistoryInitCreatesVerifiedGroundFloorWithoutApplyingToAdminDatabase(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	schemaName := "history_init_" + randomToken(t)
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".users (id bigint PRIMARY KEY);`)

	connection, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var existedBefore bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regnamespace($1) IS NOT NULL`, schemaName).Scan(&existedBefore); err != nil {
		t.Fatal(err)
	}
	if existedBefore {
		t.Fatalf("test schema %s already exists in admin database", schemaName)
	}

	output := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary", "--bundle", "ground-floor"}, repository)
	})
	if output.code != 0 {
		t.Fatalf("history init exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report historyinit.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.ProtocolVersion != historyinit.Version || report.Outcome != "initialized" || report.BundleID != "ground-floor" || report.Verification == nil || report.Verification.Outcome != "verified" {
		t.Fatalf("history init report = %#v", report)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 1 || chain.HeadDigest != report.HistoryHead {
		t.Fatalf("history chain = %#v", chain)
	}
	manifest := chain.Entries[0].Artifact.Manifest
	if manifest.BundleID != "ground-floor" || manifest.Purpose != "baseline" || manifest.History == nil || manifest.History.ParentDigest != bundle.HistoryRootDigest() {
		t.Fatalf("baseline manifest = %#v", manifest)
	}
	if !strings.Contains(string(chain.Entries[0].Artifact.Files["phases/expand.sql"]), `CREATE TABLE "`+schemaName+`"."users"`) {
		t.Fatalf("baseline expand SQL = %s", chain.Entries[0].Artifact.Files["phases/expand.sql"])
	}
	var existsAfter bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regnamespace($1) IS NOT NULL`, schemaName).Scan(&existsAfter); err != nil {
		t.Fatal(err)
	}
	if existsAfter {
		t.Fatalf("history init applied declarative schema to caller database")
	}

	second := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary", "--bundle", "another-root"}, repository)
	})
	if second.code != 4 {
		t.Fatalf("second history init exit = %d, stdout = %s", second.code, second.stdout)
	}
	var blocked historyinit.Report
	if err := json.Unmarshal([]byte(second.stdout), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Outcome != "blocked" || len(blocked.Findings) != 1 || blocked.Findings[0].Code != "history_already_initialized" {
		t.Fatalf("second history init report = %#v", blocked)
	}

	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".users (id bigint PRIMARY KEY); CREATE TABLE "`+schemaName+`".projects (id bigint PRIMARY KEY);`)
	feature := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "projects", "--after", "ground-floor"}, repository)
	})
	if feature.code != 0 {
		t.Fatalf("draft after history init exit = %d, stdout = %s", feature.code, feature.stdout)
	}
	featureArtifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	if featureArtifact.Manifest.History == nil || featureArtifact.Manifest.History.ParentDigest != report.HistoryHead {
		t.Fatalf("feature history receipt = %#v, baseline head = %s", featureArtifact.Manifest.History, report.HistoryHead)
	}
}

func TestHistoryInitWritesNothingForUnsupportedBaseline(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE DOMAIN public.email_address AS text CHECK (VALUE <> '');\n")

	output := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary"}, repository)
	})
	if output.code != 3 {
		t.Fatalf("history init exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report historyinit.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != string(protocol.Unsupported) || report.Plan == nil || report.Plan.Status != protocol.Unsupported {
		t.Fatalf("history init report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(repository, "onward-bundles")); !os.IsNotExist(err) {
		t.Fatalf("unsupported history init wrote bundle root: %v", err)
	}
}

func TestConfigCheckMaterializesEveryTarget(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	t.Setenv("ONWARDPG_CONFIG_CHECK_URL", url)
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_CONFIG_CHECK_URL"
scratch_database_env = "ONWARDPG_CONFIG_CHECK_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.config_check (id bigint PRIMARY KEY);\n")
	output := captureStdout(t, func() int {
		return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
	})
	if output.code != 0 {
		t.Fatalf("config check exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report struct {
		ProtocolVersion string `json:"protocol_version"`
		Status          string `json:"status"`
		Targets         []struct {
			Name                 string `json:"name"`
			Provenance           string `json:"provenance"`
			Fingerprint          string `json:"fingerprint"`
			DevPostgresMajor     int    `json:"dev_postgres_major"`
			ScratchPostgresMajor int    `json:"scratch_postgres_major"`
			HistoryPostgresMajor int    `json:"history_postgres_major"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.ProtocolVersion != "onwardpg.config-check/v3" || report.Status != "valid" || len(report.Targets) != 1 {
		t.Fatalf("config check report = %#v", report)
	}
	target := report.Targets[0]
	if target.Name != "primary" || target.Provenance != "schema_file:schema.sql" || !strings.HasPrefix(target.Fingerprint, "sha256:") || target.DevPostgresMajor < 15 || target.DevPostgresMajor > 18 || target.ScratchPostgresMajor != target.DevPostgresMajor {
		t.Fatalf("config check target = %#v", target)
	}
	if target.HistoryPostgresMajor != 0 {
		t.Fatalf("uninitialized config unexpectedly reported a history major: %#v", target)
	}

	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("config fixture init = %d, %s", initialized.code, initialized.stdout)
	}
	withHistory := captureStdout(t, func() int {
		return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
	})
	if withHistory.code != 0 {
		t.Fatalf("config check with history = %d, %s", withHistory.code, withHistory.stdout)
	}
	if err := json.Unmarshal([]byte(withHistory.stdout), &report); err != nil {
		t.Fatal(err)
	}
	target = report.Targets[0]
	if target.HistoryPostgresMajor != target.DevPostgresMajor {
		t.Fatalf("config check did not validate the history major: %#v", target)
	}
}

func TestDevPlanQuestionsNeverApplyAndConvergesAfterDeliberateApplication(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	t.Setenv("ONWARDPG_DEV_TEST_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_DEV_TEST_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint);\n")
	connection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);"); err != nil {
		t.Fatal(err)
	}

	pendingOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("pending exit = %d, stdout = %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending struct {
		ProtocolVersion string              `json:"protocol_version"`
		Status          string              `json:"status"`
		Decisions       []protocol.Decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.ProtocolVersion != "onwardpg.dev-plan/v4" || pending.Status != "needs_decisions" || len(pending.Decisions) != 1 {
		t.Fatalf("pending = %#v", pending)
	}
	var renameHint *protocol.Hint
	for _, choice := range pending.Decisions[0].Choices {
		if choice.Hint.Kind == "rename" {
			hint := choice.Hint
			renameHint = &hint
		}
	}
	if renameHint == nil {
		t.Fatalf("pending decision has no rename hint: %#v", pending)
	}
	hintData, err := json.Marshal(renameHint)
	if err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary", "--hint", string(hintData)}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("planned exit = %d, stdout = %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned protocol.Result
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.ProtocolVersion != "onwardpg.dev-plan/v4" || planned.Status != protocol.Planned || len(planned.Statements) == 0 || planned.Statements[0].Phase != "contract" {
		t.Fatalf("planned = %#v", planned)
	}
	var oldExists, newExists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass('app.accounts') IS NOT NULL, to_regclass('app.customers') IS NOT NULL`).Scan(&oldExists, &newExists); err != nil {
		t.Fatal(err)
	}
	if !oldExists || newExists {
		t.Fatalf("dev plan applied SQL: old=%t new=%t", oldExists, newExists)
	}
	for _, statement := range planned.Statements {
		if _, err := connection.Exec(context.Background(), statement.SQL); err != nil {
			t.Fatalf("deliberate local application: %v", err)
		}
	}
	residualOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if residualOutput.code != 0 {
		t.Fatalf("residual exit = %d, stdout = %s", residualOutput.code, residualOutput.stdout)
	}
	var residual struct {
		ProtocolVersion    string `json:"protocol_version"`
		Status             string `json:"status"`
		Changed            bool   `json:"changed"`
		CurrentFingerprint string `json:"current_fingerprint"`
		DesiredFingerprint string `json:"desired_fingerprint"`
	}
	if err := json.Unmarshal([]byte(residualOutput.stdout), &residual); err != nil {
		t.Fatal(err)
	}
	if residual.ProtocolVersion != "onwardpg.dev-plan/v4" || residual.Status != "no_changes" || residual.Changed || residual.CurrentFingerprint != residual.DesiredFingerprint {
		t.Fatalf("residual = %#v", residual)
	}
}

func TestBundleVerifyReportsPartialResidualAndFullConvergenceOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.obsolete (id bigint PRIMARY KEY);\n"
	writeTestFile(t, repository, "schema.sql", "")
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	writeHistoryContractDropFixture(t, repository, url, "remove-obsolete")
	bundlePath := filepath.Join(repository, "onward-bundles", "primary", "remove-obsolete")
	writeTestFile(t, bundlePath, "verify.sql", "-- onwardpg:assert obsolete_removed\nSELECT to_regclass('app.obsolete') IS NULL;\n")
	edited, err := bundle.PrepareEdited(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.InstallReceipts(bundlePath, edited); err != nil {
		t.Fatal(err)
	}

	before := disposableDatabaseCount(t, url)
	partial := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete", "--through", "expand"}, repository)
	})
	if partial.code != 0 {
		t.Fatalf("partial exit = %d, stdout = %s", partial.code, partial.stdout)
	}
	var partialReport verify.Report
	if err := json.Unmarshal([]byte(partial.stdout), &partialReport); err != nil {
		t.Fatal(err)
	}
	if partialReport.Outcome != "partial_verified" || partialReport.Residual == nil || partialReport.Residual.Status != protocol.NeedsInput || len(partialReport.SimulatedPhases) != 0 || !reflect.DeepEqual(partialReport.RemainingPhases, []string{"contract"}) || len(partialReport.VerifiedAssertions) != 0 || !reflect.DeepEqual(partialReport.ContinuationAssertions, []string{"obsolete_removed"}) {
		t.Fatalf("partial report = %#v", partialReport)
	}

	full := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete"}, repository)
	})
	if full.code != 0 {
		t.Fatalf("full exit = %d, stdout = %s", full.code, full.stdout)
	}
	var fullReport verify.Report
	if err := json.Unmarshal([]byte(full.stdout), &fullReport); err != nil {
		t.Fatal(err)
	}
	if fullReport.ProtocolVersion != verify.Version || fullReport.Outcome != "verified" || fullReport.ObservedFingerprint != fullReport.DesiredFingerprint || fullReport.Residual == nil || len(fullReport.Residual.Statements) != 0 || !reflect.DeepEqual(fullReport.VerifiedAssertions, []string{"obsolete_removed"}) || len(fullReport.ContinuationAssertions) != 0 {
		t.Fatalf("full report = %#v", fullReport)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("disposable database count = %d, want %d", after, before)
	}
}

func TestVerifyFailureModesAlwaysCleanDisposableDatabases(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	tests := []struct {
		name       string
		phaseSQL   string
		verifySQL  string
		wantCode   string
		cancelSoon bool
	}{
		{
			name: "transactional failure",
			phaseSQL: "-- onwardpg:batch transactional\n" +
				"CREATE SCHEMA should_rollback;\nSELECT definitely_not_a_function();\n",
			wantCode: "transactional_batch_failed",
		},
		{
			name: "non-transactional failure",
			phaseSQL: "-- onwardpg:batch nontransactional\n" +
				"CREATE SCHEMA partial_disposable_effect;\nSELECT definitely_not_a_function();\n",
			wantCode: "non_transactional_batch_failed",
		},
		{
			name:      "false assertion",
			verifySQL: "-- onwardpg:assert expected_product_effect\nSELECT false;\n",
			wantCode:  "assertion_false",
		},
		{
			name:       "cancellation",
			phaseSQL:   "-- onwardpg:batch transactional\nSELECT pg_sleep(30);\n",
			wantCode:   "transactional_batch_failed",
			cancelSoon: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
			ddl := "CREATE SCHEMA app; CREATE TABLE app.items (id bigint PRIMARY KEY);\n"
			writeTestFile(t, repository, "schema.sql", ddl)
			writeHistoryFixture(t, repository, url, "genesis", ddl)
			bundlePath := filepath.Join(repository, "onward-bundles", "primary", "genesis")
			if test.phaseSQL != "" {
				writeTestFile(t, bundlePath, "phases/expand.sql", test.phaseSQL)
			}
			if test.verifySQL != "" {
				writeTestFile(t, bundlePath, "verify.sql", test.verifySQL)
			}
			edited, err := bundle.PrepareEdited(bundlePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := bundle.InstallReceipts(bundlePath, edited); err != nil {
				t.Fatal(err)
			}
			chain, err := history.Load(repository, "onward-bundles", "primary")
			if err != nil {
				t.Fatal(err)
			}
			before := disposableDatabaseCount(t, url)
			ctx := context.Background()
			if test.cancelSoon {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			report, err := verify.Run(ctx, verify.Input{AdminURL: url, Chain: chain, BundleID: "genesis"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Outcome != "failed" || report.Failure == nil || report.Failure.Code != test.wantCode || len(report.Findings) != 1 || report.Findings[0].Code != test.wantCode || report.Failure.Remediation == "" {
				t.Fatalf("failure report = %#v", report)
			}
			if after := disposableDatabaseCount(t, url); after != before {
				t.Fatalf("disposable database count after failure = %d, want %d", after, before)
			}
		})
	}
}

func TestVerifyRejectsHistoryReceiptedForAnotherPostgresMajor(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	ddl := "CREATE TABLE items (id bigint PRIMARY KEY);\n"
	writeHistoryFixture(t, repository, url, "genesis", ddl)
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := source.PostgresMajor(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	different := 14
	if actual == different {
		different = 15
	}
	chain.Entries[0].Artifact.Manifest.DesiredSource.PostgresMajor = different
	before := disposableDatabaseCount(t, url)
	_, err = verify.Run(context.Background(), verify.Input{AdminURL: url, Chain: chain, BundleID: "genesis"})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("targets PostgreSQL %d but the scratch server is PostgreSQL %d", different, actual)) {
		t.Fatalf("major mismatch error = %v", err)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("major mismatch created a disposable database: before=%d after=%d", before, after)
	}
}

func writeHistoryFixture(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-history", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "empty history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func writeHistoryTransitionFixture(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := history.Load(root, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-transition-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture transition", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func writeHistoryContractDropFixture(t *testing.T, root, devURL, id string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := history.Load(root, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-desired", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) == 0 {
		t.Fatalf("drop planning did not ask for destructive intent: %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint,
		DesiredFingerprint: pending.DesiredFingerprint,
	}
	for _, question := range pending.Questions {
		if len(question.Choices) == 0 {
			t.Fatalf("drop question has no explicit choice: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{
			Kind: question.Kind, Key: question.Key, Value: question.Choices[0],
			QuestionFingerprint: question.ScopeFingerprint,
		})
	}
	result, err := graphplan.Build(current, desired, answers, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned {
		t.Fatalf("drop result = %#v", result)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "empty fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result, Answers: &answers})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func disposableDatabaseCount(t *testing.T, url string) int {
	t.Helper()
	connection, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var count int
	if err := connection.QueryRow(context.Background(), `SELECT count(*) FROM pg_database WHERE datname LIKE 'onwardpg_verify_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func randomToken(t *testing.T) string {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(random)
}

func createTestDatabase(t *testing.T, adminURL string) (string, func()) {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "onwardpg_dev_test_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+`"`+name+`"`); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String(), func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+`"`+name+`" WITH (FORCE)`)
		_ = admin.Close(context.Background())
	}
}

type captured struct {
	code   int
	stdout string
}

func captureStdout(t *testing.T, call func() int) captured {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	code := call()
	_ = writer.Close()
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	return captured{code: code, stdout: string(data)}
}
