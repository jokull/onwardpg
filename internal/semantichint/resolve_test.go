package semantichint

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestResolveConsumesSemanticDropAcrossPlannerStages(t *testing.T) {
	current, desired, _ := renameFixture(t)
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	resolution, err := Resolve(current, desired, []protocol.Hint{hint}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.Planned || len(resolution.Answers.Answers) != 2 || len(resolution.Questions) != 2 || len(resolution.Hints) != 1 {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestResolveRejectsUnconsumedIntent(t *testing.T) {
	current, desired, _ := renameFixture(t)
	_, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "drop", Object: "column", Name: []string{"public", "missing", "name"},
	}}, graphplan.Options{})
	if err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveWorkspaceRequiresExplicitRenameOrPreserveIntent(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeColumn := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	afterColumn := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{from, beforeColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{to, afterColumn} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	options := graphplan.Options{PreserveSurplus: true}
	pending, err := Resolve(current, desired, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Result.Status != protocol.NeedsInput || len(pending.Result.Questions) != 1 ||
		!contains(pending.Result.Questions[0].Choices, "preserve") {
		t.Fatalf("workspace ambiguity = %#v", pending)
	}
	preserved, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "preserve", Object: "table", Name: []string{"app", "accounts"},
	}}, options)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Result.Status != protocol.Planned || !contains(preserved.Result.Preserved, from.ObjectID().String()) ||
		strings.Contains(joinResolutionSQL(preserved), "RENAME TO") {
		t.Fatalf("preserved workspace object = %#v", preserved)
	}
	renamed, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "rename", Object: "table", From: []string{"app", "accounts"}, To: []string{"app", "customers"},
	}}, options)
	if err != nil {
		t.Fatal(err)
	}
	sql := joinResolutionSQL(renamed)
	if renamed.Result.Status != protocol.Planned || len(renamed.Result.Preserved) != 0 ||
		!strings.Contains(sql, `CREATE VIEW "app"."customers" WITH (security_invoker = true)`) ||
		!strings.Contains(sql, `DROP VIEW "app"."customers";`) {
		t.Fatalf("workspace rename bridge = %#v", renamed)
	}
}

func joinResolutionSQL(resolution Resolution) string {
	statements := make([]string, len(resolution.Result.Statements))
	for index, statement := range resolution.Result.Statements {
		statements[index] = statement.SQL
	}
	return strings.Join(statements, "\n")
}
