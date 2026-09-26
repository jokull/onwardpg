package sqlcheck

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
)

func TestBooleanAssertionBoundary(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "assertion_boundary")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		t.Run(mode.String(), func(t *testing.T) {
			database.Config.DefaultQueryExecMode = mode
			conn, err := database.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			if _, err := conn.Exec(ctx, `
CREATE TABLE state (value integer);
INSERT INTO state VALUES (0);
CREATE OR REPLACE FUNCTION assertion_write() RETURNS boolean LANGUAGE plpgsql
AS 'BEGIN PERFORM set_config(''transaction_read_only'', ''off'', true); UPDATE state SET value=1; RETURN true; END';`); err != nil {
				t.Fatal(err)
			}
			defer conn.Exec(ctx, "DROP TABLE state")
			cases := []struct {
				name, sql       string
				passed, wantErr bool
			}{
				{"true", "SELECT true", true, false},
				{"false", "SELECT false", false, false},
				{"null", "SELECT NULL::boolean", false, true},
				{"text", "SELECT 'true'::text", false, true},
				{"empty", "SELECT true WHERE false", false, true},
				{"columns", "SELECT true, true", false, true},
				{"rows", "SELECT true UNION ALL SELECT false", false, true},
				{"write_cte", "WITH changed AS (UPDATE state SET value=1 RETURNING value) SELECT count(*)=1 FROM changed", false, true},
				{"transaction_escape", "SELECT true; COMMIT; UPDATE state SET value=1", false, true},
				{"change_access_mode", "SELECT set_config('transaction_read_only', 'off', true) = 'off'", false, true},
				{"function_changes_access_mode", "SELECT assertion_write()", false, true},
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					passed, err := Boolean(ctx, conn, test.sql)
					if passed != test.passed || (err != nil) != test.wantErr {
						t.Fatalf("Boolean = %v, %v; want %v, error=%v", passed, err, test.passed, test.wantErr)
					}
					var value int
					if err := conn.QueryRow(ctx, "SELECT value FROM state").Scan(&value); err != nil {
						t.Fatal(err)
					}
					if value != 0 {
						t.Fatalf("assertion wrote persistent value %d", value)
					}
				})
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "UPDATE state SET value=2"); err != nil {
				t.Fatal(err)
			}
			passed, err := Boolean(ctx, tx, "SELECT value=2 FROM state")
			if err != nil || !passed {
				t.Fatalf("assertion cannot see parent transaction: %v, %v", passed, err)
			}
			if _, err := Boolean(ctx, tx, "UPDATE state SET value=3 RETURNING true"); err == nil {
				t.Fatal("accepted write inside nested assertion")
			}
			if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '50ms'"); err != nil {
				t.Fatal(err)
			}
			if passed, err := Boolean(ctx, tx, "SELECT true FROM pg_sleep(10)"); passed || err == nil {
				t.Fatalf("accepted timed out assertion: passed=%v err=%v", passed, err)
			}
			// Failed assertions must leave the parent transaction usable and writable.
			if _, err := tx.Exec(ctx, "UPDATE state SET value=4"); err != nil {
				t.Fatalf("assertion changed parent transaction: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var value int
			if err := conn.QueryRow(ctx, "SELECT value FROM state").Scan(&value); err != nil || value != 4 {
				t.Fatalf("parent transaction result: %d, %v", value, err)
			}
		})
	}
}

func TestBooleanCancellationDoesNotPersistParentWrites(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "assertion_cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE state (value integer); INSERT INTO state VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE state SET value=1"); err != nil {
		t.Fatal(err)
	}
	queryContext, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if passed, err := Boolean(queryContext, tx, "SELECT true FROM pg_sleep(10)"); passed || err == nil {
		t.Fatalf("accepted canceled assertion: passed=%v err=%v", passed, err)
	}
	// pgx may discard the canceled connection. Either way no parent work
	// may have committed, and an independent connection must observe it.
	_ = tx.Rollback(ctx)
	_ = conn.Close(ctx)
	observer, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(ctx)
	var value int
	if err := observer.QueryRow(ctx, "SELECT value FROM state").Scan(&value); err != nil || value != 0 {
		t.Fatalf("canceled assertion persisted parent work: value=%d err=%v", value, err)
	}
}

func TestBooleanCannotCommitParentTransaction(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "assertion_commit")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE state (value integer); INSERT INTO state VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"COMMIT", "COMMIT AND CHAIN", "ROLLBACK", "ROLLBACK AND CHAIN"} {
		t.Run(sql, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "UPDATE state SET value=1"); err != nil {
				t.Fatal(err)
			}
			if passed, err := Boolean(ctx, tx, sql); passed || err == nil {
				t.Fatalf("accepted transaction control: passed=%v err=%v", passed, err)
			}
			if conn.PgConn().TxStatus() != 'T' {
				t.Fatalf("assertion ended caller transaction: status=%c", conn.PgConn().TxStatus())
			}
			var value int
			if err := tx.QueryRow(ctx, "SELECT value FROM state").Scan(&value); err != nil || value != 1 {
				t.Fatalf("assertion discarded parent work: value=%d err=%v", value, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow(ctx, "SELECT value FROM state").Scan(&value); err != nil || value != 0 {
				t.Fatalf("assertion committed parent work: value=%d err=%v", value, err)
			}
		})
	}
}

