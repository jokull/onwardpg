package acceptance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/testkit"
)

const (
	rehearsalBaseline = `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, display_name text, counter integer NOT NULL DEFAULT 0);
`
	rehearsalDesired = `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, full_name text, counter integer NOT NULL DEFAULT 0);
`
	rehearsalBundle = "rename-operational-rehearsal"
)

func planOperationalRename(t *testing.T, ctx context.Context) (*testkit.Workspace, *testkit.Postgres) {
	t.Helper()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	if err := workspace.WriteSchema([]byte(rehearsalBaseline)); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, env, "config", "check")
	runOK(t, ctx, workspace.Root, env, "init", "--target", "app", "--bundle", "baseline")
	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_operational_rename")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean operational workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, []byte(rehearsalBaseline)); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema([]byte(rehearsalDesired)); err != nil {
		t.Fatal(err)
	}
	planned := runOK(t, ctx, workspace.Root, env,
		"plan", rehearsalBundle, "--target", "app", "--output", "json",
		"--hint", `{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}`,
		"--hint", `{"kind":"rename_backfill","name":["app","accounts","display_name"],"strategy":"single_transaction"}`)
	var report testkit.PlanEnvelope
	if err := planned.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	if report.Durable.Status != "planned" || report.Durable.PlanID == "" || len(report.Durable.Edits) != 0 {
		t.Fatalf("operational rename plan = %#v", report.Durable)
	}
	runOK(t, ctx, workspace.Root, env, "verify", "--target", "app", "--bundle", rehearsalBundle)
	return workspace, workload
}

