package devflow

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestVersionIsDistinctFromDurableBundleProtocol(t *testing.T) {
	if Version == "" || Version == "onwardpg.draft/v5" {
		t.Fatalf("development protocol version = %q", Version)
	}
}

func TestEvaluatePostconditionsUsesReadOnlyTransactions(t *testing.T) {
	databaseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	schema := fmt.Sprintf("onwardpg_devcheck_%d", time.Now().UnixNano())
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+quoteIdentifier(schema)+"; CREATE TABLE "+quoteIdentifier(schema)+".state (value integer NOT NULL); INSERT INTO "+quoteIdentifier(schema)+".state VALUES (0);"); err != nil {
		t.Fatal(err)
	}
	defer connection.Exec(ctx, "DROP SCHEMA "+quoteIdentifier(schema)+" CASCADE;")

	checks := []Postcondition{
		{BundleID: "upstream", Path: "upstream/verify.sql", ID: "read_only", SQL: "SELECT current_setting('transaction_read_only') = 'on';"},
		{BundleID: "upstream", Path: "upstream/verify.sql", ID: "write_rejected", SQL: "UPDATE " + quoteIdentifier(schema) + ".state SET value = 1 RETURNING true;"},
		{BundleID: "upstream", Path: "upstream/verify.sql", ID: "still_zero", SQL: "SELECT value = 0 FROM " + quoteIdentifier(schema) + ".state;"},
	}
	results, err := evaluatePostconditions(ctx, databaseURL, checks)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].Status != "passed" || results[1].Status != "failed" || results[2].Status != "passed" {
		t.Fatalf("results = %#v", results)
	}
	var value int
	if err := connection.QueryRow(ctx, "SELECT value FROM "+quoteIdentifier(schema)+".state").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 0 {
		t.Fatalf("read-only postcondition wrote %d", value)
	}
}

func quoteIdentifier(value string) string { return `"` + value + `"` }
