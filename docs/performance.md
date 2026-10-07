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

## End-to-end stage timings

Set `ONWARDPG_TIMINGS=1` to see where one command spends its time. The command
then writes one JSON line to standard error after its normal output:

```sh
ONWARDPG_TIMINGS=1 onwardpg plan add-settlement 2>timings.json
```

```json
{"timings":{"total_ms":21700,"stages":[{"name":"schema_command","count":2,"ms":9890},{"name":"input_tree_digest","count":4,"ms":8560}]}}
```

Each stage has a name, the number of times it ran, and the sum of its wall
time. A stage that contains other stages includes their time, and stages that
run at the same time each count their own time, so the stages do not add up to
`total_ms`. The report is diagnostic: no result depends on it, and the stage
names can change between versions.

| Stage | What it measures |
| --- | --- |
| `schema_command` | one run of the configured export command |
| `input_tree_digest` | one fingerprint of the checkout, before or after an export run |
| `scratch_create`, `scratch_drop` | create or drop one disposable database and its role |
| `ddl_execute` | load exported DDL or replayed history into a disposable database |
| `history_replay` | execute the history chain batch by batch for verification |
| `catalog_inspect` | read one catalog into a typed graph; `catalog_inspect.<part>` is one inspector |
| `plan_build`, `verify_residual_plan` | run the graph planner |
| `verify_run` | one complete verification |

### What one command runs

Measured on a schema of 271 tables, about 3,500 constraints and about 1,170
indexes (a 6,900-line DDL export that takes about 5 seconds to produce), with a
one-column change, PostgreSQL 18 in a local container, a checkout of 50,000
files, and an Apple silicon laptop that other work also used:

| Command | Export runs | Checkout fingerprints | Disposable databases | Full history replays | Wall time |
| --- | ---: | ---: | ---: | ---: | ---: |
| `plan` (new bundle) | 2 | 4 | 4 | 3 | 22 s |
| `plan` (existing bundle) | 2 | 4 | 4 | 3 | 27 s |
| `verify` | 2 | 4 | 3 | 2 | 21 s |
| `verify --check` | 2 | 4 | 3 | 2 | 25 s |
| `init` | 2 | 4 | 4 | 2 | 25 s |

The two export runs are the largest part and are sequential by design: the
second run is the check that the schema did not change while the command
worked. See [when the export runs](schema-inputs.md#when-the-export-runs). A
faster export command is the most effective way to make these commands
faster.

The work between the two runs overlaps where the parts do not read each
other's results:

- the first export runs while base history is replayed for planning;
- the checkout fingerprint after the first export runs while the DDL is loaded
  into disposable PostgreSQL;
- the two executions of one verification run at the same time.

The checkout fingerprint before the second export is not taken early. It must
describe the checkout at the moment that run starts: a fingerprint from before
verification could not tell an edit during verification from a write by the
export command.

onwardpg keeps no replayed database between commands. A replay of this history
takes about 2 seconds and runs while the export command runs, so a cache of
replayed databases would save little here, and it would add state that can go
stale.

## Regression history

The initial benchmark exposed repeated `ID.String()` allocation inside graph
sort comparators. Structural typed-ID ordering reduced the 1,000-table case
from roughly 3.2 seconds and 3.8 GB allocated to the figures above without
changing deterministic graph order.

A later end-to-end profile of the schema in the table above found two more
quadratic paths. The column inspector searched every snapshot object once for
each column to resolve multirange types, which cost about 7 seconds for each
catalog read; it now builds that index once. `Snapshot.Objects` and
`Snapshot.IDs` sorted every ID on each call, and planner loops call them once
for each table; the snapshot now keeps its canonical order until the next
write. The same profile showed that the checkout fingerprint read 50,000 files
one at a time, eight times for each command; it now reads up to 16 files at
the same time, four times for each command.

CI compiles and executes one iteration of the 1,000-table benchmark. Timing is
reported but not used as a hard shared-runner gate. Any material regression
must be investigated on stable hardware before release.

## Boundaries

This benchmark does not claim that every PostgreSQL feature has identical
cost. Deep partition hierarchies, many dependency cycles, large routine/view
bodies, and catalog round trips have different profiles. Add focused
benchmarks when those shapes become common enough to affect the developer
workflow.
