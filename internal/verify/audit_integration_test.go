package verify

import (
	"context"
	"os"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
)

func TestManualVerificationRejectsWritesAndNonScalarResults(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "audit_assertions")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE audit_state (value integer); INSERT INTO audit_state VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"WITH changed AS (UPDATE audit_state SET value = 1 RETURNING value) SELECT count(*) = 1 FROM changed",
		"SELECT true UNION ALL SELECT false",
		"SELECT 'true'::text",
	} {
		t.Run(query, func(t *testing.T) {
			if _, err := conn.Exec(ctx, "UPDATE audit_state SET value = 0"); err != nil {
				t.Fatal(err)
			}
			batch := protocol.Batch{Statements: []protocol.Statement{{Manual: &protocol.ManualWork{VerificationSQL: []string{query}}}}}
			if err := executeVerification(ctx, conn, batch); err == nil {
				t.Error("accepted an assertion that writes or is not one Boolean row")
			}
			var value int
			if err := conn.QueryRow(ctx, "SELECT value FROM audit_state").Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != 0 {
				t.Fatalf("assertion changed persistent data to %d", value)
			}
		})
	}
}

func TestTransactionalSQLCannotCommitAndRestartItsBatch(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "audit_batch")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, sql := range []string{"SELECT 1; COMMIT;", "SELECT 1; COMMIT; BEGIN;", "SELECT 1; ROLLBACK; BEGIN;"} {
		if err := executeRawBatch(ctx, conn, bundle.SQLBatch{SQL: sql, Transactional: true}); err == nil {
			t.Errorf("accepted transaction control in transactional batch: %s", sql)
		}
	}
	for _, sql := range []string{"SELECT 1; COMMIT;", "SELECT 1; COMMIT; BEGIN;"} {
		if err := executeBatch(ctx, conn, protocol.Batch{Transactional: true, Statements: []protocol.Statement{{SQL: sql}}}); err == nil {
			t.Errorf("generated batch accepted transaction control: %s", sql)
		}
	}
	if err := executeRawBatch(ctx, conn, bundle.SQLBatch{SQL: "BEGIN; SELECT 1;", Transactional: false}); err == nil {
		t.Error("accepted unfinished non-transactional batch")
	}
	if conn.PgConn().TxStatus() != 'I' {
		t.Fatal("batch left connection in a transaction")
	}
	if err := executeRawBatch(ctx, conn, bundle.SQLBatch{SQL: "CREATE TABLE batch_state (value integer); INSERT INTO batch_state VALUES (1);", Transactional: true}); err != nil {
		t.Fatal(err)
	}
	if err := executeRawBatch(ctx, conn, bundle.SQLBatch{SQL: "INSERT INTO batch_state VALUES (2); SELECT 1/0;", Transactional: true}); err == nil {
		t.Fatal("expected batch failure")
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM batch_state").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed batch was not rolled back: %d, %v", count, err)
	}

}
