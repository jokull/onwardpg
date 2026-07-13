# Performance envelope

onwardpg optimizes for deterministic correctness and reviewability, but large
schemas must remain comfortable in an agent loop.

## Planner benchmark

The repository benchmark constructs two typed graphs with 100 or 1,000 tables,
five unchanged columns per table, and one desired additive column per table. It
measures graph diff, dependency ordering, statement planning, batching, and
fingerprinting:

```sh
go test ./internal/graphplan \
  -run '^$' \
  -bench BenchmarkBuildLargeAdditiveSchema \
  -benchmem \
  -count=3
```

Observed on an Apple M1 Pro with Go 1.26:

| Shape | Time per plan | Allocated per plan | Allocations |
| --- | ---: | ---: | ---: |
| 100 tables | 7.2–7.5 ms | 4.6 MB | about 16,900 |
| 1,000 tables | 242–252 ms | 46–49 MB | about 165,650 |

The preview performance envelope for this workload is under one second and
under 100 MB allocated for 1,000 tables on comparable developer hardware.
These are planner numbers, not end-to-end latency: starting disposable
databases, executing DDL, and reading PostgreSQL catalogs usually dominate
`init`, `draft`, and `verify` wall time.

## Regression history

The initial benchmark exposed repeated `ID.String()` allocation inside graph
sort comparators. Structural typed-ID ordering reduced the 1,000-table case
from roughly 3.2 seconds and 3.8 GB allocated to the figures above without
changing deterministic graph order.

CI compiles and executes one iteration of the 1,000-table benchmark. Timing is
reported but not used as a hard shared-runner gate. Any material regression
must be investigated on stable hardware before release.

## Boundaries

This benchmark does not claim that every PostgreSQL feature has identical
cost. Deep partition hierarchies, many dependency cycles, large routine/view
bodies, and catalog round trips have different profiles. Add focused
benchmarks when those shapes become common enough to affect the developer
workflow.
