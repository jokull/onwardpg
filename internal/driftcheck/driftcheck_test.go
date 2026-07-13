package driftcheck

import (
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestCompareClassifiesMissingUnexpectedAndChangedObjects(t *testing.T) {
	expected, actual := pgschema.New(), pgschema.New()
	for _, object := range []pgschema.Object{
		pgschema.Schema{Name: "app"},
		pgschema.Table{Schema: "app", Name: "users"},
		pgschema.Column{Table: pgschema.ID{Kind: pgschema.KindTable, Schema: "app", Name: "users"}, Name: "email", Type: "text", Position: 1},
	} {
		if err := expected.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{
		pgschema.Schema{Name: "app"},
		pgschema.Table{Schema: "app", Name: "users"},
		pgschema.Column{Table: pgschema.ID{Kind: pgschema.KindTable, Schema: "app", Name: "users"}, Name: "email", Type: "citext", Position: 1},
		pgschema.Table{Schema: "app", Name: "manual_table"},
	} {
		if err := actual.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Compare("primary", "sha256:head", expected, actual)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "drifted" || len(report.Differences) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if report.Differences[0].Kind != "unexpected_in_actual" && report.Differences[1].Kind != "unexpected_in_actual" {
		t.Fatalf("missing unexpected classification: %#v", report.Differences)
	}
}
