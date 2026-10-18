package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// columnDefaultDependentsSnapshots drops a column default (a contract
// statement) while it widens a CHECK and adds an index and a view that read
// the column. Each of those runs in expand on its own; the default must not
// pull them into contract.
func columnDefaultDependentsSnapshots(t *testing.T, desiredNotNull bool) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "quote"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true}
	draft := "'draft'::text"
	status := pgschema.Column{Table: table.ObjectID(), Name: "status", Position: 2, Type: "text", Default: &draft}
	nextStatus := status
	nextStatus.Default = nil
	nextStatus.NotNull = desiredNotNull
	addObjects(t, current, table, id, status)
	addObjects(t, desired, table, id, nextStatus)
	narrow := pgschema.Constraint{Table: table.ObjectID(), Name: "quote_status_check", Type: pgschema.ConstraintCheck, Definition: "CHECK (status = ANY (ARRAY['draft'::text, 'open'::text]))", CheckExpression: "status = ANY (ARRAY['draft'::text, 'open'::text])", Validated: true}
	check := narrow
	check.Definition = "CHECK (status = ANY (ARRAY['draft'::text, 'open'::text, 'accepted'::text]))"
	check.CheckExpression = "status = ANY (ARRAY['draft'::text, 'open'::text, 'accepted'::text])"
	addObjects(t, current, narrow)
	if err := current.AddDependency(narrow.ObjectID(), status.ObjectID()); err != nil {
		t.Fatal(err)
	}
	index := pgschema.Index{Table: table.ObjectID(), Name: "quote_status_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "status"}}}
	view := pgschema.View{Schema: "app", Name: "open_quote", Definition: " SELECT id,\n    status\n   FROM app.quote\n  WHERE status = 'open'::text;", OutputColumns: []pgschema.ViewColumn{testViewColumn("id", "int8"), testViewColumn("status", "text")}}
	addObjects(t, desired, check, index, view)
	for _, dependent := range []pgschema.ID{check.ObjectID(), index.ObjectID(), view.ObjectID()} {
		if err := desired.AddDependency(dependent, nextStatus.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	return current, desired
}

func TestDroppedColumnDefaultDoesNotPromoteDependentsToContract(t *testing.T) {
	current, desired := columnDefaultDependentsSnapshots(t, false)
	plan := planRepeatedly(t, current, desired, 20)
	if plan.Status != protocol.Planned {
		t.Fatalf("plan status = %s: %#v", plan.Status, plan)
	}
	assertColumnDefaultDependentsInExpand(t, plan)
}

// SET NOT NULL is contract too, but a CHECK, an index, or a view does not
// need the column NOT NULL. Before, the promoted CHECK lost its VALIDATE gate
// and the plan was unsupported.
func TestColumnNotNullChangeDoesNotPromoteDependentsToContract(t *testing.T) {
	current, desired := columnDefaultDependentsSnapshots(t, true)
	plan := buildForTest(t, current, desired, []protocol.Answer{{Kind: "set_not_null", Key: pgschema.ID{Kind: pgschema.KindColumn, Schema: "app", Name: "quote", Part: "status"}.String(), Value: "direct"}}, Options{})
	if plan.Status != protocol.Planned {
		t.Fatalf("plan status = %s: %#v", plan.Status, plan)
	}
	assertColumnDefaultDependentsInExpand(t, plan)
}

func assertColumnDefaultDependentsInExpand(t *testing.T, plan protocol.Result) {
	t.Helper()
	found := map[string]bool{}
	for _, statement := range plan.Statements {
		switch {
		case strings.Contains(statement.SQL, `ALTER COLUMN "status" DROP DEFAULT`):
			found["default"] = true
			if statement.Phase != protocol.PhaseContract {
				t.Fatalf("DROP DEFAULT phase = %s, want contract", statement.Phase)
			}
		case strings.Contains(statement.SQL, "quote_status_check"), strings.Contains(statement.SQL, "quote_status_idx"), strings.Contains(statement.SQL, "open_quote"):
			found[statement.SQL] = true
			if statement.Phase != protocol.PhaseExpand || containsString(statement.Hazards, "dependency_phase_promotion") {
				t.Fatalf("dependent %q phase = %s hazards = %v, want expand", statement.SQL, statement.Phase, statement.Hazards)
			}
		}
	}
	if !found["default"] || len(found) < 4 {
		t.Fatalf("expected DROP DEFAULT and three dependent statements, got %v:\n%s", found, joinSQL(plan))
	}
}
