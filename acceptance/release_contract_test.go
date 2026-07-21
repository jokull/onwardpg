package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/testkit"
)

const (
	acceptanceEnabledEnv  = "ONWARDPG_ACCEPTANCE"
	acceptanceDatabaseEnv = "ONWARDPG_ACCEPTANCE_DATABASE_URL"
)

var acceptanceBinary testkit.Binary

func TestMain(m *testing.M) {
	if os.Getenv(acceptanceEnabledEnv) != "1" {
		os.Exit(m.Run())
	}
	if os.Getenv(acceptanceDatabaseEnv) == "" {
		fmt.Fprintf(os.Stderr, "%s=1 requires %s; acceptance must not silently skip\n", acceptanceEnabledEnv, acceptanceDatabaseEnv)
		os.Exit(2)
	}
	if expected := os.Getenv("ONWARDPG_EXPECTED_POSTGRES_MAJOR"); expected != "" {
		want, err := strconv.Atoi(expected)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid ONWARDPG_EXPECTED_POSTGRES_MAJOR %q: %v\n", expected, err)
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		actual, err := testkit.NativePostgresMajor(ctx, os.Getenv(acceptanceDatabaseEnv))
		cancel()
		if err != nil || actual != want {
			fmt.Fprintf(os.Stderr, "acceptance PostgreSQL major = %d, want %d: %v\n", actual, want, err)
			os.Exit(2)
		}
	}
	root := repositoryRoot()
	directory, err := os.MkdirTemp("", "onwardpg-acceptance-binary-")
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	acceptanceBinary, err = testkit.BuildBinary(ctx, root, filepath.Join(directory, "onwardpg"))
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(directory)
		os.Exit(2)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func TestReleaseContractNullableColumn(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	baseline := []byte(`CREATE SCHEMA app;
CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  guest_name text NOT NULL
);
`)
	desired := []byte(`CREATE SCHEMA app;
CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  guest_name text NOT NULL,
  note text
);
`)
	clients := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy booking client", Prepared: []testkit.PreparedAction{
			{Name: "legacy_insert", SQL: `INSERT INTO app.bookings (id, guest_name) VALUES ($1, $2)`},
			{Name: "legacy_read", SQL: `SELECT guest_name FROM app.bookings WHERE id = $1`},
		}},
		New: testkit.ClientContract{Name: "new booking client", Actions: []testkit.ClientAction{
			{Name: "insert note", SQL: `INSERT INTO app.bookings (id, guest_name, note) VALUES ($1, $2, $3) RETURNING id`, Arguments: []any{int64(3), "Lin", "late arrival"}, ExpectedRow: []any{int64(3)}},
			{Name: "read note", SQL: `SELECT guest_name, note FROM app.bookings WHERE id = $1`, Arguments: []any{int64(3)}, ExpectedRow: []any{"Lin", "late arrival"}},
		}},
		Final: testkit.ClientContract{Name: "final booking client", Actions: []testkit.ClientAction{
			{Name: "count final rows", SQL: `SELECT count(*) FROM app.bookings`, ExpectedRow: []any{int64(3)}},
		}},
	}
	requireReleaseClientContracts(t, clients)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")

	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_nullable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	legacy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { legacy.Close(context.Background()) })
	if err := testkit.PrepareAll(ctx, legacy, clients.Legacy.Prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_insert", int64(1), "Ada"); err != nil {
		t.Fatalf("legacy baseline insert: %v", err)
	}
	if err := workspace.WriteSchema(desired); err != nil {
		t.Fatal(err)
	}
	planned := runOK(t, ctx, workspace.Root, environment, "plan", "booking-note", "--target", "app", "--output", "json")
	var report testkit.PlanEnvelope
	if err := planned.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" || report.Durable.Status != "planned" || report.Durable.PlanID == "" || report.Durable.Generation < 1 {
		t.Fatalf("unexpected plan envelope: %#v\n%s", report, planned.Failure())
	}
	if len(report.Durable.Edits) != 0 {
		t.Fatalf("nullable addition unexpectedly requires edits: %#v", report.Durable.Edits)
	}
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "booking-note")

	deploy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deploy.Close(context.Background()) })
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath("booking-note", "expand")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_insert", int64(2), "Grace"); err != nil {
		t.Fatalf("prepared legacy insert after expand: %v", err)
	}
	if err := testkit.ExpectRow(ctx, legacy, "legacy_read", []any{"Grace"}, int64(2)); err != nil {
		t.Fatalf("prepared legacy read after expand: %v", err)
	}
	modern, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { modern.Close(context.Background()) })
	if err := testkit.RunClientActions(ctx, modern, clients.New.Actions); err != nil {
		t.Fatalf("new client after expand: %v", err)
	}
	contractPath := workspace.PhasePath("booking-note", "contract")
	if body, err := os.ReadFile(contractPath); err == nil && strings.TrimSpace(string(body)) != "" {
		t.Fatalf("nullable addition should have no contract SQL:\n%s", body)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	residual, err := testkit.AssertZeroResidual(ctx, adminURL, desired, workload.Config())
	if err != nil {
		t.Fatal(err)
	}
	if !residual.Converged || residual.Passes != 2 {
		t.Fatalf("zero-drift report = %#v", residual)
	}
	if err := testkit.RunClientActions(ctx, modern, clients.Final.Actions); err != nil {
		t.Fatalf("final client: %v", err)
	}
}

