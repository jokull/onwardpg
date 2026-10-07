package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/contractcheck"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/verify"
)

const nontransactionalHistoryConfig = `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`

// applyBundlePhase applies one phase of a bundle to a caller database the way
// a deployment runner does. It does not use the replay of onwardpg, so the
// database that a test compares with replayed history is built independently.
//
// A generated bundle runs the batches of plan.json: a transactional batch in
// one transaction, and each statement of a non-transactional batch alone,
// outside a transaction. An edited bundle runs the chunks of its phase file.
func applyBundlePhase(t *testing.T, connection *pgx.Conn, repository, bundleID, phase string) {
	t.Helper()
	ctx := context.Background()
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", bundleID))
	if err != nil {
		t.Fatal(err)
	}
	type unit struct {
		transactional bool
		statements    []string
	}
	var units []unit
	if artifact.Manifest.PhaseSource == "edited" {
		receipt, exists := artifact.Manifest.Phases[phase]
		if !exists {
			return
		}
		chunks, err := bundle.ParsePhaseSQL(artifact.Files[receipt.Path], receipt.Transactional)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			units = append(units, unit{transactional: chunk.Transactional, statements: []string{chunk.SQL}})
		}
	} else {
		var plan protocol.Result
		if err := json.Unmarshal(artifact.Files["plan.json"], &plan); err != nil {
			t.Fatal(err)
		}
		for _, batch := range plan.Batches {
			if batch.Phase != phase {
				continue
			}
			next := unit{transactional: batch.Transactional}
			for _, statement := range batch.Statements {
				next.statements = append(next.statements, statement.SQL)
			}
			units = append(units, next)
		}
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := connection.PgConn().Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatalf("apply %s %s: %v\n%s", bundleID, phase, err, sql)
		}
	}
	for _, next := range units {
		if next.transactional {
			exec("BEGIN")
		}
		for _, statement := range next.statements {
			exec(statement)
		}
		if next.transactional {
			exec("COMMIT")
		}
	}
}

func driftReport(t *testing.T, repository, liveURL string) (int, driftcheck.Report) {
	t.Helper()
	output := captureStdout(t, func() int {
		return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
	})
	var report driftcheck.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil || report.Outcome == "" || report.Outcome == "error" {
		t.Fatalf("drift check exit = %d, stdout = %s", output.code, output.stdout)
	}
	return output.code, report
}

func requireDriftFree(t *testing.T, repository, liveURL string) {
	t.Helper()
	code, report := driftReport(t, repository, liveURL)
	if code != 0 || report.Outcome != "drift_free" || len(report.Differences) != 0 || report.ExpectedFingerprint != report.ActualFingerprint {
		t.Fatalf("drift check exit = %d, report = %#v", code, report)
	}
}

func verifyReport(t *testing.T, repository string, arguments ...string) (int, verify.Report) {
	t.Helper()
	output := captureStdout(t, func() int {
		return runVerifyAt(append([]string{"--target", "primary"}, arguments...), repository)
	})
	var report verify.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatalf("verify exit = %d, stdout = %s", output.code, output.stdout)
	}
	return output.code, report
}

