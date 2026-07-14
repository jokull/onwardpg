package source

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestCatalogVersionPredicates(t *testing.T) {
	if got, want := routineKindPredicate(100000), "p.proisagg"; got != want {
		t.Fatalf("PostgreSQL 10 routine predicate = %q, want %q", got, want)
	}
	if got, want := routineKindPredicate(110000), "p.prokind = 'a'"; got != want {
		t.Fatalf("PostgreSQL 11 routine predicate = %q, want %q", got, want)
	}
	if !strings.Contains(unsupportedAggregatesQuery(100000), "p.proisagg") {
		t.Fatal("PostgreSQL 10 aggregate inspection must use proisagg")
	}
	if !strings.Contains(unsupportedAggregatesQuery(110000), "p.prokind = 'a'") {
		t.Fatal("PostgreSQL 11 aggregate inspection must use prokind")
	}
	if got, want := generatedColumnSelector(110000), "''::text"; got != want {
		t.Fatalf("PostgreSQL 11 generated selector = %q, want %q", got, want)
	}
	if got, want := generatedColumnSelector(120000), "a.attgenerated::text"; got != want {
		t.Fatalf("PostgreSQL 12 generated selector = %q, want %q", got, want)
	}
	if got, want := indexIncludeSelector(100000), "false"; got != want {
		t.Fatalf("PostgreSQL 10 INCLUDE selector = %q, want %q", got, want)
	}
	if got, want := indexIncludeSelector(110000), "idx.ord > idx.indnkeyatts"; got != want {
		t.Fatalf("PostgreSQL 11 INCLUDE selector = %q, want %q", got, want)
	}
	if got, want := indexNullsNotDistinctSelector(140000), "false"; got != want {
		t.Fatalf("PostgreSQL 14 NULLS NOT DISTINCT selector = %q, want %q", got, want)
	}
	if got, want := indexNullsNotDistinctSelector(150000), "idx.indnullsnotdistinct"; got != want {
		t.Fatalf("PostgreSQL 15 NULLS NOT DISTINCT selector = %q, want %q", got, want)
	}
	if got := catalogVersionSafetyBlockersQuery(140000); got != "" {
		t.Fatalf("PostgreSQL 14 version blocker query = %q, want empty", got)
	}
	if got := catalogVersionSafetyBlockersQuery(150000); !strings.Contains(got, "parameter_acl:") || strings.Contains(got, "not_null_constraint:") {
		t.Fatalf("PostgreSQL 15 version blocker query = %q", got)
	}
	if got := catalogVersionSafetyBlockersQuery(180000); !strings.Contains(got, "parameter_acl:") || !strings.Contains(got, "not_null_constraint:") {
		t.Fatalf("PostgreSQL 18 version blocker query = %q", got)
	}
}

func TestCatalogSafetyQueryNamesSecurityLabelsWithoutValues(t *testing.T) {
	if !strings.Contains(graphCatalogSafetyBlockersQuery, "pg_identify_object") ||
		!strings.Contains(graphCatalogSafetyBlockersQuery, "security_label:") {
		t.Fatal("security labels must have stable object-address blockers")
	}
	if strings.Contains(graphCatalogSafetyBlockersQuery, "s.label") {
		t.Fatal("security-label values must not be exposed in diagnostics")
	}
}

func TestNormalizePublicSchemaComment(t *testing.T) {
	standard := "standard public schema"
	if got := normalizeSchemaComment("public", &standard); got != nil {
		t.Fatalf("standard public schema comment = %#v, want nil", got)
	}
	custom := "application namespace"
	if got := normalizeSchemaComment("public", &custom); got == nil || *got != custom {
		t.Fatalf("custom public schema comment = %#v", got)
	}
	if got := normalizeSchemaComment("app", &standard); got == nil || *got != standard {
		t.Fatalf("non-public schema comment = %#v", got)
	}
}

func TestParseOptionsCanonicalizesCatalogOrder(t *testing.T) {
	options := parseOptions([]string{"pages_per_range=64", "autosummarize=true", "fillfactor=90"})
	want := []pgschema.Option{
		{Name: "autosummarize", Value: "true"},
		{Name: "fillfactor", Value: "90"},
		{Name: "pages_per_range", Value: "64"},
	}
	if len(options) != len(want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
	for i := range want {
		if options[i] != want[i] {
			t.Fatalf("option %d = %#v, want %#v", i, options[i], want[i])
		}
	}
}

func TestOutsideCoreBlockersCoverForeignTablesAndAggregates(t *testing.T) {
	for _, selector := range []string{"foreign_table:", "aggregate:"} {
		if !strings.Contains(graphOutsideCoreQuery, selector) {
			t.Fatalf("outside-core blocker query omits %q", selector)
		}
	}
}

func TestValidatePostgresVersion(t *testing.T) {
	if err := validatePostgresVersion(130000); err == nil {
		t.Fatal("PostgreSQL 13 must be rejected")
	}
	if err := validatePostgresVersion(140000); err == nil {
		t.Fatal("PostgreSQL 14 must be rejected")
	}
	if err := validatePostgresVersion(150000); err != nil {
		t.Fatalf("PostgreSQL 15 must be accepted: %v", err)
	}
	if err := validatePostgresVersion(180000); err != nil {
		t.Fatalf("PostgreSQL 18 must be accepted: %v", err)
	}
	if err := validatePostgresVersion(190000); err == nil {
		t.Fatal("PostgreSQL 19 must be rejected until explicitly supported")
	}
}

func TestIgnoreTrackerValidatesAndReportsObservedSelectors(t *testing.T) {
	tracker, err := newIgnoreTracker([]string{"policy:*", "trigger:public.orders.audit"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pgschema.New()
	for _, selector := range []string{"policy:public.orders.tenant", "trigger:public.orders.audit"} {
		skipped, err := tracker.Skip(selector, snapshot)
		if err != nil || !skipped {
			t.Fatalf("skip %q = %v, %v", selector, skipped, err)
		}
	}
	if err := tracker.Validate(); err != nil {
		t.Fatal(err)
	}
	ignored := snapshot.Ignored()
	if len(ignored) != 2 || ignored[0] != "policy:public.orders.tenant" || ignored[1] != "trigger:public.orders.audit" {
		t.Fatalf("ignored = %#v", ignored)
	}
}

func TestIgnoreTrackerRejectsMalformedAndUnusedSelectors(t *testing.T) {
	for _, selector := range []string{"policy", ":name", "policy:", "policy:foo*"} {
		if _, err := newIgnoreTracker([]string{selector}); err == nil {
			t.Fatalf("expected %q to be rejected", selector)
		}
	}
	tracker, err := newIgnoreTracker([]string{"policy:missing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Validate(); err == nil {
		t.Fatal("expected unused selector to be rejected")
	}
}
