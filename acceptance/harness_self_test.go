package acceptance_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/testkit"
)

func TestHarnessRejectsIncompleteClientContract(t *testing.T) {
	contracts := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy", Prepared: []testkit.PreparedAction{{Name: "old", SQL: "SELECT 1"}}},
		New:    testkit.ClientContract{Name: "new", Actions: []testkit.ClientAction{{Name: "read", SQL: "SELECT 1", ExpectedRow: []any{int32(1)}}}},
	}
	if err := contracts.Validate(); err == nil || !strings.Contains(err.Error(), "final client contract") {
		t.Fatalf("incomplete contracts error = %v", err)
	}
}

func TestHarnessRejectsAmbiguousClientExpectation(t *testing.T) {
	contracts := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy", Prepared: []testkit.PreparedAction{{Name: "old", SQL: "SELECT 1"}}},
		New: testkit.ClientContract{Name: "new", Actions: []testkit.ClientAction{{
			Name: "ambiguous", SQL: "SELECT 1", ExpectedRow: []any{int32(1)}, ExpectedSQLState: "23514",
		}}},
		Final: testkit.ClientContract{Name: "final", Actions: []testkit.ClientAction{{Name: "read", SQL: "SELECT 1"}}},
	}
	if err := contracts.Validate(); err == nil || !strings.Contains(err.Error(), "cannot expect both") {
		t.Fatalf("ambiguous contracts error = %v", err)
	}
}

func TestHarnessZeroDriftRejectsUnmodeledFinalDifference(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_oracle_bad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean zero-drift workload: %v", err)
		}
	})
	desired := []byte("CREATE SCHEMA app; CREATE TABLE app.items (id bigint PRIMARY KEY);\n")
	if err := workload.Apply(ctx, append(desired, []byte("COMMENT ON TABLE app.items IS 'unexpected';\n")...)); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, adminURL, desired, workload.Config()); err == nil || !strings.Contains(err.Error(), "nonempty semantic residual") {
		t.Fatalf("zero-drift oracle accepted extra comment: %v", err)
	}
}

func TestHarnessPhaseRunnerRejectsBrokenSQL(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workload, err := testkit.NewPostgres(ctx, os.Getenv(acceptanceDatabaseEnv), "accept_phase_bad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean broken-phase workload: %v", err)
		}
	})
	connection, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	path := t.TempDir() + "/expand.sql"
	if err := os.WriteFile(path, []byte("-- onwardpg:batch transactional\nCREATE TABL broken;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, connection, path); err == nil {
		t.Fatal("phase runner accepted deliberately broken SQL")
	}
}

func TestHarnessPhaseParserDefaultsUnmarkedFilesTransactional(t *testing.T) {
	path := t.TempDir() + "/expand.sql"
	if err := os.WriteFile(path, []byte("CREATE TABLE app.example (id bigint);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	batches, err := testkit.ReadPhaseBatches(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || !batches[0].Transactional {
		t.Fatalf("unmarked phase batches = %#v, want one transactional fallback", batches)
	}
}

func TestHarnessMixedPhasePreservesCommittedBoundariesAndRollsBackFailedBatch(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workload, err := testkit.NewPostgres(ctx, os.Getenv(acceptanceDatabaseEnv), "accept_phase_mixed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean mixed-phase workload: %v", err)
		}
	})
	connection, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	path := t.TempDir() + "/expand.sql"
	body := []byte(`-- onwardpg:batch transactional
CREATE TABLE committed_transactional (id bigint);
-- onwardpg:batch nontransactional
CREATE TABLE committed_nontransactional (id bigint);
-- onwardpg:batch transactional
CREATE TABLE rolled_back_transactional (id bigint);
SELECT 1 / 0;
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ApplyPhaseFile(ctx, connection, path); err == nil {
		t.Fatal("mixed phase unexpectedly succeeded")
	}
	var committedTransactional, committedNontransactional, rolledBack bool
	if err := connection.QueryRow(ctx, `SELECT
to_regclass('committed_transactional') IS NOT NULL,
to_regclass('committed_nontransactional') IS NOT NULL,
to_regclass('rolled_back_transactional') IS NOT NULL`).Scan(
		&committedTransactional, &committedNontransactional, &rolledBack,
	); err != nil {
		t.Fatal(err)
	}
	if !committedTransactional || !committedNontransactional || rolledBack {
		t.Fatalf("mixed phase catalog = transactional:%t nontransactional:%t rolled_back:%t", committedTransactional, committedNontransactional, rolledBack)
	}
}
