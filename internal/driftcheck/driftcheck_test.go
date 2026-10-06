package driftcheck

import (
	"encoding/json"
	"reflect"
	"strings"
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
	if err != nil || report.Outcome != "unsupported" || len(report.Unsupported) != 1 || report.Unsupported[0] != "unmodeled:retained" {
		t.Fatalf("unsupported state lost: %#v %v", report, err)
	}
	if err := actual.Add(pgschema.Table{Schema: "public", Name: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	report, err = Compare("primary", "head", expected, actual)
	if err != nil || report.Outcome != "unsupported" || len(report.Differences) != 1 || report.Differences[0].ObjectID != "table:public:unrelated" {
		t.Fatalf("unrelated object drift lost: %#v %v", report, err)
	}
}

func TestCompareReportsUnsupportedStateAlongsideDifferences(t *testing.T) {
	expected, actual := pgschema.New(), pgschema.New()
	for _, snapshot := range []*pgschema.Snapshot{expected, actual} {
		if err := snapshot.Add(pgschema.Table{Schema: "public", Name: "items"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := actual.Add(pgschema.Table{Schema: "public", Name: "manual"}); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"parameter_acl:session_replication_role", "ownership:extension:hypopg=pscale_admin"} {
		if err := actual.AddUnsupported(selector); err != nil {
			t.Fatal(err)
		}
	}
	if err := expected.AddUnsupported("ownership:extension:hypopg=pscale_admin"); err != nil {
		t.Fatal(err)
	}
	report, err := Compare("primary", "head", expected, actual)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ownership:extension:hypopg=pscale_admin", "parameter_acl:session_replication_role"}
	if report.Outcome != "unsupported" || !reflect.DeepEqual(report.Unsupported, want) {
		t.Fatalf("unsupported report = %#v", report)
	}
	if len(report.Differences) != 1 || report.Differences[0].Kind != "unexpected_in_actual" {
		t.Fatalf("differences were not retained: %#v", report.Differences)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"status":"unsupported"`, `"unsupported":["ownership:extension:hypopg=pscale_admin","parameter_acl:session_replication_role"]`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("report JSON %s does not contain %s", encoded, fragment)
		}
	}
}

func TestCompareWithoutUnsupportedStateOmitsTheField(t *testing.T) {
	report, err := Compare("primary", "head", pgschema.New(), pgschema.New())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "drift_free" || strings.Contains(string(encoded), "unsupported") {
		t.Fatalf("clean report = %s", encoded)
	}
}