func TestReleaseContractRequiredColumnWithReviewedCleanup(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	baseline := []byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY);\n")
	desired := []byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY, status text NOT NULL);\n")
	clients := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy required-column client", Prepared: []testkit.PreparedAction{
			{Name: "legacy_required_insert", SQL: `INSERT INTO app.bookings (id) VALUES ($1)`},
		}},
		New: testkit.ClientContract{Name: "new required-column client", Actions: []testkit.ClientAction{
			{Name: "insert explicit status", SQL: `INSERT INTO app.bookings (id, status) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(3), "confirmed"}, ExpectedRow: []any{int64(3)}},
		}},
		Final: testkit.ClientContract{Name: "final required-column client", Actions: []testkit.ClientAction{
			{Name: "insert after enforcement", SQL: `INSERT INTO app.bookings (id, status) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(4), "pending"}, ExpectedRow: []any{int64(4)}},
			{Name: "no null statuses", SQL: `SELECT count(*) FROM app.bookings WHERE status IS NULL`, ExpectedRow: []any{int64(0)}},
		}},
	}
	requireReleaseClientContracts(t, clients)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")

	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_required")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	legacy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.PrepareAll(ctx, legacy, clients.Legacy.Prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_required_insert", int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema(desired); err != nil {
		t.Fatal(err)
	}
	questions := runExit(t, ctx, workspace.Root, environment, 2, "plan", "required-status", "--target", "app", "--output", "json")
	var questionReport testkit.PlanEnvelope
	if err := questions.DecodeJSON(&questionReport); err != nil {
		t.Fatal(err)
	}
	if questionReport.Durable.Status != "needs_input" || len(questionReport.NextActions) == 0 {
		t.Fatalf("required-column questions = %#v", questionReport)
	}
	pending := runExit(t, ctx, workspace.Root, environment, 2,
		"plan", "required-status", "--target", "app", "--output", "json",
		"--hint", `{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}`,
		"--hint", `{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}`,
	)
	var pendingReport testkit.PlanEnvelope
	if err := pending.DecodeJSON(&pendingReport); err != nil {
		t.Fatal(err)
	}
	if pendingReport.Durable.Status != "needs_sql_edits" || len(pendingReport.Durable.Edits) != 1 {
		t.Fatalf("required-column edit handoff = %#v", pendingReport)
	}
	contractPath := workspace.PhasePath("required-status", "contract")
	pocketIDs, err := testkit.PhasePocketIDs(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pocketIDs) != 1 || pendingReport.Durable.Edits[0].PocketID != pocketIDs[0] {
		t.Fatalf("contract pockets = %v, output edits = %#v", pocketIDs, pendingReport.Durable.Edits)
	}
	if err := testkit.EditPhasePocket(contractPath, pocketIDs[0], []byte("UPDATE \"app\".\"bookings\" SET \"status\" = 'pending' WHERE \"status\" IS NULL;\n")); err != nil {
		t.Fatal(err)
	}
	verification := []byte("-- onwardpg:assert booking_status_present\nSELECT NOT EXISTS (SELECT 1 FROM \"app\".\"bookings\" WHERE \"status\" IS NULL);\n")
	if err := os.WriteFile(filepath.Join(workspace.BundlePath("required-status"), "verify.sql"), verification, 0o600); err != nil {
		t.Fatal(err)
	}
	restacked := runOK(t, ctx, workspace.Root, environment, "plan", "--target", "app", "--output", "json")
	var restackedReport testkit.PlanEnvelope
	if err := restacked.DecodeJSON(&restackedReport); err != nil {
		t.Fatal(err)
	}
	if restackedReport.Durable.Status != "planned" || restackedReport.Durable.PlanID != pendingReport.Durable.PlanID || restackedReport.Durable.Generation < pendingReport.Durable.Generation {
		t.Fatalf("plain rerun did not retain edited plan: before=%#v after=%#v", pendingReport.Durable, restackedReport.Durable)
	}
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "required-status")
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "required-status", "--check")

	deploy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath("required-status", "expand")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_required_insert", int64(2)); err != nil {
		t.Fatalf("prepared legacy writer after nullable expand: %v", err)
	}
	modern, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer modern.Close(context.Background())
	if err := testkit.RunClientActions(ctx, modern, clients.New.Actions); err != nil {
		t.Fatalf("new client after expand: %v", err)
	}
	var nulls int
	if err := modern.QueryRow(ctx, `SELECT count(*) FROM app.bookings WHERE status IS NULL`).Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("expanded legacy rows with NULL status = %d, err=%v", nulls, err)
	}
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, contractPath); err != nil {
		t.Fatalf("reviewed contract: %v", err)
	}
	if err := testkit.RunClientActions(ctx, modern, clients.Final.Actions); err != nil {
		t.Fatalf("final client: %v", err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, adminURL, desired, workload.Config()); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseContractSameTypeRename(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	baseline := []byte("CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, display_name text NOT NULL);\n")
	desired := []byte("CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, full_name text NOT NULL);\n")
	clients := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy rename client", Prepared: []testkit.PreparedAction{
			{Name: "legacy_name_insert", SQL: `INSERT INTO app.accounts (id, display_name) VALUES ($1, $2)`},
			{Name: "legacy_name_read", SQL: `SELECT display_name FROM app.accounts WHERE id = $1`},
		}},
		New: testkit.ClientContract{Name: "new rename client", Actions: []testkit.ClientAction{
			{Name: "insert new name", SQL: `INSERT INTO app.accounts (id, full_name) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(3), "Lin"}, ExpectedRow: []any{int64(3)}},
			{Name: "read both names", SQL: `SELECT display_name, full_name FROM app.accounts WHERE id = $1`, Arguments: []any{int64(3)}, ExpectedRow: []any{"Lin", "Lin"}},
			{Name: "reject contradictory names", SQL: `INSERT INTO app.accounts (id, display_name, full_name) VALUES ($1, $2, $3)`, Arguments: []any{int64(4), "Old", "New"}, ExpectedSQLState: "23514"},
		}},
		Final: testkit.ClientContract{Name: "final rename client", Actions: []testkit.ClientAction{
			{Name: "insert final name", SQL: `INSERT INTO app.accounts (id, full_name) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(5), "Katherine"}, ExpectedRow: []any{int64(5)}},
			{Name: "reject legacy name", SQL: `INSERT INTO app.accounts (id, display_name) VALUES ($1, $2)`, Arguments: []any{int64(6), "obsolete"}, ExpectedSQLState: "42703"},
		}},
	}
	requireReleaseClientContracts(t, clients)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")
	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_rename")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	legacy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.PrepareAll(ctx, legacy, clients.Legacy.Prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_name_insert", int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema(desired); err != nil {
		t.Fatal(err)
	}
	planned := runOK(t, ctx, workspace.Root, environment,
		"plan", "rename-display-name", "--target", "app", "--output", "json",
		"--hint", `{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}`,
		"--hint", `{"kind":"rename_backfill","name":["app","accounts","display_name"],"strategy":"single_transaction"}`,
	)
	var report testkit.PlanEnvelope
	if err := planned.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	if report.Durable.Status != "planned" || report.Durable.PlanID == "" {
		t.Fatalf("rename plan = %#v", report)
	}
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "rename-display-name")
	deploy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath("rename-display-name", "expand")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_name_insert", int64(2), "Grace"); err != nil {
		t.Fatalf("legacy writer after rename expand: %v", err)
	}
	if err := testkit.ExpectRow(ctx, legacy, "legacy_name_read", []any{"Grace"}, int64(2)); err != nil {
		t.Fatalf("legacy read after rename expand: %v", err)
	}
	modern, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer modern.Close(context.Background())
	if err := testkit.RunClientActions(ctx, modern, clients.New.Actions); err != nil {
		t.Fatalf("new client after rename expand: %v", err)
	}
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath("rename-display-name", "contract")); err != nil {
		t.Fatal(err)
	}
	if err := testkit.RunClientActions(ctx, modern, clients.Final.Actions); err != nil {
		t.Fatalf("final client after rename contract: %v", err)
	}
	if err := testkit.AssertNoCompatibilityArtifacts(ctx, modern, "onwardpg_sync_column_%"); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, adminURL, desired, workload.Config()); err != nil {
		t.Fatal(err)
	}
}

