# Plan-loop blind gauntlet closure

Status: delivered. This is the record of the blind-agent hill climb, not a list
of unshipped promises.

## Outcome

onwardpg now has a tested path from accepted history and final DDL to one
rolling-release migration:

```text
accepted history + final DDL + explicit product decisions
                              |
                              v
                 dependency-closed transition
                              |
                   expand -> drain -> contract
```

The planner generates compatibility SQL where PostgreSQL facts are enough. If
the missing information is product meaning—a cast, cleanup rule, conflict
policy, view overlap, or materialized-view freshness choice—it creates a
bounded, named SQL pocket instead. Verification executes that exact edited
plan and checks convergence on disposable PostgreSQL.

The public promise is deliberately narrower than “automatic migrations”:

- old and new application SQL must both work after expand;
- contract may remove the old interface only after writer and data gates pass;
- an agent or developer may supply product-specific SQL, but onwardpg owns the
  dependency boundary, ordering, edit markers, hazards, and final residual;
- optional development-database reconciliation must never obscure whether the
  durable release bundle is merge-ready.

## What the blind runs changed

### Wave 1 — Permanent compatibility workloads

- [x] Run A prepares legacy SQL before expand for a same-type column rename
  beneath an intentionally changed ordinary view. It proves the old and new
  view outputs coexist, then proves final catalog convergence.
- [x] Run B covers `age_text text -> age integer`, an ordinary view, a
  materialized view, and its index. Its reviewed pockets handle conversion,
  reverse conversion, malformed values, overflow, synchronization, conflicts,
  dependency teardown, and final recreation.
- [x] Run B prepares the legacy materialized-view query before expand and
  executes both old `age_text` and new `age` queries against the overlap shape.
- [x] Run C keeps one backend alive across expand with prepared legacy
  INSERT/SELECT/UPDATE statements. It proves a rename bridge, nullable staging
  for a future required column, a loosened CHECK, contradictory-write
  rejection, writer drain, cleanup, and final enforcement.
- [x] A, B, and C are repository-owned integration tests rather than `/tmp`
  reports or prose-only examples.

### Wave 2 — One transition owns the dependency closure

- [x] Explicit column identity is independent of type equality. A precise
  rename hint can bind `age_text text` to `age integer`; automatic suggestions
  remain conservative and same-type.
- [x] A confirmed rename/type change consumes both endpoint diffs and the
  current and desired dependent closures. It no longer leaks out as an
  unrelated drop, create, and illegal view replacement.
- [x] View snapshots carry ordered output signatures. Every ordinary-view
  replacement passes one PostgreSQL legality check; an output rename or type
  change cannot reach `CREATE OR REPLACE VIEW`.
- [x] Provable append-compatible ordinary views stay generated. Intentional or
  unprovable overlap semantics get bounded expand/removal/recreation pockets,
  the exact desired view definitions, and the owned view/materialized-view/
  index closure.
- [x] Cross-name/type transitions produce exactly two product-owned pockets:
  expand establishes both physical interfaces and conversion behavior;
  contract performs catch-up and blocking assertions, removes the bridge, and
  recreates the final closure.
- [x] Materialized-view rebuild/freshness remains an explicit reviewed choice.
  The planner does not pretend an overlap aggregation is semantically
  equivalent to the final aggregation merely because both SQL interfaces work.

### Wave 3 — A plan loop an agent can drive safely

- [x] Ordinary `plan` JSON is a compact decision envelope: stable plan
  identity, generation, durable/development outcomes, findings, written edit
  paths, verification summary, and executable next actions.
- [x] Cross-type identity ambiguity is raised before a destructive drop choice,
  with an exact copyable rename hint. The finding disappears once consumed and
  is present in both JSON and text output.
- [x] Edit actions are emitted only for files that were actually written.
  Product assertions live in the named phase pocket that verification runs.
- [x] Rebase reruns retain the PlanID and fingerprint-scoped decisions when
  upstream history changes without changing their meaning.