// A bundle that builds and drops indexes concurrently has batches that
// PostgreSQL refuses in a transaction block. Each command that replays
// accepted history must run them as verification does. Before the shared
// replay, drift check and the base replay of plan and draft sent the complete
// history as one query, which PostgreSQL runs in one implicit transaction, and
// failed with "CREATE INDEX CONCURRENTLY cannot run inside a transaction
// block" as soon as such a bundle was in history.
func TestHistoryWithConcurrentIndexBundleReplaysInEveryCommandOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	t.Setenv("ONWARDPG_TEST_CONCURRENT_LIVE_URL", liveURL)
	live, err := pgx.Connect(ctx, liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	before := disposableDatabaseCount(t, adminURL)

	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", nontransactionalHistoryConfig)
	table := "CREATE SCHEMA app;\nCREATE TABLE app.users (id bigint PRIMARY KEY, email text, name text, created_at timestamptz);\n"
	writeTestFile(t, repository, "schema.sql", table+
		"CREATE INDEX users_email_idx ON app.users (email);\n"+
		"CREATE INDEX users_old_idx ON app.users (name);\n")
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}

	// One bundle with every concurrent statement the planner emits: two new
	// indexes (one non-transactional batch with two statements), a same-name
	// replacement (a transactional rename, then a concurrent build), and two
	// concurrent drops in contract.
	replaced := table +
		"CREATE INDEX users_email_idx ON app.users (email, name);\n" +
		"CREATE INDEX users_created_idx ON app.users (created_at);\n" +
		"CREATE INDEX users_pair_idx ON app.users (id, created_at);\n"
	writeTestFile(t, repository, "schema.sql", replaced)
	planned := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{
			"concurrent-indexes", "--target", "primary", "--concurrent-indexes",
			"--hint", `{"kind":"drop","object":"index","name":["app","users","users_old_idx"]}`,
		}, repository)
	})
	if planned.code != 0 {
		t.Fatalf("plan --concurrent-indexes = %d, %s", planned.code, planned.stdout)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "concurrent-indexes"))
	if err != nil {
		t.Fatal(err)
	}
	expand, contract := string(artifact.Files["phases/expand.sql"]), string(artifact.Files["phases/contract.sql"])
	if strings.Count(expand, "CREATE INDEX CONCURRENTLY") != 3 || strings.Count(contract, "DROP INDEX CONCURRENTLY") != 2 ||
		!strings.Contains(expand, "-- onwardpg:batch transactional") || !strings.Contains(expand, "-- onwardpg:batch nontransactional") {
		t.Fatalf("the plan does not hold the concurrent statements this test needs:\n%s\n%s", expand, contract)
	}
	var plan protocol.Result
	if err := json.Unmarshal(artifact.Files["plan.json"], &plan); err != nil {
		t.Fatal(err)
	}
	largest := 0
	for _, batch := range plan.Batches {
		if !batch.Transactional {
			largest = max(largest, len(batch.Statements))
		}
	}
	if largest < 2 {
		t.Fatalf("no non-transactional batch holds two statements: %#v", plan.Batches)
	}

	if code, report := verifyReport(t, repository, "--bundle", "concurrent-indexes"); code != 0 || report.Outcome != "verified" {
		t.Fatalf("verify = %d, %#v", code, report)
	}
	if code, report := verifyReport(t, repository, "--bundle", "concurrent-indexes", "--check"); code != 0 || report.Outcome != "verified" {
		t.Fatalf("verify --check = %d, %#v", code, report)
	}
	if code, report := verifyReport(t, repository, "--bundle", "concurrent-indexes", "--through", "expand"); code != 0 || report.Outcome != "partial_verified" {
		t.Fatalf("verify --through expand = %d, %#v", code, report)
	}
	if status := captureStdout(t, func() int {
		return runHistoryStatusAt([]string{"status", "--target", "primary"}, repository)
	}); status.code != 0 || !strings.Contains(status.stdout, `"status":"valid"`) {
		t.Fatalf("history status = %d, %s", status.code, status.stdout)
	}

	// The live database has expand only. Drift check must replay the history
	// and report exactly the work that contract still has to do.
	applyBundlePhase(t, live, repository, "baseline", "expand")
	applyBundlePhase(t, live, repository, "concurrent-indexes", "expand")
	code, report := driftReport(t, repository, liveURL)
	if code != 4 || report.Outcome != "drifted" || len(report.Differences) != 2 {
		t.Fatalf("drift check between expand and contract = %d, %#v", code, report)
	}
	for _, difference := range report.Differences {
		if difference.Kind != "unexpected_in_actual" || !strings.Contains(difference.ObjectID, "onwardpg_tmpidx_") && !strings.Contains(difference.ObjectID, "users_old_idx") {
			t.Fatalf("drift check reported more than the pending contract work: %#v", report.Differences)
		}
	}

	// Contract check reads the receipted checkpoint and the live catalog.
	manifest := artifact.Manifest
	now := time.Now().UTC()
	evidence := contractcheck.Evidence{
		Target: "primary", Environment: "test", PlanID: manifest.PlanID, BundleEntryDigest: manifest.History.EntryDigest,
		DesiredFingerprint: manifest.DesiredSource.Fingerprint, Generation: manifest.Generation, Release: "isolated-test",
		ObservedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	for _, category := range []string{"web", "workers", "scheduled_jobs", "queues", "connection_pools", "previews", "ad_hoc_writers"} {
		evidence.Cohorts = append(evidence.Cohorts, contractcheck.Cohort{Category: category, Name: category, Status: "isolated", SourceKind: "manual", Source: "disposable regression database"})
	}
	evidenceBody, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(t.TempDir(), "readiness.json")
	if err := os.WriteFile(evidencePath, evidenceBody, 0o600); err != nil {
		t.Fatal(err)
	}
	readiness := captureStdout(t, func() int {
		return runContractAt([]string{
			"check", "--target", "primary", "--bundle", "concurrent-indexes", "--environment", "test",
			"--database-env", "ONWARDPG_TEST_CONCURRENT_LIVE_URL", "--evidence", evidencePath,
		}, repository)
	})
	var ready contractcheck.Report
	if err := json.Unmarshal([]byte(readiness.stdout), &ready); err != nil || readiness.code != 0 || ready.Status != "ready" {
		t.Fatalf("contract check = %d, %s", readiness.code, readiness.stdout)
	}

	applyBundlePhase(t, live, repository, "concurrent-indexes", "contract")
	requireDriftFree(t, repository, liveURL)

	// A real difference is still found: the index that the bundle built
	// concurrently is expected in the live database.
	if _, err := live.Exec(ctx, "DROP INDEX app.users_created_idx"); err != nil {
		t.Fatal(err)
	}
	code, report = driftReport(t, repository, liveURL)
	if code != 4 || report.Outcome != "drifted" || len(report.Differences) != 1 ||
		report.Differences[0].Kind != "missing_in_actual" || !strings.Contains(report.Differences[0].ObjectID, "users_created_idx") {
		t.Fatalf("drift check after a manual index drop = %d, %#v", code, report)
	}
	if _, err := live.Exec(ctx, "CREATE INDEX users_created_idx ON app.users (created_at)"); err != nil {
		t.Fatal(err)
	}
	requireDriftFree(t, repository, liveURL)

	// The bundle is accepted. A checkout without the local plan anchor starts
	// the next feature, whose base is the history with the concurrent bundle.
	if err := os.RemoveAll(filepath.Join(repository, ".onwardpg")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", strings.Replace(replaced, "created_at timestamptz", "created_at timestamptz, age integer", 1))
	if next := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"add-age", "--target", "primary"}, repository)
	}); next.code != 0 {
		t.Fatalf("plan after a concurrent bundle = %d, %s", next.code, next.stdout)
	}
	// The explicit draft command replays the same base.
	redrafted := captureStdout(t, func() int {
		return runTestDraftAt(t, []string{"--target", "primary", "--bundle", "add-age", "--after", "concurrent-indexes"}, repository)
	})
	var draft draftflow.Report
	if err := json.Unmarshal([]byte(redrafted.stdout), &draft); err != nil || redrafted.code != 0 || draft.Outcome != string(protocol.Planned) || draft.BaseBundle != "concurrent-indexes" {
		t.Fatalf("draft after a concurrent bundle = %d, %s", redrafted.code, redrafted.stdout)
	}
	if code, report := verifyReport(t, repository, "--bundle", "add-age", "--check"); code != 0 || report.Outcome != "verified" {
		t.Fatalf("verify --check of the next bundle = %d, %#v", code, report)
	}
	applyBundlePhase(t, live, repository, "add-age", "expand")
	applyBundlePhase(t, live, repository, "add-age", "contract")
	requireDriftFree(t, repository, liveURL)

	if after := disposableDatabaseCount(t, adminURL); after != before {
		t.Fatalf("disposable database count = %d, want %d", after, before)
	}
}

