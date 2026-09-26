package contractcheck

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
)

func TestBooleanGateCannotEscapeReadOnlyTransaction(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "audit_gate")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.Config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE audit_state (value integer); INSERT INTO audit_state VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	passed, checkErr := queryBoolean(ctx, tx, "SELECT true; COMMIT; UPDATE audit_state SET value = 1;")
	if checkErr == nil && passed {
		t.Error("readiness gate accepted multiple statements")
	}
	_ = tx.Rollback(ctx)
	var value int
	if err := conn.QueryRow(ctx, "SELECT value FROM audit_state").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 0 {
		t.Fatalf("read-only readiness gate committed a write: %d", value)
	}
}

func TestBooleanGateRefusesRowsHiddenByForcedRLS(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "audit_rls")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE audit_state (value integer); INSERT INTO audit_state VALUES (NULL); ALTER TABLE audit_state ENABLE ROW LEVEL SECURITY; ALTER TABLE audit_state FORCE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	passed, checkErr := queryBoolean(ctx, tx, "SELECT NOT EXISTS (SELECT 1 FROM audit_state WHERE value IS NULL)")
	if checkErr == nil && passed {
		t.Fatal("readiness gate ignored an invalid row hidden from the database owner by FORCE RLS")
	}
}
