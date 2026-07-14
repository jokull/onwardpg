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
