package graphplan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// columnRenameCatalogs creates currentDDL in a fresh schema and loads it with
// the desired DDL (both receive the schema name through %[1]s).
func columnRenameCatalogs(t *testing.T, prefix, currentDDL, desiredDDL string) (context.Context, *pgx.Conn, string, string, *pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") })
	if _, err := conn.Exec(ctx, fmt.Sprintf(currentDDL, quote(schemaName))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(desiredDDL, quote(schemaName))), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, conn, url, schemaName, current, desired
}

// planDirectColumnRenames answers each rename_column question from renames
// (table.old -> new, in schemaName) and each strategy question with
// direct_rename. Any other question fails the test: a dependent that the
// renames do not absorb surfaces as a reconcile or destructive question.
func planDirectColumnRenames(t *testing.T, current, desired *pgschema.Snapshot, schemaName string, renames map[string]string) protocol.Result {
	t.Helper()
	targets := make(map[string]string, len(renames))
	for from, to := range renames {
		table, column, _ := strings.Cut(from, ".")
		tableID := (pgschema.Table{Schema: schemaName, Name: table}).ObjectID()
		targets[(pgschema.Column{Table: tableID, Name: column}).ObjectID().String()] = (pgschema.Column{Table: tableID, Name: to}).ObjectID().String()
	}
	var answers protocol.Answers
	for attempt := 0; attempt < 4; attempt++ {
		result, err := Build(current, desired, answers, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != protocol.NeedsInput {
			if result.Status != protocol.Planned {
				t.Fatalf("expected a planned rename: %#v", result)
			}
			return result
		}
		answers.CurrentFingerprint, answers.DesiredFingerprint = result.CurrentFingerprint, result.DesiredFingerprint
		for _, question := range result.Questions {
			value := ""
			switch question.Kind {
			case "rename_column":
				value = targets[question.Key]
			case "rename_backfill_strategy":
				value = "direct_rename"
			}
			if value == "" {
				t.Fatalf("column renames left an unabsorbed dependent: question %s %s (%s)", question.Kind, question.Key, question.Message)
			}
			answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
		}
	}
	t.Fatal("column rename questions did not settle")
	return protocol.Result{}
}

func assertOnlyColumnRenameStatements(t *testing.T, plan protocol.Result) {
	t.Helper()
	for _, statement := range plan.Statements {
		for _, forbidden := range []string{"FOREIGN KEY", "VALIDATE", "DROP CONSTRAINT", "ADD CONSTRAINT", "CHECK", "DROP INDEX", "CREATE INDEX", "CREATE UNIQUE INDEX"} {
			if strings.Contains(statement.SQL, forbidden) {
				t.Fatalf("column rename rebuilt a dependent (%s): %s", forbidden, statement.SQL)
			}
		}
	}
}

func TestColumnRenameKeepsForeignKeysOnBothEndsOnPostgreSQL(t *testing.T) {
	ddl := func(code, parent string) string {
		return "CREATE SCHEMA %[1]s;" +
			"CREATE TABLE %[1]s.parents (id bigint PRIMARY KEY, " + code + " text, CONSTRAINT parents_code_key UNIQUE (" + code + "));" +
			"CREATE TABLE %[1]s.children (id bigint PRIMARY KEY, code text, " + parent + " bigint," +
			" CONSTRAINT children_code_fk FOREIGN KEY (code) REFERENCES %[1]s.parents (" + code + ") ON DELETE SET NULL (code)," +
			" CONSTRAINT children_parent_fk FOREIGN KEY (" + parent + ") REFERENCES %[1]s.parents (id) ON DELETE CASCADE);"
	}
	ctx, conn, url, schemaName, current, desired := columnRenameCatalogs(t, "onwardpg_column_rename_fk", ddl("code", "parent_id"), ddl("ref_code", "owner_id"))
	plan := planDirectColumnRenames(t, current, desired, schemaName, map[string]string{"parents.code": "ref_code", "children.parent_id": "owner_id"})
	assertOnlyColumnRenameStatements(t, plan)
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestColumnRenamesKeepDependentsOfSeveralRenamedColumnsOnPostgreSQL(t *testing.T) {
	ddl := func(a, b, c, d string) string {
		return "CREATE SCHEMA %[1]s;" +
			"CREATE TABLE %[1]s.accounts (id bigint, tenant_id bigint, PRIMARY KEY (id, tenant_id));" +
			"CREATE TABLE %[1]s.drafts (id bigint PRIMARY KEY, " + a + " timestamptz, " + b + " text, " + c + " text, " + d + " bigint, tenant_id bigint," +
			" CONSTRAINT drafts_resolution_state CHECK ((" + a + " IS NULL AND " + b + " IS NULL AND " + c + " IS NULL) OR (" + a + " IS NOT NULL AND " + b + " IN ('ok', 'failed') AND " + c + " IS NOT NULL AND " + d + " > 0))," +
			" CONSTRAINT drafts_account_fk FOREIGN KEY (" + d + ", tenant_id) REFERENCES %[1]s.accounts (id, tenant_id));" +
			"CREATE INDEX drafts_state_idx ON %[1]s.drafts (" + b + ", " + c + ");"
	}
	ctx, conn, url, schemaName, current, desired := columnRenameCatalogs(t, "onwardpg_column_renames_multi",
		ddl("resolved_at", "resolution_status", "resolution_receipt", "account_id"),
		ddl("settled_at", "settle_status", "settle_receipt", "owner_id"))
	plan := planDirectColumnRenames(t, current, desired, schemaName, map[string]string{
		"drafts.resolved_at": "settled_at", "drafts.resolution_status": "settle_status",
		"drafts.resolution_receipt": "settle_receipt", "drafts.account_id": "owner_id",
	})
	assertOnlyColumnRenameStatements(t, plan)
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestColumnRenameCarriesPartialUniqueIndexesOnPostgreSQL(t *testing.T) {
	ddl := func(trip string) string {
		return "CREATE SCHEMA %[1]s;" +
			"CREATE TABLE %[1]s.claims (id bigint PRIMARY KEY, order_id bigint, gift_key text, kind text, status text, " + trip + " bigint);" +
			"CREATE UNIQUE INDEX claims_active_order_gift_unique ON %[1]s.claims (order_id, gift_key) WHERE " + trip + " IS NULL AND status IN ('claimed', 'granted');" +
			"CREATE UNIQUE INDEX claims_esim_trip_unique ON %[1]s.claims (" + trip + ") WHERE kind = 'esim' AND " + trip + " IS NOT NULL;" +
			"CREATE INDEX claims_trip_expression_idx ON %[1]s.claims ((" + trip + " + 1)) INCLUDE (" + trip + ");"
	}
	ctx, conn, url, schemaName, current, desired := columnRenameCatalogs(t, "onwardpg_column_rename_partial", ddl("trip_line_item_id"), ddl("trip_id"))
	plan := planDirectColumnRenames(t, current, desired, schemaName, map[string]string{"claims.trip_line_item_id": "trip_id"})
	assertOnlyColumnRenameStatements(t, plan)
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	claims := quote(schemaName) + ".claims"
	if _, err := conn.Exec(ctx, "INSERT INTO "+claims+" (id, order_id, gift_key, kind, status, trip_id) VALUES (1, 7, 'g', 'esim', 'claimed', NULL), (2, 8, 'g', 'esim', 'claimed', 9)"); err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []string{
		"INSERT INTO " + claims + " (id, order_id, gift_key, kind, status, trip_id) VALUES (3, 7, 'g', 'other', 'granted', NULL)",
		"INSERT INTO " + claims + " (id, order_id, gift_key, kind, status, trip_id) VALUES (4, 10, 'h', 'esim', 'claimed', 9)",
	} {
		if _, err := conn.Exec(ctx, duplicate); err == nil {
			t.Fatalf("partial unique index stopped enforcing after the rename: %s", duplicate)
		}
	}
}
