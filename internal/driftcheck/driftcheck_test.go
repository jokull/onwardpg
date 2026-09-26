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

func TestCompareNormalizesOnlyAsymmetricIgnoreReceipts(t *testing.T) {
	expected, actual := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "items"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Type: "integer"}
	for _, snapshot := range []*pgschema.Snapshot{expected, actual} {
		for _, object := range []pgschema.Object{table, column} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := actual.AddIgnored("table:public.django_migrations"); err != nil {
		t.Fatal(err)
	}
	report, err := Compare("primary", "head", expected, actual)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "drift_free" || len(report.Differences) != 0 || report.ExpectedFingerprint != report.ActualFingerprint || len(report.Ignored) != 1 {
		t.Fatalf("receipt-only drift: %#v", report)
	}
	if len(expected.Ignored()) != 0 {
		t.Fatal("comparison mutated input evidence")
	}
	if err := actual.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	report, err = Compare("primary", "head", expected, actual)
	if err != nil || report.Outcome != "drifted" {
		t.Fatalf("dependency drift lost: %#v %v", report, err)
	}
	if err := expected.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := actual.AddUnsupported("unmodeled:retained"); err != nil {
		t.Fatal(err)
	}
	report, err = Compare("primary", "head", expected, actual)
	if err != nil || report.Outcome != "drifted" {
		t.Fatalf("unsupported drift lost: %#v %v", report, err)
	}
	if err := actual.Add(pgschema.Table{Schema: "public", Name: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	report, err = Compare("primary", "head", expected, actual)
	if err != nil || report.Outcome != "drifted" || len(report.Differences) != 1 || report.Differences[0].ObjectID != "table:public:unrelated" {
		t.Fatalf("unrelated object drift lost: %#v %v", report, err)
	}
}
