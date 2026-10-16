package verify

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestNonConvergentObjectsNamesDifferingPresentAndMissingObjects(t *testing.T) {
	observed, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	same := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	// ADD COLUMN appends physically; a position difference is not a difference.
	moved := same
	moved.Position = 7
	observedView := pgschema.View{Schema: "public", Name: "report", Definition: "SELECT 'a'::text AS text"}
	desiredView := observedView
	desiredView.Definition = "SELECT 'a'::text"
	extra := pgschema.Index{Table: table.ObjectID(), Name: "orders_extra_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	missing := pgschema.Index{Table: table.ObjectID(), Name: "orders_missing_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for _, object := range []pgschema.Object{table, same, observedView, extra} {
		if err := observed.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, moved, desiredView, missing} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	got := nonConvergentObjects(observed, desired)
	want := []string{
		"index:public:orders:orders_extra_idx (not in the desired schema)",
		"index:public:orders:orders_missing_idx (missing after contract)",
		"view:public:report (definition differs)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nonConvergentObjects = %#v, want %#v", got, want)
	}
	if names := nonConvergentObjects(observed, nil); names != nil {
		t.Fatalf("a nil desired snapshot must name nothing, got %#v", names)
	}
}
