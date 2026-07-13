package change

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestBetweenProducesTypedSemanticChanges(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	oldTable := pgschema.Table{Schema: "public", Name: "obsolete"}
	sharedBefore := pgschema.Table{Schema: "public", Name: "accounts"}
	sharedAfter := pgschema.Table{Schema: "public", Name: "accounts", Partition: &pgschema.Partition{Strategy: "HASH"}}
	newTable := pgschema.Table{Schema: "public", Name: "orders"}
	for _, item := range []pgschema.Object{oldTable, sharedBefore} {
		if err := current.Add(item); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []pgschema.Object{sharedAfter, newTable} {
		if err := desired.Add(item); err != nil {
			t.Fatal(err)
		}
	}
	changes := Between(current, desired)
	got := make([]Kind, 0, len(changes))
	for _, change := range changes {
		got = append(got, change.Kind)
	}
	if want := []Kind{Modify, Drop, Create}; !reflect.DeepEqual(got, want) {
		t.Fatalf("change kinds = %#v, want %#v", got, want)
	}
	if _, ok := changes[0].Before.(pgschema.Table); !ok {
		t.Fatalf("modify must retain typed before object: %#v", changes[0])
	}
	if _, ok := changes[0].After.(pgschema.Table); !ok {
		t.Fatalf("modify must retain typed after object: %#v", changes[0])
	}
}

func TestScheduleCreatesInDependencyOrderAndDropsInReverse(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	table := pgschema.Table{Schema: "public", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Type: "bigint"}
	for _, object := range []pgschema.Object{schema, table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, dependency := range [][2]pgschema.ID{{table.ObjectID(), schema.ObjectID()}, {column.ObjectID(), table.ObjectID()}} {
		if err := desired.AddDependency(dependency[0], dependency[1]); err != nil {
			t.Fatal(err)
		}
	}
	changes := Between(current, desired)
	batches := Schedule(current, desired, changes)
	var created []pgschema.ID
	for _, batch := range batches {
		for _, change := range batch.Changes {
			created = append(created, change.ID)
		}
	}
	want := []pgschema.ID{schema.ObjectID(), table.ObjectID(), column.ObjectID()}
	if !reflect.DeepEqual(created, want) {
		t.Fatalf("create order = %#v, want %#v", created, want)
	}

	drops := Schedule(desired, current, Between(desired, current))
	var dropped []pgschema.ID
	for _, batch := range drops {
		for _, change := range batch.Changes {
			dropped = append(dropped, change.ID)
		}
	}
	want = []pgschema.ID{column.ObjectID(), table.ObjectID(), schema.ObjectID()}
	if !reflect.DeepEqual(dropped, want) {
		t.Fatalf("drop order = %#v, want %#v", dropped, want)
	}
}

func TestBetweenIgnoresLegacyIndexDefinition(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	before := pgschema.Index{Table: table.ObjectID(), Name: "orders_id_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}, Definition: "CREATE INDEX legacy spelling"}
	after := before
	after.Definition = "CREATE INDEX differently formatted spelling"
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	if changes := Between(current, desired); len(changes) != 0 {
		t.Fatalf("legacy index definition must not create a semantic diff: %#v", changes)
	}
}
