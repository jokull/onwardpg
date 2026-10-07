package semantichint

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestIndexDropHintsAreUnneededOnlyWhenNoDecisionIsAsked(t *testing.T) {
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	plain := pgschema.Index{Table: table.ObjectID(), Name: "accounts_org_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "org"}}}
	unique := pgschema.Index{Table: table.ObjectID(), Name: "accounts_email_idx", Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "email"}}}
	kept := pgschema.Index{Table: table.ObjectID(), Name: "accounts_name_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "name"}}}
	current, desired := pgschema.New(), pgschema.New()
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, table, plain, unique, kept} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, table, kept} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	hint := func(object, name string) protocol.Hint {
		return protocol.Hint{Kind: "drop", Object: object, Name: []string{"app", "accounts", name}}
	}
	for name, want := range map[string]bool{"accounts_org_idx": true, "accounts_email_idx": false, "accounts_name_idx": false, "absent_idx": false} {
		if got := Unneeded(hint("index", name), current, desired); got != want {
			t.Fatalf("Unneeded(%s) = %t, want %t", name, got, want)
		}
	}
	if Unneeded(hint("column", "accounts_org_idx"), current, desired) {
		t.Fatal("a column hint is never unneeded")
	}

	// The plain drop needs no hint; a hint for it is accepted. The unique
	// drop asks one decision whose hazard is the enforcement, not data loss.
	resolution, err := Resolve(current, desired, []protocol.Hint{hint("index", "accounts_org_idx")}, graphplan.Options{})
	if err != nil {
		t.Fatalf("an unneeded hint must not fail planning: %v", err)
	}
	if resolution.Result.Status != protocol.NeedsInput || len(resolution.Result.Questions) != 1 {
		t.Fatalf("result = %#v", resolution.Result)
	}
	decisions, err := Decisions(resolution.Result.Questions, current, desired)
	if err != nil || len(decisions) != 1 || len(decisions[0].Choices) != 1 {
		t.Fatalf("decisions = %#v (%v)", decisions, err)
	}
	if got := strings.Join(decisions[0].Choices[0].Hazards, ","); got != "unique_index_enforcement_removed,duplicate_rows_possible" {
		t.Fatalf("choice hazards = %s", got)
	}
	// A workspace plan preserves the index, so the hint answers nothing there.
	if _, err := Resolve(current, desired, []protocol.Hint{hint("index", "accounts_org_idx")}, graphplan.Options{PreserveSurplus: true}); err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("a drop hint for a preserved index must fail: %v", err)
	}
	if _, err := Resolve(current, desired, []protocol.Hint{hint("index", "accounts_name_idx")}, graphplan.Options{}); err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("a hint for a retained index must still fail: %v", err)
	}
}
