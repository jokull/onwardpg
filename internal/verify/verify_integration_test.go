package verify

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
)

func TestTransactionalBatchRollsBackWhenManualVerificationFails(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	connection, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	name, err := databaseName()
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.Replace(name, "onwardpg_verify_", "onwardpg_rollback_", 1)
	statement := protocol.Statement{
		SQL: "CREATE SCHEMA " + quote(schema) + "; CREATE TABLE " + quote(schema) + ".example (id bigint);",
		Manual: &protocol.ManualWork{
			Summary: "prove false assertions roll back", ExecutionMode: "transactional",
			Statements: []string{"SELECT 1;"}, VerificationSQL: []string{"SELECT false;"},
		},
	}
	err = executeBatch(context.Background(), connection, protocol.Batch{
		ID: "batch-001", Phase: "manual", Transactional: true, Statements: []protocol.Statement{statement},
	})
	if err == nil || !strings.Contains(err.Error(), "returned false") {
		t.Fatalf("executeBatch error = %v", err)
	}
	var exists bool
	if err := connection.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("schema %s survived transactional verification failure", schema)
	}
}
