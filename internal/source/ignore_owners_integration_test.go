package source

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

// A schema of archive tables that the operator granted to pg_read_all_data
// holds ACL state that the planner cannot model. schema:NAME masks it with
// the schema; the same state on a schema or object outside the selectors
// still blocks. Table, column, and index selectors mask the state of their
// own object.
func TestIgnoreSelectorsMaskUnsupportedStateOfTheirObjectsOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockIntegrationDatabase(t, ctx, conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())

	suffix := time.Now().UTC().UnixNano()
	legacy := fmt.Sprintf("onwardpg_masked_%d", suffix)
	other := fmt.Sprintf("onwardpg_kept_%d", suffix)
	kept := fmt.Sprintf("onwardpg_kept_table_%d", suffix)
	var user string
	if err := tx.QueryRow(ctx, "SELECT quote_ident(current_user)").Scan(&user); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE SCHEMA " + legacy,
		"CREATE SCHEMA " + other,
		"CREATE TABLE " + legacy + ".order_snapshot (id bigint PRIMARY KEY, body text)",
		"CREATE INDEX order_snapshot_body_idx ON " + legacy + ".order_snapshot (body)",
		"CLUSTER " + legacy + ".order_snapshot USING order_snapshot_body_idx",
		"CREATE SEQUENCE " + legacy + ".archive_seq",
		"CREATE TYPE " + legacy + ".state AS ENUM ('kept')",
		"CREATE FUNCTION " + legacy + ".archive(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1'",
		"GRANT USAGE ON SCHEMA " + legacy + " TO pg_read_all_data",
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + legacy + " TO pg_read_all_data",
		"GRANT SELECT (body) ON " + legacy + ".order_snapshot TO pg_read_all_data",
		"GRANT USAGE ON SEQUENCE " + legacy + ".archive_seq TO pg_read_all_data",
		"REVOKE USAGE ON TYPE " + legacy + ".state FROM PUBLIC",
		"REVOKE EXECUTE ON FUNCTION " + legacy + ".archive(integer) FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA " + legacy + " GRANT SELECT ON TABLES TO pg_read_all_data",
		"GRANT USAGE ON SCHEMA " + other + " TO pg_read_all_data",
		"CREATE TABLE public." + kept + " (id bigint PRIMARY KEY, secret text)",
		"CREATE INDEX " + kept + "_secret_idx ON public." + kept + " (secret)",
		"CLUSTER public." + kept + " USING " + kept + "_secret_idx",
		"GRANT SELECT (secret) ON public." + kept + " TO pg_read_all_data",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	legacyState := []string{
		"acl:schema:" + legacy,
		"acl:column:" + legacy + ".order_snapshot.body",
		"clustered_index:" + legacy + ".order_snapshot_body_idx",
		"acl:relation:" + legacy + ".archive_seq",
		"acl:type:" + legacy + ".state",
		"acl:routine:" + legacy + ".archive(integer)",
		"acl:default:" + user + ":" + legacy + ":r",
	}
	otherState := "acl:schema:" + other
	keptColumn := "acl:column:public." + kept + ".secret"
	keptIndex := "clustered_index:public." + kept + "_secret_idx"
	inspect := func(ignores ...string) *pgschema.Snapshot {
		t.Helper()
		if _, err := tx.Exec(ctx, "SAVEPOINT inspect"); err != nil {
			t.Fatal(err)
		}
		graph, err := InspectGraphTransaction(ctx, tx, ignores, true)
		if err != nil {
			t.Fatalf("inspect with %v: %v", ignores, err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT inspect"); err != nil {
			t.Fatal(err)
		}
		return graph
	}
	ours := func(graph *pgschema.Snapshot) []string {
		var result []string
		for _, selector := range graph.Unsupported() {
			if strings.Contains(selector, fmt.Sprint(suffix)) {
				result = append(result, selector)
			}
		}
		return result
	}
	requireState := func(graph *pgschema.Snapshot, want ...string) {
		t.Helper()
		got := ours(graph)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("unsupported state = %v, want %v", got, want)
		}
		for _, receipt := range graph.Ignored() {
			if !strings.HasPrefix(receipt, "schema:") && !strings.HasPrefix(receipt, "table:") &&
				!strings.HasPrefix(receipt, "column:") && !strings.HasPrefix(receipt, "index:") &&
				!strings.HasPrefix(receipt, "sequence:") && !strings.HasPrefix(receipt, "enum:") &&
				!strings.HasPrefix(receipt, "routine:") && !strings.HasPrefix(receipt, "table_privilege:") &&
				!strings.HasPrefix(receipt, "constraint:") {
				t.Fatalf("masked state left a receipt of its own: %s", receipt)
			}
		}
	}

	requireState(inspect(), append([]string{otherState, keptColumn, keptIndex}, legacyState...)...)
	requireState(inspect("schema:"+legacy), otherState, keptColumn, keptIndex)
	requireState(inspect("schema:"+legacy, "table:public."+kept), otherState)
	requireState(inspect("schema:"+legacy, "column:public."+kept+".secret"), otherState, keptIndex)
	requireState(inspect("schema:"+legacy, "index:public."+kept+"."+kept+"_secret_idx"), otherState, keptColumn)
}

