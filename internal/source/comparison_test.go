package source_test

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

func TestAlignIgnoreReceiptsPreservesCatalogDifferences(t *testing.T) {
	for _, scenario := range []string{"receipt only", "dependency", "unsupported", "object"} {
		t.Run(scenario, func(t *testing.T) {
			left, right := pgschema.New(), pgschema.New()
			table := pgschema.Table{Schema: "public", Name: "items"}
			column := pgschema.Column{Table: table.ObjectID(), Name: "id", Type: "integer", Position: 1}
			for _, snapshot := range []*pgschema.Snapshot{left, right} {
				for _, object := range []pgschema.Object{table, column} {
					if err := snapshot.Add(object); err != nil {
						t.Fatal(err)
					}
				}
			}
			const selector = "table:public.django_migrations"
			if err := left.AddIgnored(selector); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "dependency":
				if err := right.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
					t.Fatal(err)
				}
			case "unsupported":
				if err := right.AddUnsupported("unmodeled:retained"); err != nil {
					t.Fatal(err)
				}
			case "object":
				// Receipt alignment must never remove an actual object, even one
				// named by the other snapshot's receipt.
				if err := right.Add(pgschema.Table{Schema: "public", Name: "django_migrations"}); err != nil {
					t.Fatal(err)
				}
			}
			beforeLeft, _ := left.Fingerprint()
			beforeRight, _ := right.Fingerprint()
			for _, pair := range [][2]*pgschema.Snapshot{{left, right}, {right, left}} {
				alignedLeft, alignedRight, err := source.AlignIgnoreReceipts(pair[0], pair[1])
				if err != nil {
					t.Fatal(err)
				}
				plan, err := graphplan.Build(alignedLeft, alignedRight, protocol.Answers{}, graphplan.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(plan.Ignored, []string{selector}) {
					t.Fatalf("lost ignore evidence: %#v", plan)
				}
				switch scenario {
				case "receipt only":
					if plan.Status != protocol.Planned || len(plan.Statements) != 0 || plan.CurrentFingerprint != plan.DesiredFingerprint {
						t.Fatalf("receipt-only difference: %#v", plan)
					}
				case "dependency":
					if plan.Status != protocol.Unsupported || !reflect.DeepEqual(plan.Unsupported, []string{"dependency_only_graph_difference"}) {
						t.Fatalf("dependency difference lost: %#v", plan)
					}
				case "unsupported":
					if plan.Status != protocol.Unsupported || !reflect.DeepEqual(plan.Unsupported, []string{"unmodeled:retained"}) {
						t.Fatalf("unsupported marker lost: %#v", plan)
					}
				case "object":
					if plan.CurrentFingerprint == plan.DesiredFingerprint || (plan.Status == protocol.Planned && len(plan.Statements) == 0) {
						t.Fatalf("object difference lost: %#v", plan)
					}
				}
			}
			afterLeft, _ := left.Fingerprint()
			afterRight, _ := right.Fingerprint()
			if beforeLeft != afterLeft || beforeRight != afterRight || len(right.Ignored()) != 0 {
				t.Fatal("comparison mutated durable snapshot evidence")
			}
			if err := source.ValidateIgnoreSelectors([]string{"table:public.django_migrationz"}, left, right); err == nil {
				t.Fatal("explicit selector typo accepted")
			}
		})
	}
}
