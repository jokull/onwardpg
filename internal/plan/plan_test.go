package plan

import (
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/schema"
)

func TestBuildRequiresExplicitRenameThenPlansIt(t *testing.T) {
	current := state(table("public", "dogs", column("name", "text")))
	desired := state(table("public", "dogs_v2", column("name", "text")))

	pending := Build(current, desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected one question, got %#v", pending)
	}
	answer := protocol.Answers{Answers: []protocol.Answer{{Kind: "rename_table", Key: "public.dogs", Value: "public.dogs_v2"}}}
	result := Build(current, desired, answer)
	if result.Status != protocol.Planned {
		t.Fatalf("expected planned, got %#v", result)
	}
	if got, want := result.Statements[0].SQL, `ALTER TABLE "public"."dogs" RENAME TO "dogs_v2";`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if result.Statements[0].Phase != "contract" {
		t.Fatalf("rename must be a contract operation: %#v", result.Statements[0])
	}
}

func TestCreateIsAnExpandOperation(t *testing.T) {
	result := Build(state(), state(table("public", "dogs", column("id", "integer"))), protocol.Answers{})
	if result.Status != protocol.Planned || result.Statements[0].Phase != "expand" {
		t.Fatalf("expected expand create, got %#v", result)
	}
}

func TestBuildRequiresTypeExpression(t *testing.T) {
	current := state(table("public", "dogs", column("age", "text")))
	desired := state(table("public", "dogs", column("age", "integer")))
	pending := Build(current, desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("expected type question, got %#v", pending)
	}
	answers := protocol.Answers{Answers: []protocol.Answer{{Kind: "type_change", Key: "public.dogs.age", Value: "age::integer"}}}
	result := Build(current, desired, answers)
	if got, want := result.Statements[0].SQL, `ALTER TABLE "public"."dogs" ALTER COLUMN "age" TYPE integer USING age::integer;`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildRejectsUnmodelledDifference(t *testing.T) {
	current, desired := state(), state()
	desired.Unsupported = []string{"view:public.report"}
	result := Build(current, desired, protocol.Answers{})
	if result.Status != protocol.Unsupported || result.Unsupported[0] != "view:public.report" {
		t.Fatalf("expected unsupported view, got %#v", result)
	}
}

func state(tables ...schema.Table) schema.State {
	state := schema.NewState()
	for _, table := range tables {
		namespace := state.Schemas[table.Schema]
		if namespace.Tables == nil {
			namespace = schema.Schema{Name: table.Schema, Tables: map[string]schema.Table{}}
		}
		namespace.Tables[table.Name] = table
		state.Schemas[table.Schema] = namespace
	}
	return state
}

func table(namespace, name string, columns ...schema.Column) schema.Table {
	return schema.Table{Schema: namespace, Name: name, Columns: columns}
}
func column(name, typ string) schema.Column { return schema.Column{Name: name, Type: typ} }
