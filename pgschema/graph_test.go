package pgschema

import (
	"reflect"
	"strings"
	"testing"
)

func TestBatchesOrdersDependenciesDeterministically(t *testing.T) {
	snapshot := New()
	schema := Schema{Name: "public"}
	table := Table{Schema: "public", Name: "orders"}
	column := Column{Table: table.ObjectID(), Name: "id", Type: "bigint"}
	index := Index{Table: table.ObjectID(), Name: "orders_id_idx"}
	for _, object := range []Object{index, column, table, schema} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]ID{{table.ObjectID(), schema.ObjectID()}, {column.ObjectID(), table.ObjectID()}, {index.ObjectID(), table.ObjectID()}} {
		if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for _, batch := range snapshot.Batches() {
		if batch.Cyclic {
			t.Fatalf("unexpected cycle: %#v", batch)
		}
		for _, object := range batch.Objects {
			got = append(got, object.ObjectID().String())
		}
	}
	want := []string{"schema:public", "table:public:orders", "column:public:orders:id", "index:public:orders:orders_id_idx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %#v, want %#v", got, want)
	}
}

func TestBatchesPreserveColumnPositionsWithinOneRelation(t *testing.T) {
	snapshot := New()
	schema := Schema{Name: "public"}
	table := Table{Schema: "public", Name: "accounts"}
	first := Column{Table: table.ObjectID(), Name: "zeta", Position: 1, Type: "text"}
	second := Column{Table: table.ObjectID(), Name: "alpha", Position: 2, Type: "text"}
	for _, object := range []Object{second, first, table, schema} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]ID{
		{table.ObjectID(), schema.ObjectID()},
		{first.ObjectID(), table.ObjectID()},
		{second.ObjectID(), table.ObjectID()},
	} {
		if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for _, batch := range snapshot.Batches() {
		for _, object := range batch.Objects {
			if column, ok := object.(Column); ok {
				got = append(got, column.Name)
			}
		}
	}
	if want := []string{"zeta", "alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("column batch order = %#v, want %#v", got, want)
	}
}

