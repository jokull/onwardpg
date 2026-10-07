package source

import (
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestMultirangeOwnersMapsEachMultirangeNameToItsRange(t *testing.T) {
	snapshot := pgschema.New()
	for _, object := range []pgschema.Object{
		pgschema.Schema{Name: "app"},
		pgschema.Schema{Name: "other"},
		pgschema.Range{Schema: "app", Name: "span", Subtype: "integer", MultirangeName: "span_multirange"},
		pgschema.Range{Schema: "other", Name: "span", Subtype: "integer", MultirangeName: "span_multirange"},
		pgschema.Range{Schema: "app", Name: "window", Subtype: "date", MultirangeName: "windows"},
		pgschema.Enum{Schema: "app", Name: "windows_enum", Labels: []string{"a"}},
	} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	owners := multirangeOwners(snapshot)
	if len(owners) != 3 {
		t.Fatalf("owners = %#v, want one entry for each range", owners)
	}
	for key, want := range map[multirangeKey]pgschema.ID{
		{schema: "app", name: "span_multirange"}:   {Kind: pgschema.KindRange, Schema: "app", Name: "span"},
		{schema: "other", name: "span_multirange"}: {Kind: pgschema.KindRange, Schema: "other", Name: "span"},
		{schema: "app", name: "windows"}:           {Kind: pgschema.KindRange, Schema: "app", Name: "window"},
	} {
		if got, exists := owners[key]; !exists || got != want {
			t.Fatalf("owner of %v = %v (found %t), want %v", key, got, exists, want)
		}
	}
	// A range name is not a multirange name, and a schema is part of the key.
	for _, key := range []multirangeKey{{schema: "app", name: "span"}, {schema: "missing", name: "span_multirange"}, {schema: "app", name: "windows_enum"}} {
		if _, exists := owners[key]; exists {
			t.Fatalf("%v must not have an owner", key)
		}
	}
}
