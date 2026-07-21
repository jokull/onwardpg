package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestClassifyViewReplacementUsesPostgresOutputRules(t *testing.T) {
	column := testViewColumn
	base := pgschema.View{
		Schema: "app", Name: "account_directory", Definition: "SELECT id, display_name FROM app.accounts",
		OutputColumns: []pgschema.ViewColumn{column("id", "int8"), column("display_name", "text")},
	}

	tests := []struct {
		name     string
		mutate   func(*pgschema.View)
		legality viewReplacementLegality
		reason   string
	}{
		{name: "same signature", legality: viewReplacementLegal},
		{name: "append output", mutate: func(view *pgschema.View) {
			view.OutputColumns = append(view.OutputColumns, column("full_name", "text"))
		}, legality: viewReplacementLegal},
		{name: "rename output", mutate: func(view *pgschema.View) {
			view.OutputColumns[1].Name = "full_name"
		}, legality: viewReplacementIllegal, reason: "output_name_changed"},
		{name: "change type", mutate: func(view *pgschema.View) {
			view.OutputColumns[1] = column("display_name", "int4")
		}, legality: viewReplacementIllegal, reason: "output_type_changed"},
		{name: "remove output", mutate: func(view *pgschema.View) {
			view.OutputColumns = view.OutputColumns[:1]
		}, legality: viewReplacementIllegal, reason: "output_removed"},
		{name: "missing signature", mutate: func(view *pgschema.View) {
			view.OutputColumns = nil
		}, legality: viewReplacementUnknown, reason: "output_signature_unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			after := base
			after.OutputColumns = append([]pgschema.ViewColumn(nil), base.OutputColumns...)
			if test.mutate != nil {
				test.mutate(&after)
			}
			legality, reason := classifyViewReplacement(base, after)
			if legality != test.legality || reason != test.reason {
				t.Fatalf("legality=%s reason=%q, want %s containing %q", legality, reason, test.legality, test.reason)
			}
		})
	}
}

func TestBuildNeverRendersIllegalCreateOrReplaceView(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.View{
		Schema: "app", Name: "account_directory",
		Definition:    "SELECT id, display_name FROM app.accounts",
		OutputColumns: []pgschema.ViewColumn{testViewColumn("id", "int8"), testViewColumn("display_name", "text")},
	}
	after := before
	after.Definition = "SELECT id, full_name FROM app.accounts"
	after.OutputColumns = []pgschema.ViewColumn{testViewColumn("id", "int8"), testViewColumn("full_name", "text")}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "view_replace_output_signature:" + after.ObjectID().String() + ":output_name_changed"
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, want) {
		t.Fatalf("illegal output rename escaped renderer: %#v", result)
	}
	if strings.Contains(joinSQL(result), "CREATE OR REPLACE VIEW") {
		t.Fatalf("illegal CREATE OR REPLACE VIEW was rendered: %#v", result)
	}
}

func TestBuildAllowsAppendCompatibleCreateOrReplaceView(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.View{
		Schema: "app", Name: "account_directory",
		Definition:    "SELECT id FROM app.accounts",
		OutputColumns: []pgschema.ViewColumn{testViewColumn("id", "int8")},
	}
	after := before
	after.Definition = "SELECT id, full_name FROM app.accounts"
	after.OutputColumns = append(append([]pgschema.ViewColumn(nil), before.OutputColumns...), testViewColumn("full_name", "text"))
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || !strings.Contains(joinSQL(result), "CREATE OR REPLACE VIEW") {
		t.Fatalf("append-compatible replacement was not planned: %#v", result)
	}
}

func testViewColumn(name, typeName string) pgschema.ViewColumn {
	return pgschema.ViewColumn{
		Name: name, Type: typeName, TypeMod: -1,
		TypeSchema: "pg_catalog", TypeName: typeName, TypeKind: "b",
	}
}
