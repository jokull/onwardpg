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
