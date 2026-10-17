package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"

	"github.com/jokull/onwardpg/pgschema"
)

func TestColumnRenameAbsorbsForeignKeyColumnLists(t *testing.T) {
	parents := pgschema.Table{Schema: "app", Name: "parents"}.ObjectID()
	children := pgschema.Table{Schema: "app", Name: "children"}.ObjectID()
	column := func(table pgschema.ID, name string) pgschema.Column {
		return pgschema.Column{Table: table, Name: name, Type: "text"}
	}
	foreignKey := func(local []string, referenced []string, definition string) pgschema.Constraint {
		return pgschema.Constraint{
			Table: children, Name: "children_fk", Type: pgschema.ConstraintForeign, Reference: &parents,
			Definition: definition, ForeignKeyColumns: local, ReferencedColumns: referenced,
			ForeignKeyMatch: pgschema.ForeignKeyMatchSimple, ForeignKeyOnUpdate: pgschema.ForeignKeyNoAction,
			ForeignKeyOnDelete: pgschema.ForeignKeySetNull, UsingIndex: "parents_code_key", Validated: true,
		}
	}
	for _, test := range []struct {
		name          string
		renames       columnRenameSet
		before, after pgschema.Constraint
		want          bool
	}{
		{
			name:    "local column renamed",
			renames: columnRenamesOf(column(children, "parent_code"), column(children, "owner_code")),
			before:  foreignKey([]string{"parent_code"}, []string{"code"}, "FOREIGN KEY (parent_code) REFERENCES app.parents(code) ON DELETE SET NULL (parent_code)"),
			after:   foreignKey([]string{"owner_code"}, []string{"code"}, "FOREIGN KEY (owner_code) REFERENCES app.parents(code) ON DELETE SET NULL (owner_code)"),
			want:    true,
		},
		{
			name:    "referenced column renamed, local column shares its name",
			renames: columnRenamesOf(column(parents, "code"), column(parents, "ref_code")),
			before:  foreignKey([]string{"code"}, []string{"code"}, "FOREIGN KEY (code) REFERENCES app.parents(code) ON DELETE SET NULL"),
			after:   foreignKey([]string{"code"}, []string{"ref_code"}, "FOREIGN KEY (code) REFERENCES app.parents(ref_code) ON DELETE SET NULL"),
			want:    true,
		},
		{
			name:    "both ends renamed",
			renames: columnRenamesOf(column(parents, "code"), column(parents, "ref_code"), column(children, "code"), column(children, "parent_ref")),
			before:  foreignKey([]string{"code"}, []string{"code"}, "FOREIGN KEY (code) REFERENCES app.parents(code)"),
			after:   foreignKey([]string{"parent_ref"}, []string{"ref_code"}, "FOREIGN KEY (parent_ref) REFERENCES app.parents(ref_code)"),
			want:    true,
		},
		{
			name:    "a rename on the other end does not rewrite the local list",
			renames: columnRenamesOf(column(parents, "code"), column(parents, "ref_code")),
			before:  foreignKey([]string{"code"}, []string{"code"}, "FOREIGN KEY (code) REFERENCES app.parents(code)"),
			after:   foreignKey([]string{"ref_code"}, []string{"ref_code"}, "FOREIGN KEY (ref_code) REFERENCES app.parents(ref_code)"),
			want:    false,
		},
		{
			name:    "a real action change is still planned",
			renames: columnRenamesOf(column(children, "parent_code"), column(children, "owner_code")),
			before:  foreignKey([]string{"parent_code"}, []string{"code"}, "FOREIGN KEY (parent_code) REFERENCES app.parents(code) ON DELETE SET NULL"),
			after: func() pgschema.Constraint {
				after := foreignKey([]string{"owner_code"}, []string{"code"}, "FOREIGN KEY (owner_code) REFERENCES app.parents(code) ON DELETE CASCADE")
				after.ForeignKeyOnDelete = pgschema.ForeignKeyCascade
				return after
			}(),
			want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := equivalentAutomaticColumnReferencesAfterRenames(test.before, test.after, test.renames); got != test.want {
				t.Fatalf("equivalent = %v, want %v", got, test.want)
			}
		})
	}
}

// multiColumnRenameSnapshots builds app.ranges(lo, hi) with a CHECK and an
// index over both columns; desired renames lo -> low and hi -> high.
func multiColumnRenameSnapshots(t *testing.T) (current, desired *pgschema.Snapshot, renames map[string]string) {
	t.Helper()
	table := pgschema.Table{Schema: "app", Name: "ranges"}
	build := func(lo, hi string) *pgschema.Snapshot {
		snapshot := pgschema.New()
		columns := []pgschema.Column{
			{Table: table.ObjectID(), Name: lo, Position: 1, Type: "integer"},
			{Table: table.ObjectID(), Name: hi, Position: 2, Type: "integer"},
		}
		check := pgschema.Constraint{
			Table: table.ObjectID(), Name: "ranges_ordered", Type: pgschema.ConstraintCheck, Validated: true,
			Definition: "CHECK ((" + lo + " <= " + hi + "))", CheckExpression: "(" + lo + " <= " + hi + ")",
		}
		index := pgschema.Index{Table: table.ObjectID(), Name: "ranges_bounds_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: lo}, {Column: hi}}}
		for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, table, columns[0], columns[1], check, index} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, column := range columns {
			for _, dependent := range []pgschema.ID{check.ObjectID(), index.ObjectID()} {
				if err := snapshot.AddDependency(dependent, column.ObjectID()); err != nil {
					t.Fatal(err)
				}
			}
		}
		return snapshot
	}
	column := func(name string) string {
		return (pgschema.Column{Table: table.ObjectID(), Name: name}).ObjectID().String()
	}
	return build("lo", "hi"), build("low", "high"), map[string]string{column("lo"): column("low"), column("hi"): column("high")}
}

func TestColumnRenamesAbsorbADependentNamingSeveralRenamedColumns(t *testing.T) {
	current, desired, renames := multiColumnRenameSnapshots(t)
	var answers protocol.Answers
	var plan protocol.Result
	for attempt := 0; attempt < 4; attempt++ {
		result, err := Build(current, desired, answers, Options{})
		if err != nil {
			t.Fatal(err)
		}
		plan = result
		if result.Status != protocol.NeedsInput {
			break
		}
		answers.CurrentFingerprint, answers.DesiredFingerprint = result.CurrentFingerprint, result.DesiredFingerprint
		for _, question := range result.Questions {
			value := map[string]string{"rename_backfill_strategy": "direct_rename"}[question.Kind]
			if question.Kind == "rename_column" {
				value = renames[question.Key]
			}
			if value == "" {
				t.Fatalf("a dependent of both renamed columns was not absorbed: %s %s", question.Kind, question.Key)
			}
			answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
		}
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected a planned rename: %#v", plan)
	}
	sql := joinSQL(plan)
	if strings.Contains(sql, "CHECK") || strings.Contains(sql, "INDEX") || strings.Count(sql, "RENAME COLUMN") != 2 {
		t.Fatalf("expected exactly the two column renames:\n%s", sql)
	}
}

func TestRenameIdentifierTokensAppliesRenamesInOnePass(t *testing.T) {
	got := renameIdentifierTokens(`CHECK ((a < b) AND ("b" > 0))`, map[string]string{"a": "b", "b": "a"})
	if want := `CHECK ((b < a) AND ("a" > 0))`; got != want {
		t.Fatalf("renameIdentifierTokens = %q, want %q", got, want)
	}
}
