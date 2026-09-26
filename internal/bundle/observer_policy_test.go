package bundle

import (
	"reflect"
	"strings"
	"testing"
)

func TestObserverPolicyIsBoundByHistoryDigest(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	meta.Planner.ObserverIgnoreSelectors = []string{"table:public.django_migrations"}
	artifact, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	artifact.Manifest.Planner.ObserverIgnoreSelectors = []string{"table:public.unrelated"}
	if err := artifact.Manifest.Validate(); err == nil || !strings.Contains(err.Error(), "history entry digest") {
		t.Fatalf("observer policy mutation did not invalidate history: %v", err)
	}
}

func TestObserverPolicyValidationAndLegacyFallback(t *testing.T) {
	for _, selectors := range [][]string{{"table:*"}, {"table"}, {"table:"}, {"table:public.*"}, {"table:b", "table:a"}, {"table:a", "table:a"}} {
		meta := metadata()
		meta.Planner.ObserverIgnoreSelectors = selectors
		_, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
		if err == nil {
			t.Fatalf("invalid observer selectors accepted: %#v", selectors)
		}
	}
	receipt := PlannerReceipt{IgnoreSelectors: []string{"extension:pg_stat_statements"}}
	if !reflect.DeepEqual(receipt.ObserverIgnores(), receipt.IgnoreSelectors) {
		t.Fatal("legacy bundle observer policy changed")
	}
	receipt.ObserverIgnoreSelectors = []string{"extension:pg_stat_statements", "table:public.django_migrations"}
	if !reflect.DeepEqual(receipt.ObserverIgnores(), []string{"extension:pg_stat_statements", "table:public.django_migrations"}) {
		t.Fatal("observer union lost or duplicated configured selectors")
	}
}
