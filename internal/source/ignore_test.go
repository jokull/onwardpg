package source

import (
	"errors"
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

func TestSelectorMatchesKindsAndSchemaContents(t *testing.T) {
	for _, test := range []struct {
		requested, actual string
		want              bool
	}{
		{"extension:pg_stat_statements", "extension:pg_stat_statements", true},
		{"index:public.users.users_email_idx", "index:public.users.users_email_idx", true},
		{"column:public.draft.preview_token_hash", "column:public.draft.preview_token_hash", true},
		{"constraint:public.audit.audit_Mixed_fkey", "constraint:public.audit.audit_Mixed_fkey", true},
		{"constraint:public.audit.audit_mixed_fkey", "constraint:public.audit.audit_Mixed_fkey", false},
		{"table:*", "table:legacy.order_snapshot", true},
		// An ignored schema excludes the schema and every object in it.
		{"schema:legacy", "schema:legacy", true},
		{"schema:legacy", "table:legacy.order_snapshot", true},
		{"schema:legacy", "column:legacy.order_snapshot.body", true},
		{"schema:legacy", "routine:legacy.archive(integer)", true},
		{"schema:legacy", "materialized_view:legacy.totals", true},
		// It does not reach another schema with the same prefix, an extension
		// that has the schema's name, or unsupported-state markers.
		{"schema:legacy", "table:legacy_v2.order_snapshot", false},
		{"schema:legacy", "table:public.legacy", false},
		{"schema:legacy", "extension:legacy", false},
		{"schema:legacy", "aggregate:legacy.total(integer)", false},
		{"schema:*", "table:legacy.order_snapshot", false},
		{"table:legacy.order_snapshot", "column:legacy.order_snapshot.body", false},
	} {
		if got := SelectorMatches(test.requested, test.actual); got != test.want {
			t.Errorf("SelectorMatches(%q, %q) = %v, want %v", test.requested, test.actual, got, test.want)
		}
	}
}

func TestIgnoredSchemaContentsMarkTheSchemaSelectorUsed(t *testing.T) {
	snapshot := pgschema.New()
	if err := snapshot.AddIgnored("table:legacy.order_snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateIgnoreSelectors([]string{"schema:legacy"}, snapshot); err != nil {
		t.Fatal(err)
	}
	err := ValidateIgnoreSelectors([]string{"schema:legacy", "schema:archive"}, snapshot)
	if !errors.Is(err, ErrInvalidIgnore) || err.Error() != "unused ignore selectors: schema:archive" {
		t.Fatalf("stale selector error = %v", err)
	}
}
