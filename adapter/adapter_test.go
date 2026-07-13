package adapter

import (
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestArtifactRequiresExactlyOneRepresentation(t *testing.T) {
	for _, test := range []struct {
		name     string
		artifact Artifact
		valid    bool
	}{
		{name: "ddl", artifact: DDL("drizzle:schema.ts", []byte("CREATE TABLE t (id int)")), valid: true},
		{name: "snapshot", artifact: Snapshot("django:app", pgschema.New()), valid: true},
		{name: "empty", artifact: Artifact{Provenance: "empty"}},
		{name: "both", artifact: Artifact{DDL: []byte("SELECT 1"), Snapshot: pgschema.New(), Provenance: "invalid"}},
		{name: "no provenance", artifact: Artifact{DDL: []byte("SELECT 1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.artifact.Validate()
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDDLArtifactCopiesInput(t *testing.T) {
	input := []byte("CREATE TABLE t (id int)")
	artifact := DDL("test", input)
	input[0] = 'X'
	if artifact.DDL[0] != 'C' {
		t.Fatal("artifact aliases caller DDL buffer")
	}
}

func TestOrderedSnapshotRequiresExactSnapshotOrder(t *testing.T) {
	snapshot := pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, object := range []pgschema.Object{schema, table} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	valid := OrderedSnapshot("tool:schema", snapshot, []pgschema.ID{table.ObjectID(), schema.ObjectID()})
	if err := valid.ValidateDumpOrder(); err != nil {
		t.Fatalf("valid ordered snapshot rejected: %v", err)
	}
	for _, test := range []struct {
		name  string
		order []pgschema.ID
	}{
		{name: "missing", order: []pgschema.ID{schema.ObjectID()}},
		{name: "duplicate", order: []pgschema.ID{schema.ObjectID(), schema.ObjectID()}},
		{name: "unknown", order: []pgschema.ID{schema.ObjectID(), {Kind: pgschema.KindTable, Schema: "app", Name: "missing"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := OrderedSnapshot("tool:schema", snapshot, test.order)
			if err := artifact.ValidateDumpOrder(); err == nil {
				t.Fatal("expected invalid dump order")
			}
		})
	}
}

func TestDumpOrderRequiresTypedSnapshot(t *testing.T) {
	artifact := DDL("tool:schema", []byte("CREATE TABLE orders ()"))
	if err := artifact.ValidateDumpOrder(); err == nil {
		t.Fatal("expected DDL artifact to reject unsorted dump")
	}
}
