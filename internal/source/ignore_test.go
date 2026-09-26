package source

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestActiveIgnoreSelectorsReturnsOnlySelectorsUsedByComparison(t *testing.T) {
	current := pgschema.New()
	desired := pgschema.New()
	if err := current.AddIgnored("extension:pg_stat_statements"); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddIgnored("domain:extensions.earth"); err != nil {
		t.Fatal(err)
	}
	active, err := ActiveIgnoreSelectors([]string{
		"schema:auth",
		"domain:*",
		"extension:pg_stat_statements",
		"domain:*",
	}, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"domain:*", "extension:pg_stat_statements"}
	if !reflect.DeepEqual(active, want) {
		t.Fatalf("active ignores = %#v, want %#v", active, want)
	}
}

func TestActiveIgnoreSelectorsStillRejectsMalformedPolicy(t *testing.T) {
	if _, err := ActiveIgnoreSelectors([]string{"extension:pg*"}, pgschema.New()); err == nil {
		t.Fatal("expected malformed ignore selector to fail")
	}
}

func TestTableIgnoreRequiresObservedTableExclusion(t *testing.T) {
	table := (pgschema.Table{Schema: "public", Name: "django_migrations"}).ObjectID()
	for _, selector := range []string{"table:public.django_migrations", "table:*"} {
		t.Run(selector, func(t *testing.T) {
			tracker, err := newIgnoreTracker([]string{selector})
			if err != nil {
				t.Fatal(err)
			}
			if tracker.tableIgnored(table) {
				t.Fatal("an unobserved selector cannot suppress missing table metadata")
			}
			snapshot := pgschema.New()
			skipped, err := tracker.Skip("table:public.django_migrations", snapshot)
			if err != nil || !skipped {
				t.Fatalf("Skip = %v, %v", skipped, err)
			}
			if !tracker.tableIgnored(table) {
				t.Fatal("observed table exclusion was not recorded")
			}
			if tracker.tableIgnored((pgschema.Table{Schema: "public", Name: "application_items"}).ObjectID()) {
				t.Fatal("an unrelated table was treated as excluded")
			}
			if tracker.tableIgnored((pgschema.View{Schema: "public", Name: "django_migrations"}).ObjectID()) {
				t.Fatal("table exclusion also excluded a view")
			}
			if got := snapshot.Ignored(); !reflect.DeepEqual(got, []string{"table:public.django_migrations"}) {
				t.Fatalf("ignore receipt = %#v", got)
			}
		})
	}
}
