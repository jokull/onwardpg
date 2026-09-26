package contractcheck

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestObserverIgnoreReceiptProjectionPreservesCatalogAndPlanningPolicy(t *testing.T) {
	expected, live := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "items"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Type: "integer"}
	for _, snapshot := range []*pgschema.Snapshot{expected, live} {
		for _, object := range []pgschema.Object{table, column} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddUnsupported("unmodeled:retained"); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddIgnored("extension:pg_stat_statements"); err != nil {
			t.Fatal(err)
		}
	}
	if err := live.AddIgnored("table:public.django_migrations"); err != nil {
		t.Fatal(err)
	}
	projected, err := withoutObserverIgnoreReceipts(live, []string{"table:public.django_migrations"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := expected.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	got, err := projected.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("observer projection changed unrelated catalog semantics: %s != %s", got, want)
	}
	if !reflect.DeepEqual(live.Ignored(), []string{"extension:pg_stat_statements", "table:public.django_migrations"}) {
		t.Fatal("original evidence mutated")
	}
	unchanged, err := withoutObserverIgnoreReceipts(live, nil)
	if err != nil || unchanged != live {
		t.Fatal("legacy receipt semantics changed")
	}
}
