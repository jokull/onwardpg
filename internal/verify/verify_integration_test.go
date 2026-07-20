package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
)

func TestFailureDiagnosticsDistinguishExecutionBoundariesAndAssertions(t *testing.T) {
	transactional := batchFailure("feature", "batch-1", protocol.PhaseContract, true, errors.New("boom"))
	nonTransactional := batchFailure("feature", "batch-2", "expand", false, errors.New("boom"))
	assertion := assertionFailure("feature", "rows_backfilled", "assertion_false", "verification assertion returned false")
	if transactional.Code != "transactional_batch_failed" || transactional.ExecutionMode != "transactional" || !strings.Contains(transactional.Remediation, "phases/contract.sql") {
		t.Fatalf("transactional failure = %#v", transactional)
	}
	if nonTransactional.Code != "non_transactional_batch_failed" || nonTransactional.ExecutionMode != "non_transactional" || !strings.Contains(nonTransactional.Remediation, "disposable database") {
		t.Fatalf("non-transactional failure = %#v", nonTransactional)
	}
	if assertion.Code != "assertion_false" || assertion.CheckID != "rows_backfilled" || !strings.Contains(assertion.Remediation, "verify.sql") {
		t.Fatalf("assertion failure = %#v", assertion)
	}
}

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
	schema := fmt.Sprintf("onwardpg_rollback_%d", time.Now().UTC().UnixNano())
	statement := protocol.Statement{
		SQL: "CREATE SCHEMA " + testQuote(schema) + "; CREATE TABLE " + testQuote(schema) + ".example (id bigint);",
		Manual: &protocol.ManualWork{
			Summary: "prove false assertions roll back", ExecutionMode: "transactional",
			Statements: []string{"SELECT 1;"}, VerificationSQL: []string{"SELECT false;"},
		},
	}
	err = executeBatch(context.Background(), connection, protocol.Batch{
		ID: "batch-001", Phase: protocol.PhaseContract, Transactional: true, Statements: []protocol.Statement{statement},
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

func testQuote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
