package source

import (
	"context"
	"os"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

// A column whose type is the multirange of a project range depends on that
// range, and so does its table. A column of a built-in type, an enum, or the
// range itself must keep its own dependencies.
func TestColumnOfAMultirangeTypeDependsOnTheOwningRange(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	snapshot, err := LoadDDLGraphForComparison(context.Background(), []byte(`
CREATE SCHEMA app;
CREATE TYPE app.mood AS ENUM ('calm', 'busy');
CREATE TYPE app.span AS RANGE (subtype = integer, multirange_type_name = app.spans);
CREATE TYPE app.window AS RANGE (subtype = date);
CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  note text,
  mood app.mood,
  one app.span,
  many app.spans,
  windows app.window_multirange[]
);
CREATE TABLE app.plain (id bigint PRIMARY KEY, built_in int4multirange);
`), "multirange-fixture", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := snapshot.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("fixture is unsupported: %v", unsupported)
	}
	span := pgschema.ID{Kind: pgschema.KindRange, Schema: "app", Name: "span"}
	window := pgschema.ID{Kind: pgschema.KindRange, Schema: "app", Name: "window"}
	mood := pgschema.ID{Kind: pgschema.KindEnum, Schema: "app", Name: "mood"}
	bookings := pgschema.ID{Kind: pgschema.KindTable, Schema: "app", Name: "bookings"}
	plain := pgschema.ID{Kind: pgschema.KindTable, Schema: "app", Name: "plain"}
	column := func(table pgschema.ID, name string) pgschema.ID {
		return pgschema.Column{Table: table, Name: name}.ObjectID()
	}
	depends := func(object, dependency pgschema.ID) bool {
		for _, candidate := range snapshot.Dependencies(object) {
			if candidate == dependency {
				return true
			}
		}
		return false
	}
	for _, test := range []struct {
		object, dependency pgschema.ID
		want               bool
	}{
		{column(bookings, "many"), span, true},
		{column(bookings, "windows"), window, true},
		{column(bookings, "one"), span, true},
		{column(bookings, "mood"), mood, true},
		{bookings, span, true},
		{bookings, window, true},
		{bookings, mood, true},
		{column(bookings, "many"), window, false},
		{column(bookings, "windows"), span, false},
		{column(bookings, "note"), span, false},
		{column(bookings, "id"), window, false},
		{column(plain, "built_in"), span, false},
		{plain, span, false},
		{plain, window, false},
	} {
		if got := depends(test.object, test.dependency); got != test.want {
			t.Errorf("%s depends on %s = %t, want %t (dependencies: %v)", test.object, test.dependency, got, test.want, snapshot.Dependencies(test.object))
		}
	}
}
