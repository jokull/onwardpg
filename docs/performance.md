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
{"timings":{"total_ms":15700,"stages":[{"name":"schema_command","count":2,"ms":9820},{"name":"export_side_effect_status","count":2,"ms":360}]}}
```

Each stage has a name, the number of times it ran, and the sum of its wall
time. A stage that contains other stages includes their time, and stages that
run at the same time each count their own time, so the stages do not add up to
`total_ms`. The report is diagnostic: no result depends on it, and the stage
names can change between versions.

| Stage | What it measures |
| --- | --- |
| `schema_command` | one run of the configured export command |
| `export_side_effect_status` | one `git status` query for the [side-effect warning](protocol.md#warnings), before the first export run or after the last |
| `scratch_create`, `scratch_drop` | create or drop one disposable database and its role |
| `ddl_execute` | load exported DDL or replayed history into a disposable database |
| `history_replay` | execute the history chain batch by batch for verification |
| `catalog_inspect` | read one catalog into a typed graph; `catalog_inspect.<part>` is one inspector |
| `plan_build`, `verify_residual_plan` | run the graph planner |
| `verify_run` | one complete verification |

### What one command runs

Measured on a schema of 271 tables, about 3,500 constraints and about 1,170
indexes (a 6,900-line DDL export that takes about 5 seconds to produce), with a
one-column change, PostgreSQL 18 in a local container, a git work tree of
50,000 tracked files, and an Apple silicon laptop that other work also used:

| Command | Export runs | `git status` queries | Disposable databases | Full history replays | Wall time |
| --- | ---: | ---: | ---: | ---: | ---: |
| `plan` (new bundle) | 2 | 2 | 4 | 3 | 16 s (27 s in a run whose export runs took 17 s, not 10) |
| `plan` (existing bundle) | 2 | 2 | 4 | 3 | 17–19 s |
| `verify` | 2 | 2 | 3 | 2 | 15 s |
| `verify --check` | 2 | 2 | 3 | 2 | 16–22 s |
| `init` | 2 | 2 | 4 | 2 | 17–18 s |

Wall time follows the load of the machine closely, because the export command
is bound by the operating system. The same commands took two to three times as
long while other work kept the machine busy.

The two export runs are the largest part and are sequential by design: the
second run is the check that the schema did not change while the command
worked. See [when the export runs](schema-inputs.md#when-the-export-runs). A
faster export command is the most effective way to make these commands
faster.

The work between the two runs overlaps where the parts do not read each
other's results:

- the first export runs while base history is replayed for planning;
- the two executions of one verification run at the same time.

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
write.

The same profile showed that a fingerprint of the checkout, which guarded each
export run, read 50,000 files one at a time, eight times for each command. It
was first made parallel and reduced to four for each command. It was then
removed: four fingerprints still took 8 to 10 seconds of a `plan` of about 25
seconds in a clean checkout, and in a long-lived checkout with 1.3 million paths and
24 GB outside `.git` and `node_modules` one fingerprint took 96 seconds (63
seconds only to list the paths). A fingerprint also failed when another
process changed any file during an export run. Two `git status` queries for
each command, about 0.2 seconds each in both checkouts, now report such
changes as a warning. In interleaved runs on the same machine, `plan` went
from 24–27 seconds to 16–19 (27 in one run, whose two export runs took 17
seconds), `verify` from 20–24 to 15, `verify --check` from 27–31 to 16–22, and
`init` from 24 to 17–18. See
[the schema export and the checkout](safety-model.md#the-schema-export-and-the-checkout).

CI compiles and executes one iteration of the 1,000-table benchmark. Timing is
reported but not used as a hard shared-runner gate. Any material regression
must be investigated on stable hardware before release.

## Boundaries

This benchmark does not claim that every PostgreSQL feature has identical
cost. Deep partition hierarchies, many dependency cycles, large routine/view
bodies, and catalog round trips have different profiles. Add focused
benchmarks when those shapes become common enough to affect the developer
workflow.
