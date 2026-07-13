package graphplan

import (
	"fmt"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func BenchmarkBuildLargeAdditiveSchema(b *testing.B) {
	for _, tables := range []int{100, 1000} {
		b.Run(fmt.Sprintf("tables_%d", tables), func(b *testing.B) {
			current, desired, objects := additiveBenchmarkSnapshots(b, tables)
			b.ResetTimer()
			for range b.N {
				result, err := Build(current, desired, protocol.Answers{}, Options{})
				if err != nil {
					b.Fatal(err)
				}
				if result.Status != protocol.Planned || len(result.Statements) != tables {
					b.Fatalf("result status=%s statements=%d", result.Status, len(result.Statements))
				}
			}
			b.ReportMetric(float64(objects), "objects/plan")
		})
	}
}

func additiveBenchmarkSnapshots(tb testing.TB, tables int) (*pgschema.Snapshot, *pgschema.Snapshot, int) {
	tb.Helper()
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			tb.Fatal(err)
		}
	}
	objects := 2
	for tableIndex := range tables {
		table := pgschema.Table{Schema: "app", Name: fmt.Sprintf("entity_%04d", tableIndex)}
		for _, snapshot := range []*pgschema.Snapshot{current, desired} {
			if err := snapshot.Add(table); err != nil {
				tb.Fatal(err)
			}
			objects++
		}
		for columnIndex := range 5 {
			column := pgschema.Column{
				Table: table.ObjectID(), Name: fmt.Sprintf("value_%02d", columnIndex),
				Position: columnIndex + 1, Type: "text",
			}
			for _, snapshot := range []*pgschema.Snapshot{current, desired} {
				if err := snapshot.Add(column); err != nil {
					tb.Fatal(err)
				}
				if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
					tb.Fatal(err)
				}
				objects++
			}
		}
		added := pgschema.Column{Table: table.ObjectID(), Name: "feature_value", Position: 6, Type: "bigint"}
		if err := desired.Add(added); err != nil {
			tb.Fatal(err)
		}
		if err := desired.AddDependency(added.ObjectID(), table.ObjectID()); err != nil {
			tb.Fatal(err)
		}
		objects++
	}
	return current, desired, objects
}
