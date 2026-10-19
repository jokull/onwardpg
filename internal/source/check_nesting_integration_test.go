package source

import (
	"context"
	"os"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

// BETWEEN keeps its own AND node, so the source catalog deparses a CHECK,
// an index predicate or expression, and a generated column nested. A dump of
// that catalog restores them flat. Both catalogs hold the same expressions and
// must inspect to the same graph.
func TestExpressionNestingFromADumpRestoreIsNotADifference(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	load := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		snapshot, err := LoadDDLGraphForComparison(context.Background(), []byte(ddl), "check-nesting-fixture", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	source := load(`CREATE TABLE public.slots (x integer, y integer, z integer,
  open boolean GENERATED ALWAYS AS (x BETWEEN 1 AND 5 AND y > 0) STORED,
  CONSTRAINT slots_window CHECK (x BETWEEN 1 AND 5 AND y > 0),
  CONSTRAINT slots_either CHECK (x > 0 OR (y > 0 OR z > 0)));
CREATE INDEX slots_partial ON public.slots (x) WHERE x BETWEEN 1 AND 5 AND y > 0;
CREATE INDEX slots_expression ON public.slots ((x BETWEEN 1 AND 5 AND y > 0));`)
	restored := load(`CREATE TABLE public.slots (x integer, y integer, z integer,
  open boolean GENERATED ALWAYS AS ((((x >= 1) AND (x <= 5)) AND (y > 0))) STORED,
  CONSTRAINT slots_window CHECK ((((x >= 1) AND (x <= 5)) AND (y > 0))),
  CONSTRAINT slots_either CHECK (((x > 0) OR (y > 0) OR (z > 0))));
CREATE INDEX slots_partial ON public.slots (x) WHERE (((x >= 1) AND (x <= 5)) AND (y > 0));
CREATE INDEX slots_expression ON public.slots (((((x >= 1) AND (x <= 5)) AND (y > 0))));`)
	table := pgschema.Table{Schema: "public", Name: "slots"}.ObjectID()
	for name, want := range map[string]string{
		"slots_window": "CHECK (((x >= 1) AND (x <= 5) AND (y > 0)))",
		"slots_either": "CHECK (((x > 0) OR (y > 0) OR (z > 0)))",
	} {
		id := pgschema.Constraint{Table: table, Name: name}.ObjectID()
		for label, snapshot := range map[string]*pgschema.Snapshot{"source": source, "restored": restored} {
			object, ok := snapshot.Object(id)
			if !ok {
				t.Fatalf("%s: %s missing", label, id)
			}
			if got := object.(pgschema.Constraint).Definition; got != want {
				t.Fatalf("%s: %s definition = %q, want %q", label, name, got, want)
			}
		}
	}
	left, err := source.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	right, err := restored.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("fingerprints differ: %s %s", left, right)
	}
}