// GRANT then REVOKE leaves an explicit ACL that holds exactly the default
// privileges. It means what the NULL ACL means, so it is neither unsupported
// state nor drift.
func TestExplicitDefaultACLsAfterGrantRevokeAreTheDefaultOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockIntegrationDatabase(t, ctx, conn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	schema := fmt.Sprintf("onwardpg_default_acl_%d", time.Now().UTC().UnixNano())
	exec := func(statements ...string) {
		t.Helper()
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	exec(
		"CREATE SCHEMA "+schema,
		"CREATE TABLE "+schema+".item (id bigint PRIMARY KEY, body text)",
		"CREATE SEQUENCE "+schema+".item_seq",
		"CREATE TYPE "+schema+".state AS ENUM ('kept')",
		"CREATE FUNCTION "+schema+".touch(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1'",
	)
	inspect := func() *pgschema.Snapshot {
		t.Helper()
		graph, err := InspectGraphTransaction(ctx, tx, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, selector := range graph.Unsupported() {
			if strings.Contains(selector, schema) {
				t.Fatalf("unsupported state for a default-equivalent ACL: %v", graph.Unsupported())
			}
		}
		return graph
	}
	baseline, err := inspect().Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	exec(
		"GRANT USAGE ON SCHEMA "+schema+" TO pg_read_all_data",
		"GRANT SELECT ON "+schema+".item TO pg_read_all_data",
		"GRANT SELECT (body) ON "+schema+".item TO pg_read_all_data",
		"GRANT USAGE ON SEQUENCE "+schema+".item_seq TO pg_read_all_data",
		"GRANT USAGE ON TYPE "+schema+".state TO pg_read_all_data",
		"GRANT EXECUTE ON FUNCTION "+schema+".touch(integer) TO pg_read_all_data",
		"REVOKE USAGE ON SCHEMA "+schema+" FROM pg_read_all_data",
		"REVOKE SELECT ON "+schema+".item FROM pg_read_all_data",
		"REVOKE SELECT (body) ON "+schema+".item FROM pg_read_all_data",
		"REVOKE USAGE ON SEQUENCE "+schema+".item_seq FROM pg_read_all_data",
		"REVOKE USAGE ON TYPE "+schema+".state FROM pg_read_all_data",
		"REVOKE EXECUTE ON FUNCTION "+schema+".touch(integer) FROM pg_read_all_data",
	)
	var explicit int
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM pg_namespace WHERE nspname = $1 AND nspacl IS NOT NULL)
     + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname IN ('item', 'item_seq') AND c.relacl IS NOT NULL)
     + (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = $1 AND t.typname = 'state' AND t.typacl IS NOT NULL)
     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1 AND p.proname = 'touch' AND p.proacl IS NOT NULL)`,
		schema).Scan(&explicit); err != nil {
		t.Fatal(err)
	}
	if explicit != 5 {
		t.Fatalf("grant and revoke left %d explicit ACLs, want 5", explicit)
	}
	got, err := inspect().Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != baseline {
		t.Fatalf("explicit default ACLs changed the fingerprint: got %s, want %s", got, baseline)
	}
	// A privilege that differs from the default still blocks.
	exec("REVOKE EXECUTE ON FUNCTION " + schema + ".touch(integer) FROM PUBLIC")
	graph, err := InspectGraphTransaction(ctx, tx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(graph.Unsupported(), "acl:routine:"+schema+".touch(integer)") {
		t.Fatalf("a revoked default privilege is not unsupported: %v", graph.Unsupported())
	}
}