func runOK(t *testing.T, ctx context.Context, directory string, environment map[string]string, arguments ...string) testkit.CommandResult {
	t.Helper()
	result := acceptanceBinary.Run(ctx, directory, environment, arguments...)
	if result.ExitCode != 0 {
		t.Fatalf("command failed:\n%s", result.Failure())
	}
	return result
}

func runExit(t *testing.T, ctx context.Context, directory string, environment map[string]string, exitCode int, arguments ...string) testkit.CommandResult {
	t.Helper()
	result := acceptanceBinary.Run(ctx, directory, environment, arguments...)
	if result.ExitCode != exitCode {
		t.Fatalf("command exit = %d, want %d:\n%s", result.ExitCode, exitCode, result.Failure())
	}
	return result
}

func trackAcceptanceWorkspace(t *testing.T, workspace *testkit.Workspace) {
	t.Helper()
	t.Cleanup(func() {
		if err := testkit.RecordWorkspace(workspace.Root, t.Name()); err != nil {
			t.Errorf("record acceptance workspace: %v", err)
		}
	})
}

func requireReleaseClientContracts(t *testing.T, contracts testkit.ReleaseClientContracts) {
	t.Helper()
	if err := contracts.Validate(); err != nil {
		t.Fatalf("invalid authoritative client contracts: %v", err)
	}
}

func requireAcceptance(t *testing.T) {
	t.Helper()
	if os.Getenv(acceptanceEnabledEnv) != "1" {
		t.Skip("set ONWARDPG_ACCEPTANCE=1 to run native release acceptance")
	}
}

func repositoryRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate acceptance package")
	}
	return filepath.Dir(filepath.Dir(file))
}
