package graphplan

import (
	"testing"

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