func TestBatchesPreserveForeignKeyCycle(t *testing.T) {
	snapshot := New()
	a := Table{Schema: "public", Name: "accounts"}
	b := Table{Schema: "public", Name: "profiles"}
	for _, object := range []Object{a, b} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.AddDependency(a.ObjectID(), b.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddDependency(b.ObjectID(), a.ObjectID()); err != nil {
		t.Fatal(err)
	}
	batches := snapshot.Batches()
	if len(batches) != 1 || !batches[0].Cyclic || len(batches[0].Objects) != 2 {
		t.Fatalf("expected one cyclic component, got %#v", batches)
	}
}

func TestSnapshotRejectsDanglingDependencies(t *testing.T) {
	snapshot := New()
	table := Table{Schema: "public", Name: "orders"}
	if err := snapshot.Add(table); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddDependency(table.ObjectID(), ID{Kind: KindSchema, Name: "missing"}); err == nil {
		t.Fatal("expected a dangling dependency error")
	}
}

func TestProjectRemovesLeafNormalizesObjectAndFiltersUnsupported(t *testing.T) {
	snapshot := New()
	table := Table{Schema: "public", Name: "orders", Owner: "app_owner"}
	privilege := TablePrivilege{Table: table.ObjectID(), Grantee: "onwardpg_observer", Grantor: "@owner", Privilege: "SELECT"}
	for _, object := range []Object{table, privilege} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.AddDependency(privilege.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddUnsupported("ownership:schema:app=app_owner"); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddUnsupported("publication:events"); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddIgnored("comment:table:public.orders"); err != nil {
		t.Fatal(err)
	}

	projected, err := snapshot.Project(func(object Object) (Object, bool) {
		switch value := object.(type) {
		case Table:
			value.Owner = ""
			return value, true
		case TablePrivilege:
			return nil, false
		default:
			return object, true
		}
	}, func(selector string) bool { return selector != "ownership:schema:app=app_owner" })
	if err != nil {
		t.Fatal(err)
	}
	object, exists := projected.Object(table.ObjectID())
	if !exists || object.(Table).Owner != "" {
		t.Fatalf("projected table = %#v, exists=%t", object, exists)
	}
	if _, exists := projected.Object(privilege.ObjectID()); exists {
		t.Fatal("projected observer privilege was retained")
	}
	if got := projected.Unsupported(); !reflect.DeepEqual(got, []string{"publication:events"}) {
		t.Fatalf("unsupported = %#v", got)
	}
	if got := projected.Ignored(); !reflect.DeepEqual(got, []string{"comment:table:public.orders"}) {
		t.Fatalf("ignored = %#v", got)
	}
	if _, exists := snapshot.Object(privilege.ObjectID()); !exists {
		t.Fatal("projection mutated the source snapshot")
	}
}

func TestProjectRejectsRetainedDependentOfRemovedObject(t *testing.T) {
	snapshot := New()
	table := Table{Schema: "public", Name: "orders"}
	column := Column{Table: table.ObjectID(), Name: "id", Type: "bigint"}
	for _, object := range []Object{table, column} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	_, err := snapshot.Project(func(object Object) (Object, bool) {
		return object, object.ObjectID() != table.ObjectID()
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "depends on removed object") {
		t.Fatalf("error = %v", err)
	}
}

func TestProjectRejectsChangedObjectIdentity(t *testing.T) {
	snapshot := New()
	if err := snapshot.Add(Table{Schema: "public", Name: "orders"}); err != nil {
		t.Fatal(err)
	}
	_, err := snapshot.Project(func(Object) (Object, bool) {
		return Table{Schema: "public", Name: "renamed"}, true
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed object identity") {
		t.Fatalf("error = %v", err)
	}
}

func FuzzBatchesContainEveryObjectExactlyOnce(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(0xff))
	f.Add(uint8(0x2a))
	f.Fuzz(func(t *testing.T, edges uint8) {
		snapshot := New()
		objects := []Schema{{Name: "a"}, {Name: "b"}, {Name: "c"}}
		for _, object := range objects {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		pairs := [][2]int{{0, 1}, {1, 0}, {1, 2}, {2, 1}, {0, 2}, {2, 0}}
		for bit, pair := range pairs {
			if edges&(1<<bit) == 0 {
				continue
			}
			if err := snapshot.AddDependency(objects[pair[0]].ObjectID(), objects[pair[1]].ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
		seen := make(map[ID]int)
		for _, batch := range snapshot.Batches() {
			for _, object := range batch.Objects {
				seen[object.ObjectID()]++
			}
		}
		for _, object := range objects {
			if seen[object.ObjectID()] != 1 {
				t.Fatalf("object %s appeared %d times for edges %08b", object.ObjectID(), seen[object.ObjectID()], edges)
			}
		}
	})
}

func TestFingerprintIsIndependentOfInsertionOrder(t *testing.T) {
	schema := Schema{Name: "public"}
	table := Table{Schema: "public", Name: "orders"}
	left, right := New(), New()
	for _, object := range []Object{schema, table} {
		if err := left.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []Object{table, schema} {
		if err := right.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, snapshot := range []*Snapshot{left, right} {
		if err := snapshot.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	leftFingerprint, err := left.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	rightFingerprint, err := right.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatalf("fingerprints differ: %q != %q", leftFingerprint, rightFingerprint)
	}
	if len(leftFingerprint) != len("sha256:")+64 {
		t.Fatalf("unexpected fingerprint format %q", leftFingerprint)
	}
}

func TestFingerprintIgnoresPhysicalColumnPosition(t *testing.T) {
	table := Table{Schema: "public", Name: "orders"}
	left, right := New(), New()
	for _, snapshot := range []*Snapshot{left, right} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := left.Add(Column{Table: table.ObjectID(), Name: "email", Position: 2, Type: "text"}); err != nil {
		t.Fatal(err)
	}
	if err := right.Add(Column{Table: table.ObjectID(), Name: "email", Position: 7, Type: "text"}); err != nil {
		t.Fatal(err)
	}
	leftFingerprint, _ := left.Fingerprint()
	rightFingerprint, _ := right.Fingerprint()
	if leftFingerprint != rightFingerprint {
		t.Fatalf("physical column position changed semantic fingerprint: %s != %s", leftFingerprint, rightFingerprint)
	}
}

func TestUnsupportedAndIgnoredAreCanonical(t *testing.T) {
	left, right := New(), New()
	for _, selector := range []string{"policy:public.orders.p", "trigger:public.orders.t"} {
		if err := left.AddUnsupported(selector); err != nil {
			t.Fatal(err)
		}
	}
	for _, selector := range []string{"trigger:public.orders.t", "policy:public.orders.p"} {
		if err := right.AddUnsupported(selector); err != nil {
			t.Fatal(err)
		}
	}
	if err := left.AddIgnored("extension:pgcrypto"); err != nil {
		t.Fatal(err)
	}
	if err := right.AddIgnored("extension:pgcrypto"); err != nil {
		t.Fatal(err)
	}
	lf, _ := left.Fingerprint()
	rf, _ := right.Fingerprint()
	if lf != rf {
		t.Fatalf("metadata changed fingerprint order: %s != %s", lf, rf)
	}
}

func TestLegacyIndexDefinitionDoesNotAffectFingerprint(t *testing.T) {
	table := Table{Schema: "public", Name: "orders"}
	left, right := New(), New()
	for _, snapshot := range []*Snapshot{left, right} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	index := Index{Table: table.ObjectID(), Name: "orders_id_idx", Method: "btree", Parts: []IndexPart{{Column: "id"}}, Definition: "CREATE INDEX old"}
	if err := left.Add(index); err != nil {
		t.Fatal(err)
	}
	index.Definition = "CREATE INDEX new"
	if err := right.Add(index); err != nil {
		t.Fatal(err)
	}
	leftFingerprint, _ := left.Fingerprint()
	rightFingerprint, _ := right.Fingerprint()
	if leftFingerprint != rightFingerprint {
		t.Fatalf("legacy definition changed fingerprint: %s != %s", leftFingerprint, rightFingerprint)
	}
}

func TestNormalizeDefaultEquivalentTimestampForms(t *testing.T) {
	left, right := New(), New()
	table := Table{Schema: "public", Name: "orders"}
	for _, snapshot := range []*Snapshot{left, right} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	currentTimestamp, now := "CURRENT_TIMESTAMP", "now()"
	if err := left.Add(Column{Table: table.ObjectID(), Name: "created_at", Type: "timestamp with time zone", Default: &currentTimestamp}); err != nil {
		t.Fatal(err)
	}
	if err := right.Add(Column{Table: table.ObjectID(), Name: "created_at", Type: "timestamp with time zone", Default: &now}); err != nil {
		t.Fatal(err)
	}
	leftFingerprint, _ := left.Fingerprint()
	rightFingerprint, _ := right.Fingerprint()
	if leftFingerprint != rightFingerprint {
		t.Fatalf("equivalent timestamp defaults differ: %s != %s", leftFingerprint, rightFingerprint)
	}
}