- [x] Edited-plan conflicts include the path, current SQL, newly generated SQL,
  merge instructions, and exact verification command.
- [x] A scratch-only target is valid. With no development database configured,
  `config check` succeeds and `plan` reports
  `development.status: not_available`.
- [x] A ready durable plan stays top-level ready when optional development
  reconciliation is unsupported. Genuine development findings remain visible
  and scoped.
- [x] Read-only observer ownership and minimum inspection grants are projected
  explicitly rather than misreported as drift. Real application grants and
  authorization changes remain visible.
- [x] An empty verified residual is reported as `converged`.

### Wave 4 — Ground the public narrative

- [x] README, compatibility, workflow, readiness, safety, agent, plan-command,
  expand-contract, introduction, and comparison documentation distinguish
  generated SQL from dependency-scoped human/agent handoff.
- [x] Required-column, rename, type-change, and dependency-type-change examples
  are checked against current generated phase files.
- [x] Documentation explains that cleanup values are product decisions, not
  values inferred from declarative DDL.
- [x] Documentation explains the evolving plan: plain `plan` reruns restack the
  same feature plan when a rebase introduces a new accepted-history chain.
- [x] CLI documentation is checked from the real help surface, and the Blume
  website emits human pages plus raw Markdown/agent artifacts.

## Shipping evidence

| Gate | Result |
| --- | --- |
| A/B/C and focused view-legality tests | pass on PostgreSQL 15.18, 16.14, 17.10, and 18.4 |
| Strengthened B old/new materialized-view receipt | pass on PostgreSQL 15–18 |
| Full graph planner and contract-check integration suites | pass on PostgreSQL 18.4 |
| Differential suite | pass on PostgreSQL 18.4 |
| Full CLI integration suite | pass on PostgreSQL 18.4 |
| `go test ./... -count=1` without a database | pass |
| `go vet ./...` | pass |
| README and documentation SQL receipts | pass |
| Documentation consistency checker | pass |
| Blume strict check and production build | pass |
| Three final source-blind DX retests | pass |
| Independent PostgreSQL review | no release blocker after the strengthened materialized-view receipt |
| Disposable container audit | no matrix or blind-run containers/volumes remain |

The source-blind retests specifically confirmed compact output, exact hints,
scratch-only configuration, same-PlanID restacking, decision retention, exact
desired view DDL, actionable edited-plan conflict recovery, and dedicated
read-only observer access.

## Honest remaining edges

These are real boundaries, but they do not invalidate the delivered release:

- Adding a column to the same table can change a contract gate fingerprint.
  Authored SQL is not silently absorbed; the developer or agent must merge the
  current and regenerated phase using the now-complete conflict payload.
- A materialized overlap grouped by both old and new values proves interface
  compatibility, not equivalence to the final aggregation when conversion is
  non-injective. The reviewed pocket or `split_plan` owns that product choice.
- Observer projections are intentionally visible as informational findings.
  This is somewhat noisy, but makes the security normalization auditable.
- Verification proves SQL execution and catalog convergence. It cannot prove
  production capacity, acceptable lock duration, replica lag, application
  correctness, or that every old process has drained.
- Prepared-statement receipts use explicit target lists. Rigid `SELECT *`
  consumers remain an application compatibility concern.

## Future hills, not part of this delivery

- Reduce same-table rebase conflicts by making more contract gate identities
  semantic without weakening invalidation.
- Improve presentation of observer projection evidence if real usage shows it
  obscures more important drift.
- Add exhaustive ORM/framework harnesses only when they expose planner gaps not
  already represented by SQL workloads.
- Add deployment, queue, preview-environment, replica-lag, and capacity
  simulations as separate operational goals.
- Broaden automatic dependency handling only where PostgreSQL catalog facts
  prove the transformation. Do not add a general SQL rewriter, new lifecycle
  commands, or output protocol versions.

The next useful goal should start from one of those measured edges. It should
not reopen this release for cosmetic protocol polish or framework busywork.
