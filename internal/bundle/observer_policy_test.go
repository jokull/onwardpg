package bundle

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/scratchdb"
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

func TestScratchAdminExtensionReceiptIsBoundAndValidated(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	meta.Planner.ScratchAdminExtensions = []scratchdb.AdminExtension{{Name: "cube", Schema: "extensions"}, {Name: "earthdistance", Schema: "extensions"}}
	artifact, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	artifact.Manifest.Planner.ScratchAdminExtensions = []scratchdb.AdminExtension{{Name: "cube", Schema: "extensions"}}
	if err := artifact.Manifest.Validate(); err == nil || !strings.Contains(err.Error(), "history entry digest") {
		t.Fatalf("allowlist mutation did not invalidate history: %v", err)
	}
	plain := metadata()
	plain.HistoryParentDigest = HistoryRootDigest()
	without, err := Build(Input{Metadata: plain, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without.Files["manifest.json"]), "scratch_admin_extensions") {
		t.Fatal("an empty allowlist must not appear in the manifest, so existing receipts keep their digests")
	}
	for _, entries := range [][]scratchdb.AdminExtension{
		{{Name: "earthdistance", Schema: "extensions"}, {Name: "cube", Schema: "extensions"}},
		{{Name: "cube", Schema: "extensions"}, {Name: "cube", Schema: "extensions"}},
		{{Name: "cube", Schema: ""}},
		{{Name: "Cube", Schema: "extensions"}},
	} {
		meta := metadata()
		meta.Planner.ScratchAdminExtensions = entries
		if _, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))}); err == nil {
			t.Fatalf("invalid receipt accepted: %#v", entries)
		}
	}
}