func rehearsalConnect(t *testing.T, ctx context.Context, workload *testkit.Postgres) *pgx.Conn {
	t.Helper()
	conn, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestReleaseOperationalRenameWorkload(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workspace, workload := planOperationalRename(t, ctx)
	seed := rehearsalConnect(t, ctx, workload)
	if _, err := seed.Exec(ctx, `INSERT INTO app.accounts (id, display_name)
SELECT id, CASE WHEN id = 1 THEN NULL ELSE 'seed-' || id END FROM generate_series(1, 64) AS id`); err != nil {
		t.Fatal(err)
	}
	legacy := rehearsalConnect(t, ctx, workload)
	modern := rehearsalConnect(t, ctx, workload)
	if err := testkit.PrepareAll(ctx, legacy, []testkit.PreparedAction{
		{Name: "old_update", SQL: `UPDATE app.accounts SET display_name = $2, counter = counter + 1 WHERE id = $1`},
		{Name: "old_read", SQL: `SELECT display_name FROM app.accounts WHERE id = $1`},
	}); err != nil {
		t.Fatal(err)
	}
	deploy := rehearsalConnect(t, ctx, workload)
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath(rehearsalBundle, "expand")); err != nil {
		t.Fatal(err)
	}
	if err := testkit.PrepareAll(ctx, modern, []testkit.PreparedAction{
		{Name: "new_update", SQL: `UPDATE app.accounts SET full_name = $2, counter = counter + 1 WHERE id = $1`},
		{Name: "new_read", SQL: `SELECT full_name FROM app.accounts WHERE id = $1`},
	}); err != nil {
		t.Fatal(err)
	}
	if err := testkit.ExpectRow(ctx, modern, `SELECT count(*), count(*) FILTER (WHERE full_name IS DISTINCT FROM display_name) FROM app.accounts`, []any{int64(64), int64(0)}); err != nil {
		t.Fatalf("populated backfill: %v", err)
	}
	if err := testkit.ExpectRow(ctx, modern, `SELECT count(*) FROM app.accounts WHERE display_name IS NULL AND full_name IS NULL`, []any{int64(1)}); err != nil {
		t.Fatalf("NULL row backfill: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, writer := range []struct {
		conn              *pgx.Conn
		statement, prefix string
		from, through     int
	}{
		{legacy, "old_update", "old", 1, 32},
		{modern, "new_update", "new", 33, 64},
	} {
		go func(writer struct {
			conn              *pgx.Conn
			statement, prefix string
			from, through     int
		}) {
			<-start
			for id := writer.from; id <= writer.through; id++ {
				if _, err := writer.conn.Exec(ctx, writer.statement, int64(id), fmt.Sprintf("%s-%d", writer.prefix, id)); err != nil {
					results <- fmt.Errorf("%s update row %d: %w", writer.prefix, id, err)
					return
				}
			}
			results <- nil
		}(writer)
	}
	close(start)
	var writerErrors []error
	for range 2 {
		if err := <-results; err != nil {
			writerErrors = append(writerErrors, err)
		}
	}
	if len(writerErrors) != 0 {
		t.Fatal(errors.Join(writerErrors...))
	}
	if err := testkit.ExpectRow(ctx, legacy, "old_read", []any{"new-33"}, int64(33)); err != nil {
		t.Fatalf("old prepared read of new write: %v", err)
	}
	if err := testkit.ExpectRow(ctx, modern, "new_read", []any{"old-3"}, int64(3)); err != nil {
		t.Fatalf("new prepared read of old write: %v", err)
	}
	// Both prepared clients now compete for every row. The final text is
	// deliberately order-dependent, while each row's counter must be exact.
	const contentionRounds = 16
	hotStart := make(chan struct{})
	hotResults := make(chan error, 2)
	for _, writer := range []struct {
		conn              *pgx.Conn
		statement, prefix string
	}{
		{legacy, "old_update", "old-hot"},
		{modern, "new_update", "new-hot"},
	} {
		go func(writer struct {
			conn              *pgx.Conn
			statement, prefix string
		}) {
			<-hotStart
			for round := range contentionRounds {
				for id := 1; id <= 64; id++ {
					if _, err := writer.conn.Exec(ctx, writer.statement, int64(id), fmt.Sprintf("%s-%d-%d", writer.prefix, round, id)); err != nil {
						hotResults <- fmt.Errorf("%s round %d row %d: %w", writer.prefix, round, id, err)
						return
					}
				}
			}
			hotResults <- nil
		}(writer)
	}
	close(hotStart)
	writerErrors = writerErrors[:0]
	for range 2 {
		if err := <-hotResults; err != nil {
			writerErrors = append(writerErrors, err)
		}
	}
	if len(writerErrors) != 0 {
		t.Fatal(errors.Join(writerErrors...))
	}
	const updatesPerRow = int64(1 + 2*contentionRounds)
	if err := testkit.ExpectRow(ctx, seed, `SELECT count(*), count(*) FILTER (WHERE display_name IS DISTINCT FROM full_name), min(counter), max(counter), count(*) FILTER (WHERE counter <> $1) FROM app.accounts`, []any{int64(64), int64(0), int32(updatesPerRow), int32(updatesPerRow), int64(0)}, int32(updatesPerRow)); err != nil {
		t.Fatalf("same-row contention and exact counters: %v", err)
	}
	var row3Name string
	if err := seed.QueryRow(ctx, `SELECT display_name FROM app.accounts WHERE id = 3`).Scan(&row3Name); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, `UPDATE app.accounts SET display_name = NULL WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := modern.Exec(ctx, `UPDATE app.accounts SET full_name = NULL WHERE id = 34`); err != nil {
		t.Fatal(err)
	}
	if err := testkit.ExpectRow(ctx, seed, `SELECT count(*) FROM app.accounts WHERE id IN (2,34) AND display_name IS NULL AND full_name IS NULL`, []any{int64(2)}); err != nil {
		t.Fatalf("NULL propagation: %v", err)
	}
	if _, err := modern.Exec(ctx, `UPDATE app.accounts SET display_name = 'old-conflict', full_name = 'new-conflict' WHERE id = 3`); testkit.SQLState(err) != "23514" {
		t.Fatalf("conflicting dual update SQLSTATE = %q, want 23514: %v", testkit.SQLState(err), err)
	}
	if err := testkit.ExpectRow(ctx, seed, `SELECT display_name, full_name, counter FROM app.accounts WHERE id = 3`, []any{row3Name, row3Name, int32(updatesPerRow)}); err != nil {
		t.Fatalf("conflict changed row: %v", err)
	}
	if err := testkit.ExpectRow(ctx, seed, `SELECT count(*), count(*) FILTER (WHERE display_name IS DISTINCT FROM full_name), sum(counter), count(*) FILTER (WHERE display_name IS NULL) FROM app.accounts`, []any{int64(64), int64(0), int64(64) * updatesPerRow, int64(2)}); err != nil {
		t.Fatalf("overlap data and counter preservation: %v", err)
	}
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath(rehearsalBundle, "contract")); err != nil {
		t.Fatal(err)
	}
	if err := testkit.ExpectRow(ctx, seed, `SELECT count(*), sum(counter), min(counter), max(counter), count(*) FILTER (WHERE full_name IS NULL) FROM app.accounts`, []any{int64(64), int64(64) * updatesPerRow, int32(updatesPerRow), int32(updatesPerRow), int64(2)}); err != nil {
		t.Fatalf("final data and counter preservation: %v", err)
	}
	if err := testkit.ExpectRow(ctx, seed, `SELECT full_name FROM app.accounts WHERE id = 3`, []any{row3Name}); err != nil {
		t.Fatalf("final conflict row preservation: %v", err)
	}
	if err := testkit.AssertNoCompatibilityArtifacts(ctx, seed, "onwardpg_sync_column_%"); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, os.Getenv(acceptanceDatabaseEnv), []byte(rehearsalDesired), workload.Config()); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseOperationalTransactionalLockCancellation(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workspace, workload := planOperationalRename(t, ctx)
	observer := rehearsalConnect(t, ctx, workload)
	blocker := rehearsalConnect(t, ctx, workload)
	if _, err := observer.Exec(ctx, `CREATE TABLE public.rehearsal_marker (id integer PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Exec(ctx, `INSERT INTO app.accounts (id, display_name) VALUES (1, 'original')`); err != nil {
		t.Fatal(err)
	}
	batches, err := testkit.ReadPhaseBatches(workspace.PhasePath(rehearsalBundle, "expand"))
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || !batches[0].Transactional {
		t.Fatalf("expected one transactional generated expand batch, got %#v", batches)
	}
	batch := testkit.PhaseBatch{SQL: "INSERT INTO public.rehearsal_marker VALUES (1);\n" + batches[0].SQL, Transactional: true}
	transaction, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(ctx, `LOCK TABLE app.accounts IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := transaction.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	deploy := rehearsalConnect(t, ctx, workload)
	var deployPID int
	if err := deploy.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&deployPID); err != nil {
		t.Fatal(err)
	}
	attemptCtx, stop := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- testkit.ApplyPhaseBatch(attemptCtx, deploy, batch) }()
	if err := waitForLockWait(ctx, observer, deployPID, blockerPID); err != nil {
		stop()
		<-result
		t.Fatal(err)
	}
	stop()
	if err := <-result; err == nil {
		t.Fatal("cancelled locked phase unexpectedly committed")
	}
	if err := testkit.ExpectRow(ctx, observer, `SELECT count(*) FROM public.rehearsal_marker`, []any{int64(0)}); err != nil {
		t.Fatalf("transaction marker survived cancellation: %v", err)
	}
	if err := testkit.ExpectRow(ctx, observer, `SELECT to_regclass('app.accounts') IS NOT NULL, EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='app' AND table_name='accounts' AND column_name='full_name')`, []any{true, false}); err != nil {
		t.Fatalf("cancelled phase changed schema: %v", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	retry := rehearsalConnect(t, ctx, workload)
	if err := testkit.ApplyPhaseBatch(ctx, retry, batch); err != nil {
		t.Fatalf("retry after lock release: %v", err)
	}
	if err := testkit.ExpectRow(ctx, observer, `SELECT count(*) FROM public.rehearsal_marker`, []any{int64(1)}); err != nil {
		t.Fatalf("retry marker: %v", err)
	}
	if err := testkit.ExpectRow(ctx, observer, `SELECT display_name, full_name FROM app.accounts WHERE id = 1`, []any{"original", "original"}); err != nil {
		t.Fatalf("retry backfill: %v", err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, retry, workspace.PhasePath(rehearsalBundle, "contract")); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Exec(ctx, `DROP TABLE public.rehearsal_marker`); err != nil {
		t.Fatal(err)
	}
	if err := testkit.AssertNoCompatibilityArtifacts(ctx, observer, "onwardpg_sync_column_%"); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, os.Getenv(acceptanceDatabaseEnv), []byte(rehearsalDesired), workload.Config()); err != nil {
		t.Fatal(err)
	}
}

func waitForLockWait(ctx context.Context, observer *pgx.Conn, pid, blockerPID int) error {
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waitType *string
		var blockedByExpected bool
		err := observer.QueryRow(deadline, `SELECT wait_event_type, $2 = ANY(pg_blocking_pids(pid)) FROM pg_stat_activity WHERE pid = $1`, pid, blockerPID).Scan(&waitType, &blockedByExpected)
		if err != nil {
			return err
		}
		if waitType != nil && *waitType == "Lock" && blockedByExpected {
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline.Done():
			return fmt.Errorf("phase backend %d did not wait on a lock: %w", pid, deadline.Err())
		}
	}
}