// An edited phase is replayed in the chunks that its batch directives give,
// and one connection replays the complete history. The baseline here sets
// search_path, and the edited bundle after it uses unqualified names, so it
// resolves only if that session state is still set. Verification already
// worked like this; drift check and the base replay of draft must give the
// same catalog.
func TestEditedConcurrentBundleAndSessionStateReplayAsVerifiedOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	live, err := pgx.Connect(ctx, liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())

	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", nontransactionalHistoryConfig)
	table := "CREATE SCHEMA app;\nSET search_path = app;\nCREATE TABLE users (id bigint PRIMARY KEY, email text, name text);\n"
	writeTestFile(t, repository, "schema.sql", table)
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	indexed := table + "CREATE INDEX users_email_idx ON users (email);\nCREATE INDEX users_name_idx ON users (name);\n"
	writeTestFile(t, repository, "schema.sql", indexed)
	if planned := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"two-indexes", "--target", "primary", "--concurrent-indexes"}, repository)
	}); planned.code != 0 {
		t.Fatalf("plan --concurrent-indexes = %d, %s", planned.code, planned.stdout)
	}
	bundlePath := filepath.Join(repository, "onward-bundles", "primary", "two-indexes")
	generated, err := os.ReadFile(filepath.Join(bundlePath, "phases", "expand.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(generated), "-- onwardpg:batch nontransactional") != 1 || strings.Count(string(generated), "CREATE INDEX CONCURRENTLY") != 2 {
		t.Fatalf("generated expand phase is not one batch with two concurrent builds:\n%s", generated)
	}

	// An edit that leaves both statements in one chunk cannot run: onwardpg
	// sends a chunk as written, and PostgreSQL runs a query with two
	// statements in one implicit transaction. The report must say what to do.
	unqualified := strings.ReplaceAll(string(generated), `ON "app"."users"`, "ON users")
	if unqualified == string(generated) {
		t.Fatalf("generated expand phase has no qualified table name to edit:\n%s", generated)
	}
	writeTestFile(t, bundlePath, "phases/expand.sql", unqualified)
	code, failed := verifyReport(t, repository, "--bundle", "two-indexes")
	if code == 0 || failed.Outcome != "failed" || failed.Failure == nil || failed.Failure.Code != "non_transactional_batch_failed" ||
		!strings.Contains(failed.Failure.Message, "25001") || !strings.Contains(failed.Failure.Remediation, `its own "-- onwardpg:batch nontransactional" line`) {
		t.Fatalf("verify of two concurrent builds in one edited chunk = %d, %#v", code, failed)
	}

	// One directive for each statement gives two chunks.
	lines := strings.SplitAfter(unqualified, "\n")
	var edited strings.Builder
	builds := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "CREATE INDEX CONCURRENTLY") {
			builds++
			if builds == 2 {
				edited.WriteString("-- onwardpg:batch nontransactional\n")
			}
		}
		edited.WriteString(line)
	}
	writeTestFile(t, bundlePath, "phases/expand.sql", edited.String())
	if code, report := verifyReport(t, repository, "--bundle", "two-indexes"); code != 0 || report.Outcome != "verified" || report.SelectedBatches != 2 {
		t.Fatalf("verify of the edited bundle = %d, %#v", code, report)
	}
	artifact, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.PhaseSource != "edited" {
		t.Fatalf("phase source = %q, want edited", artifact.Manifest.PhaseSource)
	}

	// The live database gets the same SQL on one session.
	applyBundlePhase(t, live, repository, "baseline", "expand")
	applyBundlePhase(t, live, repository, "two-indexes", "expand")
	requireDriftFree(t, repository, liveURL)

	// The replay of drift check and the executions of verification give one
	// catalog.
	_, drift := driftReport(t, repository, liveURL)
	_, verified := verifyReport(t, repository, "--bundle", "two-indexes", "--check")
	if verified.Outcome != "verified" || drift.ExpectedFingerprint != verified.ObservedFingerprint || drift.ExpectedFingerprint != artifact.Manifest.DesiredSource.Fingerprint {
		t.Fatalf("replayed fingerprint %s, verification %#v, receipt %s", drift.ExpectedFingerprint, verified, artifact.Manifest.DesiredSource.Fingerprint)
	}

	if err := os.RemoveAll(filepath.Join(repository, ".onwardpg")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", strings.Replace(indexed, "name text", "name text, age integer", 1))
	if next := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"add-age", "--target", "primary"}, repository)
	}); next.code != 0 {
		t.Fatalf("plan after an edited concurrent bundle = %d, %s", next.code, next.stdout)
	}
	applyBundlePhase(t, live, repository, "add-age", "expand")
	applyBundlePhase(t, live, repository, "add-age", "contract")
	requireDriftFree(t, repository, liveURL)
}