func TestBooleanRejectsFunctionLocalRLSVisibility(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	for _, externalOwner := range []bool{false, true} {
		name := "forced_owner"
		if externalOwner {
			name = "other_table_owner"
		}
		t.Run(name, func(t *testing.T) {
			database, err := scratchdb.Create(ctx, adminURL, "assertion_rls")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			conn, err := database.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			if _, err := conn.Exec(ctx, `
CREATE TABLE state (value integer);
INSERT INTO state VALUES (NULL);
ALTER TABLE state ENABLE ROW LEVEL SECURITY;
CREATE FUNCTION filtered_state() RETURNS boolean LANGUAGE sql SET row_security=on
AS 'SELECT NOT EXISTS (SELECT FROM state WHERE value IS NULL)';`); err != nil {
				t.Fatal(err)
			}
			// Non-forced RLS owned by the observer does not filter its rows.
			if passed, err := Boolean(ctx, conn, "SELECT filtered_state()"); passed || err != nil {
				t.Fatalf("owner bypass should observe the invalid row: passed=%v err=%v", passed, err)
			}
			if externalOwner {
				config, err := pgx.ParseConfig(adminURL)
				if err != nil {
					t.Fatal(err)
				}
				config.Database = database.Name
				admin, err := pgx.ConnectConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer admin.Close(ctx)
				if _, err := admin.Exec(ctx, "ALTER TABLE state OWNER TO "+pgx.Identifier{config.User}.Sanitize()+"; GRANT SELECT ON state TO "+pgx.Identifier{database.Role}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := conn.Exec(ctx, "ALTER TABLE state FORCE ROW LEVEL SECURITY"); err != nil {
				t.Fatal(err)
			}
			// A function-local GUC restores row_security on return. Checking only
			// the caller's setting before or after the assertion misses this case.
			if _, err := conn.Exec(ctx, "SET row_security=off"); err != nil {
				t.Fatal(err)
			}
			var hidden bool
			var rowSecurity string
			if err := conn.QueryRow(ctx, "SELECT filtered_state(), current_setting('row_security')").Scan(&hidden, &rowSecurity); err != nil || !hidden || rowSecurity != "off" {
				t.Fatalf("RLS reproduction: hidden=%v row_security=%s err=%v", hidden, rowSecurity, err)
			}
			if _, err := conn.Exec(ctx, `
CREATE SCHEMA shadow;
CREATE FUNCTION shadow.row_security_active(oid) RETURNS boolean LANGUAGE sql AS 'SELECT false';
CREATE VIEW shadow.pg_class AS SELECT * FROM pg_catalog.pg_class WHERE false;
SET search_path=shadow,public,pg_catalog;`); err != nil {
				t.Fatal(err)
			}
			passed, err := Boolean(ctx, conn, "SELECT filtered_state()")
			if passed || err == nil || !strings.Contains(err.Error(), "row-level security") {
				t.Fatalf("accepted hidden invalid row: passed=%v err=%v", passed, err)
			}
			// The preflight is deliberately conservative even if this particular
			// query does not refer to the filtered table.
			if passed, err := Boolean(ctx, conn, "SELECT true"); passed || err == nil {
				t.Fatalf("accepted assertion in filtered catalog: passed=%v err=%v", passed, err)
			}
		})
	}
}

func TestBooleanPreservesCallerSnapshot(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "assertion_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reader, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close(ctx)
	writer, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(ctx)
	if _, err := writer.Exec(ctx, "CREATE TABLE state (value integer); INSERT INTO state VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	tx, err := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if passed, err := Boolean(ctx, tx, "SELECT value=0 FROM state"); !passed || err != nil {
		t.Fatalf("initial snapshot: passed=%v err=%v", passed, err)
	}
	if _, err := writer.Exec(ctx, "UPDATE state SET value=1"); err != nil {
		t.Fatal(err)
	}
	if passed, err := Boolean(ctx, tx, "SELECT value=0 FROM state"); !passed || err != nil {
		t.Fatalf("assertion lost caller snapshot: passed=%v err=%v", passed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if passed, err := Boolean(ctx, reader, "SELECT value=1 FROM state"); !passed || err != nil {
		t.Fatalf("fresh snapshot: passed=%v err=%v", passed, err)
	}
}
