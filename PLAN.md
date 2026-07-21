# Release-contract test kit milestone

Status: core milestone implemented; remaining canonical families and adversarial
restack variants are explicitly retained below as follow-up coverage.

## Delivery ledger

Delivered in this milestone:

- a compiled-binary acceptance harness with isolated native PostgreSQL
  workload databases, exact phase execution, prepared legacy clients, edited
  pockets, writer evidence, dedicated read-only observation, and redacted CI
  artifacts;
- native release journeys for nullable additions, future-required columns,
  bidirectional same-type renames, contract refusal/readiness, renderer parity,
  development ahead-work preservation, branch switching, partial/full
  absorption, safe edited-pocket retention, and an actionable development
  manual-SQL handoff in both JSON and text;
- the mandatory two-pass semantic zero-drift oracle, backed by the production
  catalog loader, graph planner, equivalence rules, and fingerprints;
- 84 capability-registered PGlite variants paired to native owners across six
  core phase/client families;
- a machine-validated claim-to-receipt inventory and separate PostgreSQL 15–18
  acceptance and PGlite CI jobs; and
- hermetic website installation/build receipts plus one full local shipping
  command; and
- two fresh source-blind release simulations using real prepared legacy
  statements, followed by an independent PostgreSQL audit; the only blocking
  finding (a development `needs_sql_edits` response without a next action) now
  has a compiled regression receipt.

Still future work—not represented as delivered:

- compiled-CLI canonical journeys for independent table/index additions,
  CHECK cleanup, dependent ordinary and
  materialized views, reviewed cross-type conversion, and the composite
  nightmare (the lower native PostgreSQL receipts remain required and mapped);
- deliberately conflicting edited pockets, dependency-closure invalidation,
  endpoint/type decision invalidation, and multi-bundle reorder restacks;
- a broader table-driven renderer parity matrix and the remaining deliberately
  weakened harness mutations; and
- repeat source-blind framework cohorts and an independent final PostgreSQL
  review after those additional journeys land.

## Why this milestone exists

onwardpg does not lack PostgreSQL integration tests. It already has extensive
catalog convergence coverage, differential tests, a PostgreSQL 15–18 matrix,
CLI tests, documentation receipts, and three demanding rollout gauntlets.

The missing layer is a permanent suite that tests the whole product promise as
one contract:

```text
compiled onwardpg binary
        |
        v
plan -> decisions -> edited pockets -> plain plan reruns
        |                                  |
        v                                  v
      expand                         rebase/restack
        |
        +---- legacy application still works
        +---- new application works
        |
        v
contract readiness refuses premature cleanup
        |
        v
writer drain + data evidence -> contract -> final convergence
```

Today most tests prove one or two boxes. Blind agents keep finding faults in the
joins between them: a warning present in JSON but absent from text, an optional
configuration accepted by parsing but rejected by a later command, a
materialized-view test that checked row counts but never queried the new
column, or an edit pocket that existed without giving the developer enough
information to complete it.

The goal is not to replace blind testing. It is to turn the recurring blind
test model into a first-class, repeatable acceptance kit so blind agents spend
their time finding genuinely new failures instead of rediscovering omitted
product invariants.

## Definition of success

For every canonical rolling-release scenario, the suite must prove:

```text
after expand:
  prepared legacy SQL succeeds
  new application SQL succeeds
  temporary schema invariants hold

before contract:
  missing writer or data evidence blocks cleanup

after contract:
  new application SQL succeeds
  onwardpg's semantic diff against the declared destination is empty
  compatibility artifacts are gone
```

It must reach that result through the public compiled CLI and persisted bundle,
not by calling the planner directly or replacing an in-memory plan with
test-authored SQL.

## Scenario and variant scope

The suite distinguishes three things which must not be counted as equivalent:

- A **scenario family** is a product story, such as adding a required column,
  renaming beneath a view, or restacking after a rebase.
- A **native acceptance variant** drives the complete compiled CLI and native
  PostgreSQL lifecycle. It is an authoritative product receipt.
- A **PGlite variant** applies an already-generated phase shape and exercises a
  focused data/client contract inside PGlite's declared capabilities. It adds
  cheap breadth, but is not another full release receipt.

The first milestone targets roughly 12–16 canonical native journeys, plus a
native rebase matrix of roughly 10–15 focused variants. Running the applicable
journeys across PostgreSQL 15–18 should produce approximately 60–100
authoritative cases. PGlite should add roughly 80–120 small table-driven
DDL/data/client variants. These are coverage budgets, not quotas: every case
must protect a distinct invariant.

| Family | Native acceptance scope | PGlite variant scope |
| --- | --- | --- |
| Additive schema | Nullable column; independent table/index | Identifier quoting, defaults, empty/nonempty tables, insert/select/update shapes |
| Required column | Nullable expand, product cleanup, NULL gate, final `NOT NULL` | Empty/all-NULL/mixed/already-clean data, cleanup expressions, old writers omitting the column |
| CHECK transition | Expand-only widening; widening then cleanup/narrowing | Named/unnamed checks, reordered values, old/new/mixed rows, boundaries and rejected values |
| Same-type rename | Full bidirectional bridge and native final rename | NULLs, old/new/matching/conflicting writes, multi-row updates, quoted names |
| Rename beneath views | Prepared old view plus appended new interface | Direct/two-level views, aliases, output order, appended columns, explicit projections |
| Type conversion | Reviewed manual conversion and final convergence | NULL/blank/malformed/negative/boundary/overflow values, injective/non-injective mappings |
| Materialized dependency | Reviewed overlap, freshness boundary, rebuild/index | Old/new projections, grouping variants, empty/mixed data, aggregation warnings |
| Rebase/restack | Dedicated variant matrix described below | Replay resulting phase/client SQL only; PGlite never proves restacking |
| Development/observer | Absent, behind, ahead, unrelated objects, read-only observer | None: role/backend fidelity requires native PostgreSQL |
| Composite nightmare | Rename + required column + CHECK + view + drain | Selected core phase/client combinations, never drain or concurrency proof |

Variants are selected across six axes:

1. **DDL shape:** schema qualification, quoted/reserved identifiers, defaults,
   nullability, named objects, and dependency depth.
2. **Data state:** empty, single, mixed, NULL, duplicates, malformed, boundary,
   overflow, and already-converged data.
3. **Client contract:** prepared/unprepared, read/write, old-only, new-only,
   matching dual input, contradictory dual input, and explicit view outputs.
4. **Product decision:** generated path, reviewed manual pocket,
   single-transaction consent, or split-plan refusal.
5. **Plan evolution:** first plan, plain rerun, edited rerun, upstream restack,
   scoped conflict, absorption, and branch switch.
6. **Environment:** PostgreSQL major, development availability, observer mode,
   and authorization differences.

Do not take the Cartesian product. Use pairwise combinations where dimensions
are independent, plus hand-picked triples for historically dangerous joins.
PGlite expands axes 1–3 and the SQL portion of axis 4. Native PostgreSQL and the
compiled binary own axes 5–6.

### Rebase scenario variants

Rebase behavior is a first-class acceptance family, not one checkbox on the
composite scenario. Begin with these upstream-change classes:

| Upstream change | Required result |
| --- | --- |
| Additive object on another table | Same PlanID; generation advances; all semantic decisions and edits survive |
| Additive column on the transition's table | Same PlanID; decisions survive if their semantic scope is unchanged; regenerated gates may require an explicit edit merge |
| New index or constraint depending on the transition | Dependency closure changes; only closure-bound decisions invalidate |
| Desired view changes outside the owned output | Unrelated decisions survive; view-bound pocket or decision restacks |
| Endpoint name or type changes | Identity/type decisions invalidate and are asked again before destructive SQL |
| Upstream introduces part of the feature destination | Already-landed work is absorbed; the remaining plan shrinks without duplicating DDL |
| Upstream fully satisfies the feature destination | Generated feature bundle is absorbed/removed and the workspace still fast-forwards safely |
| Generated SQL changes around an untouched edited pocket | Authored SQL is retained when its pocket fingerprint still matches |
| Both generated pocket meaning and authored SQL change | Plan blocks with path, current SQL, regenerated SQL, merge guidance, and a working verify command |
| Accepted chain is reordered or gains multiple bundles | Restack follows the new head deterministically; generation advances once per material change |
| Development DB contains abandoned branch work | Durable history excludes it; development reconciliation preserves or reports it explicitly |
| Switch from a newer branch to an older branch | No reverse migration is invented; accepted work is distinguished from branch-local/ahead state |

Each rebase variant must assert:

- stable feature identity unless the feature is fully absorbed;
- the expected generation transition;
- the exact retained, invalidated, deferred, or conflicted decisions;
- unchanged developer SQL byte-for-byte when safe;
- exact remediation when it is not safe;
- correct workspace fast-forward SQL without absorbing ahead work;
- old/new client compatibility for the resulting phases; and
- final convergence from the newly replayed history.

The rebase itself always runs through the compiled binary and native
PostgreSQL. When the resulting phase shape is PGlite-compatible, the same phase
may additionally fan out across PGlite data/client variants. That increases
confidence in the restacked SQL, but does not count as another rebase receipt.

Every scenario or variant must link to the invariant it protects, its native
owner, required engine capabilities, exact client assertion, and the blind
incident, public claim, or PostgreSQL risk that justifies it. A PGlite finding
must be promoted to a minimal native regression before the product is fixed.

## Final zero-drift oracle

The final pass for every supported native acceptance scenario uses onwardpg's
own source normalization and semantic diff engine:

```text
declared destination DDL ---- materialize/inspect ---- desired graph
                                                        |
                                                        v
accepted history + edited plan ---- apply ---- final live graph
                                                        |
                                                        v
                                             semantic residual = empty
```

The harness must not implement a second schema comparator. It should invoke the
same graph loading, normalization, dependency modeling, equivalence rules, and
residual planner used by the product. The final result is acceptable only when:

- the desired DDL materializes successfully on a separate clean scratch
  database;
- the contracted workload database is inspected through the production catalog
  path;
- the semantic diff contains zero statements, zero questions, zero manual edit
  requirements, and zero unsupported or ignored differences;
- the report says `converged` and exposes both final fingerprints as equal;
- no transition-owned bridge column, trigger, function, overlap view,
  materialized overlap, or temporary index remains; and
- running the diff a second time is stable and still empty.

The explicit compatibility-artifact check remains useful even with an empty
modeled graph diff: it catches accidentally unmodeled or filtered temporary
objects and forces the test kit to improve catalog coverage instead of silently
accepting them.

Zero schema drift does not replace client and data assertions. A schema can be
perfect while a backfill is semantically wrong, a materialized result is stale,
or a prepared query is unusable. Acceptance therefore requires both:

```text
application oracle: legacy/new/final SQL and data expectations
schema oracle:      onwardpg semantic residual is empty
```

PGlite may run a cheap preliminary graph/diff experiment for its PostgreSQL 17
subset, but it cannot own this checkpoint. The authoritative zero-drift pass is
always native PostgreSQL because catalog versions, database isolation, roles,
and dependency facts are part of the oracle.

## Design constraints

- Keep the existing unit, convergence, differential, and parity suites. They
  are valuable lower layers; this milestone fills a gap above them.
- Exercise a binary built from `cmd/onwardpg`, with real argv, environment,
  stdout, stderr, exit codes, files, and PostgreSQL connections.
- Use the existing Go testing stack, `pgx`, `scratchdb`, bundle edit machinery,
  and verification/batch execution code. Do not build a second SQL parser,
  phase runner, migration DSL, or Docker orchestrator.
- Let CI provide the PostgreSQL administrative URL. The suite creates uniquely
  named disposable databases and roles beneath it, applies timeouts, and always
  cleans them up.
- Keep scenario intent in typed Go and readable SQL fixtures. Do not invent a
  YAML language for callbacks, prepared statements, result matching, or
  PostgreSQL error expectations.
- Product-specific cleanup and conversion remain explicit fixture inputs. The
  harness tests onwardpg's handoff and safety boundary; it does not invent
  product meaning.
- A scenario is not complete merely because its final fingerprint converges.
  Intermediate legacy and new client contracts are mandatory.
- Avoid combinatorial busywork. Canonical scenarios should compose important
  boundaries; the existing catalog matrix continues to cover isolated object
  variants.

## PGlite research decision

PGlite earns a deliberately scoped fast lane, not authority over the release
contract.

The July 2026 probe used `@electric-sql/pglite` 0.4.5 through
`@electric-sql/pglite-socket` 0.1.5. It exposed PostgreSQL 17.5 over a normal
loopback PostgreSQL URL, and a pgx client could execute core DDL, use two
logical connections, prepare legacy SQL before an additive change, and execute
that same statement afterward.

The same probe also established the boundary:

- PGlite is a single-user/single-connection PostgreSQL WASM build. The socket
  server multiplexes logical clients over that connection and explicitly does
  not promise normal multi-connection behavior.
- Both logical pgx clients reported the same backend PID and
  `pg_stat_activity` contained one backend. It therefore cannot prove writer
  drain, backend identity, lock interaction, or actual concurrent sessions.
- Default pgx statement caching collided through the multiplexer with
  `prepared statement ... already exists`. Disabling automatic statement
  caching allowed the narrower prepared-statement probe to proceed, but is a
  PGlite-specific compatibility mode rather than production fidelity.
- onwardpg `config check` cannot currently use PGlite as its scratch
  administrator. The restricted role/per-database lifecycle in `scratchdb`
  assumes real PostgreSQL cluster semantics; PGlite's selected single database
  does not provide equivalent isolation even though parts of `CREATE DATABASE`
  and role DDL parse.
- The tested PGlite release represents PostgreSQL 17, not the supported
  PostgreSQL 15–18 catalog matrix. SSL is also unsupported by the socket
  server, and extensions exist only when their WASM builds are loaded.

Use PGlite for fast, sequential SQL-interface preflight:

- core table, column, default, CHECK, trigger, ordinary-view, and selected
  materialized-view phase SQL;
- old and new query shape checks where true concurrency and backend identity
  are not part of the claim;
- harness self-tests and deliberately broken overlap SQL; and
- local/agent feedback when no native PostgreSQL service is available.

Never use PGlite as the only receipt for:

- the compiled CLI's scratch/history/verify lifecycle;
- prepared legacy sessions as distinct PostgreSQL backends;
- writer drain or `pg_stat_activity` evidence;
- locks, concurrent indexes, transactional interleavings, failure recovery, or
  connection termination;
- roles, ACLs, ownership, RLS observer boundaries, or database isolation;
- PostgreSQL 15, 16, or 18 catalog behavior; or
- extension behavior without a separately loaded and paired native receipt.

Every PGlite scenario therefore declares capabilities and has a paired native
PostgreSQL acceptance owner. PGlite may make the first failure faster; it may
not turn a native failure into a green release.

## Target test architecture

Add two private test layers:

```text
internal/testkit/
  binary.go          build once; run commands with captured process results
  postgres.go        disposable databases, roles, sessions, cleanup, timeouts
  pglite.go          pinned socket subprocess for scoped sequential preflight
  capabilities.go    explicit engine eligibility; no silent skip/fallback
  workspace.go       config, desired DDL, history and bundle inspection
  phases.go          reuse onwardpg batch execution for exact phase files
  workload.go        prepared SQL, query/row assertions, SQLSTATE assertions
  residual.go        production graph inspection and mandatory empty final diff
  output.go          compact JSON/text action and status assertions

acceptance/
  release_contract_test.go
  pglite_preflight_test.go
  plan_loop_test.go
  renderer_parity_test.go
  harness_self_test.go
  testdata/<scenario>/
    baseline.sql
    desired.sql
    expand.reviewed.sql       # only when a named pocket requires product SQL
    contract.reviewed.sql     # only when a named pocket requires product SQL
```

`internal/testkit` is reusable test machinery, not a public library. The
`acceptance` package is the required product boundary. It should build the
binary once in `TestMain`, while every scenario receives isolated databases,
workspace files, and connections.

## Wave 0 — Make the test environment honest

The current `main` CI run has all PostgreSQL 15–18 jobs green but the quality
job red: a clean website install cannot resolve `@astrojs/mdx`, while the local
website build passed against existing `node_modules`. Fix that before building
more gates.

- [x] Declare or otherwise correctly provide every package imported by the
  generated Blume/Astro configuration, beginning with `@astrojs/mdx`.
- [x] Add a clean-room website receipt that starts without `node_modules`, runs
  `pnpm install --frozen-lockfile`, then performs check, validate, build,
  site audit, and agent-doc checks.
- [x] Provide one local shipping command that invokes the same clean-room gate
  as CI rather than trusting an existing dependency tree.
- [x] Keep generated `.blume` and site artifacts from making a dirty local tree
  look like a reproducible build.

Gate: current `main` CI is green and the website result is reproducible from a
fresh checkout.

## Wave 1 — Define the release contract and coverage inventory

- [x] Add a short testing architecture document that distinguishes unit,
  catalog convergence, differential, CLI integration, release acceptance,
  documentation receipt, and exploratory blind testing.
- [x] Map every public compatibility claim to at least one owner test. The map
  must state whether the receipt proves generated automation, a reviewed SQL
  handoff, split-plan behavior, or refusal.
- [x] Record the known blind discoveries as required acceptance properties:
  output parity, optional development configuration, written edit paths,
  copyable remediation, legacy/new materialized-view queries, restack decision
  retention, observer normalization, contract gating, and final convergence.
- [x] Define the mandatory checkpoints for every nontrivial scenario:
  `planned`, `expanded_legacy`, `expanded_new`, `contract_blocked`,
  `contract_ready`, and `converged`.
- [x] Require an explicit reason when a scenario omits a checkpoint—for
  example, an additive nullable column legitimately has no contract work.
- [x] Add a machine-readable coverage index for family, variant ID, protected
  invariant, native owner, PGlite eligibility, checkpoints, and public claim.
- [x] Reject duplicate variants which add no new DDL, data, client, decision,
  plan-evolution, or environment dimension.

Gate: a reviewer can start from a README or compatibility claim and find the
exact acceptance receipt without searching a 10,000-line convergence file.

## Wave 2 — Establish the PGlite preflight lane

- [x] Pin the PGlite and socket-server versions in isolated test tooling rather
  than resolving a floating `pnpm dlx` package on every run.
- [x] Start the socket server on loopback with an in-memory database, no SSL,
  bounded startup/shutdown, captured logs, and explicit cleanup.
- [x] Configure pgx for the proven PGlite wire mode: no automatic statement
  cache and an execution mode that does not leave shared prepared names in the
  multiplexer. Keep this configuration out of native PostgreSQL receipts.
- [x] Run a startup capability probe which records `version()`, backend PID
  behavior, available extensions, and the supported feature flags. Fail closed
  if a PGlite upgrade changes the assumptions.
- [x] Make scenario eligibility explicit. A PGlite test must list the
  capabilities it uses; unsupported requirements fail scenario registration
  rather than becoming a runtime skip.
- [x] Give every PGlite case a stable variant ID and canonical native owner.
  Fail registration for an orphaned PGlite-only claim.
- [ ] Apply already-generated phase SQL and run client contracts directly. Do
  not route public CLI scratch materialization or verification through PGlite
  until its database/role semantics can satisfy `scratchdb` unchanged.
- [x] Run PGlite scenarios serially or with one isolated PGlite process per
  scenario. Do not call multiplexing genuine PostgreSQL concurrency.
- [x] Add core subset receipts for nullable additions, required-column phase
  SQL, CHECK overlap, bidirectional rename triggers, append-compatible views,
  and the sequential old/new outputs of a reviewed materialized overlap.
- [x] Populate approximately 80–120 focused variants from the DDL, data,
  client, and product-decision axes. Prefer table-driven cases over copied
  end-to-end fixtures.
- [x] Pair every PGlite receipt with the canonical native PostgreSQL scenario
  and compare phase/client outcomes that both engines claim to support.
- [ ] Benchmark cold start, warm scenario time, and complete local feedback
  against the existing native workflow. Keep PGlite in the required fast lane
  only if it provides meaningful earlier feedback; convenience without a local
  PostgreSQL service may remain an opt-in benefit even if warm native
  PostgreSQL is faster.

Gate: PGlite catches deliberately broken core overlap SQL quickly, its
capability report makes every omission explicit, and the corresponding native
PostgreSQL receipt remains required and green.

## Wave 3 — Build the reusable acceptance harness

### Compiled CLI

- [x] Build the real `onwardpg` executable once per test process.
- [x] Run commands as child processes from isolated working directories.
- [x] Capture argv, environment overrides, exit code, stdout, stderr, duration,
  and produced files for every invocation.
- [x] Decode compact JSON into a small test-facing envelope while retaining the
  raw output as failure evidence.
- [x] Provide assertions for status, durable/development status, PlanID,
  generation, findings, next actions, written paths, and exact command
  executability.

### PostgreSQL lifecycle

- [x] Create separate disposable databases for scratch/history materialization,
  development reconciliation, and the simulated release workload when the
  scenario requires them.
- [x] Open the legacy application connection and prepare its statements before
  expand. Apply phase SQL from a distinct deployment connection.
- [x] Open a separate new-application connection after expand.
- [x] Support a dedicated read-only observer role with configurable grants,
  ownership differences, and `default_transaction_read_only`.
- [x] Put bounded context deadlines on database creation, planning, phase
  execution, workloads, and cleanup.
- [x] Reuse onwardpg's existing phase batch parser/executor so transactional and
  nontransactional markers have exactly the same meaning as verification.

### Edited plans and evidence

- [x] Locate named edit pockets through the bundle model rather than brittle
  string offsets.
- [x] Replace only the owned pocket and preserve markers and generated SQL.
- [x] Run plain `onwardpg plan` after every edit; never manufacture a verified
  in-memory result.
- [x] Exercise `verify`, `verify --check`, and contract readiness through their
  public commands.
- [x] Support expiring writer evidence bound to the actual PlanID and history
  head so the positive readiness path is as real as the refusal path.
- [x] Materialize the declared destination independently after contract, inspect
  the live workload database, and run the production semantic diff between
  them.
- [x] Require an empty residual with no decisions, edits, unsupported objects,
  or ignored differences. Run the same final comparison twice to prove
  idempotent convergence.
- [x] Inventory transition-owned compatibility artifacts independently and
  fail if any survive even when the modeled residual is empty.

Gate: one small rename scenario completes the entire compiled-binary lifecycle
without importing planner internals from its acceptance test.

## Wave 4 — Make client contracts impossible to hand-wave

- [x] Introduce typed `LegacyClient`, `NewClient`, and `FinalClient` contracts.
  Each names its prepared statements or actions and its expected result or
  SQLSTATE.
- [x] Require both legacy and new expand contracts for every compatibility
  transition. The runner rejects an incomplete scenario definition before
  touching PostgreSQL.
- [x] Prepare legacy statements before expand and execute those same prepared
  statements afterward.
- [x] Cover reads and writes independently. A successful `count(*)` does not
  prove a renamed or newly typed output can be selected.
- [ ] Require scenarios with views or materialized views to query every old and
  new public output they claim to preserve.
- [x] Require bidirectional write scenarios to exercise old-only, new-only,
  matching dual writes, contradictory dual writes, and subsequent reads.
- [ ] Make data-domain edges explicit: NULL, blank, malformed, boundary,
  overflow, and non-injective conversions where relevant.
- [x] Assert temporary catalog facts after expand and absence of bridge
  columns, triggers, functions, overlap views, and temporary indexes after
  contract.

Add harness self-tests with intentionally broken phase SQL or incomplete client
contracts. At minimum, prove the oracle fails when:

- [ ] the new materialized-view output is absent;
- [ ] legacy prepared SQL breaks after expand;
- [ ] a bidirectional trigger fails to reverse-synchronize;
- [ ] contract runs while a legacy backend remains alive;
- [ ] the final fingerprint matches but the expected new query is unusable; or
- [ ] final application SQL succeeds but an extra bridge trigger, column,
  function, view, index, constraint, grant, or comment leaves nonzero drift;
- [ ] a test-only ignore or normalization would hide a genuine final
  difference; or
- [ ] a required finding is missing from one output renderer.

Gate: weakening a compatibility bridge in each of those ways makes the test kit
fail for the intended reason.

## Wave 5 — Port the canonical scenario ladder

Build a deliberately small ladder matching the way developers encounter the
product.

### Easy

- [x] Add a nullable column. Prove expand-only output, old writers omitting the
  column, new writers using it, empty contract, and final convergence.
- [ ] Add an independent table and index. Prove ordering and no unnecessary
  compatibility machinery.

### Medium

- [x] Add a future-required column with developer-authored cleanup, blocking
  NULL assertion, writer drain, and final `NOT NULL`.
- [ ] Widen a CHECK for old and new values, then clean up and narrow it after
  drain. Include the expand-only widening case where no contract is needed.
- [x] Perform a same-type column rename with old/new reads, old/new writes,
  conflict rejection, equality gate, and native final rename.

### Hard

- [ ] Rename beneath an intentionally changed ordinary view. Prove prepared old
  view SQL and the new appended output against the same expanded database.
- [ ] Change a type through reviewed conversion SQL, including invalid and
  boundary data, exact blocking assertions, final statistics refresh, and no
  automatically invented product meaning.
- [ ] Exercise a development database that is absent, behind, ahead, and
  carrying an unrelated branch-local object.

### Nightmare

- [ ] Port the cross-name/type transition beneath an ordinary view,
  materialized view, and index. Prove old and new materialized outputs—not just
  aggregate row counts—and document non-injective aggregation semantics as a
  reviewed product choice.
- [ ] Combine rename, future-required column, CHECK transition, dependent view,
  legacy writer drain, and cleanup in one release.
- [ ] Implement the full rebase matrix from “Rebase scenario variants.” Cover
  unrelated and same-table additions, dependency-closure changes, endpoint
  changes, partial/full absorption, safe edit retention, true edit conflict,
  multi-bundle restacks, abandoned branch work, and branch rollback.

Gate: each rung is readable as a product story while sharing the same harness
and mandatory checkpoint assertions.

## Wave 6 — Test the plan loop, not merely the plan

- [x] Start every scenario with `config check` and `init` through the compiled
  binary.
- [x] Drive initial questions, copyable hints, named edits, plain reruns, status,
  and verification exactly as an agent or developer would.
- [ ] Execute every emitted next-action command that is supposed to be
  executable. Assert every emitted edit path exists before it is advertised.
- [x] Assert stable PlanID and monotonic generation across plain reruns,
  product-SQL edits, accepted-history restacks, and development reconciliation.
- [ ] Assert decisions survive an unrelated upstream change and invalidate when
  endpoint identity, types, dependency closure, or product question meaning
  actually changes.
- [ ] Prove edited-plan conflicts expose current SQL, regenerated SQL, path,
  merge instructions, and a working verification command.
- [ ] Cover scratch-only configuration, optional development configuration,
  unavailable development databases, and read-only observers.
- [x] Keep branch-local development objects out of durable history while
  emitting safe fast-forward SQL for accepted upstream work.
- [x] Table-drive rebase assertions rather than hiding all restack behavior in
  one composite fixture. Each variant must name exactly which decisions and
  authored pockets survive, invalidate, conflict, or disappear through
  absorption.
- [ ] For PGlite-compatible resulting phases, fan out additional data/client
  variants after the native rebase receipt. Do not label those executions as
  additional rebase tests.

Gate: a source-blind agent can complete each scenario using only README/docs,
the binary, emitted actions, and its isolated PostgreSQL databases.

## Wave 7 — Enforce semantic output parity

The formats need not contain identical prose, but they must not disagree about
safety or the next action.

- [x] Define a normalized semantic projection for JSON and text: top-level
  readiness, durable/development status, blocking findings, decision choices,
  edit paths, conflict remediation, and executable commands.
- [ ] Run representative plan-loop states through both renderers and compare
  that projection.
- [x] Assert `--output sql` is executable-only and never mixes JSON or human
  instructions into the SQL stream.
- [ ] Cover unsafe drop guidance, cross-type identity guidance, missing edits,
  unsupported development reconciliation, conflict recovery, and merge-ready
  output.
- [x] Add negative tests ensuring protocol-version fields and other unused
  compatibility scaffolding do not reappear.

Gate: deleting a blocking finding from either JSON or text fails one table-
driven parity suite.

## Wave 8 — Put the suite in CI without silent success

- [x] Add a required `acceptance` CI job on PostgreSQL 15, 16, 17, and 18.
- [x] Add a separate PGlite preflight job for its declared subset. Its name and
  output must never imply PostgreSQL matrix coverage.
- [x] Use an explicit acceptance opt-in environment variable. When opted in,
  a missing or unusable database is a failure, never `t.Skip`.
- [ ] Run the canonical acceptance ladder on every pull request. Keep the full
  catalog convergence and differential matrix unless measured runtime forces a
  deliberate split.
- [ ] Separate job names for unit/race/fuzz, catalog convergence, differential,
  release acceptance, compiled CLI/docs, and clean-room website build so a red
  check says which promise broke.
- [x] Upload the command transcript, compact outputs, generated bundle, phase
  SQL, workload failures, and final catalog diff when an acceptance scenario
  fails. Never upload credentials or unrestricted database URLs.
- [ ] Add per-scenario and per-command timeouts and print the slowest scenarios.
- [x] Fail if no acceptance scenario ran or if a scenario silently omitted a
  mandatory checkpoint.
- [x] Provide one local command that runs the same acceptance suite against a
  caller-provided disposable PostgreSQL URL.

Gate: an accidentally unset database variable, skipped scenario, dirty
`node_modules`, missing output finding, or broken overlap query cannot produce a
green required check.

## Wave 9 — Consolidate receipts and rerun blind

- [ ] Port blind gauntlets A, B, and C onto the compiled-binary acceptance
  harness before removing their bespoke orchestration.
- [ ] Retain focused planner and catalog tests below the acceptance layer; only
  delete duplicated setup and application logic after receipt parity is proven.
- [ ] Make documentation SQL receipts consume the same canonical scenario
  artifacts instead of maintaining a parallel illustrative path.
- [x] Generate a compact coverage index listing each scenario, checkpoints,
  PostgreSQL versions, public claims, and latest owning tests.
- [ ] Give three new source-blind agents clean binaries and isolated databases:
  one easy/medium plan-loop user, one dependency-heavy PostgreSQL user, and one
  rebase/team-workflow user.
- [ ] Ask an independent PostgreSQL specialist to review only phase files,
  client SQL, contract evidence, and final catalog receipts.
- [x] Classify every blind finding as a missing acceptance invariant, a real new
  product feature, an operational limitation, or documentation friction. Add a
  permanent test only when it represents a stable product contract.

Gate: the new blind cohort completes the supported paths without source access,
and any finding inside an existing promise produces a focused acceptance
receipt before the milestone ships.

## Shipping matrix

- [ ] Current clean-room CI is green.
- [x] Harness self-tests prove weak oracles are rejected.
- [x] The PGlite capability probe and scoped preflight pass, and every PGlite
  scenario names a paired native acceptance owner.
- [x] The PGlite matrix contains broad DDL/data/client variants without any
  case claiming restack, drain, authorization, locking, or version coverage.
- [ ] Easy, medium, hard, and nightmare scenarios pass through the compiled CLI.
- [ ] The native rebase matrix covers every upstream-change class in the scope
  table and reports exact retain/invalidate/conflict/absorb behavior.
- [ ] Every nontrivial scenario runs prepared legacy and new application SQL
  after expand.
- [ ] Every supported native scenario and rebase variant finishes with the
  production diff engine reporting a stable empty residual against independently
  materialized desired DDL.
- [ ] No acceptance case uses fingerprint equality or hand-picked catalog
  queries as a substitute for the empty semantic residual.
- [x] Contract refusal and positive readiness paths both run.
- [ ] Restack, decision retention, invalidation, and edited-conflict recovery
  run through real persisted bundles.
- [x] JSON/text semantic parity and SQL-stream purity pass.
- [x] Acceptance passes on PostgreSQL 15–18 with zero silent skips.
- [x] Existing unit, race, fuzz, convergence, parity, and differential suites
  remain green.
- [x] README, documentation receipts, CLI help, Blume clean-room checks, and
  agent artifacts remain green.
- [x] A final blind cohort and independent PostgreSQL review report no release
  blocker.
- [x] All disposable databases, roles, containers, and temporary workspaces are
  removed.
- [x] `git diff --check` passes and the coverage inventory matches the shipped
  scenarios.

## Explicitly deferred

This milestone does not require:

- exhaustive Django, Drizzle, Prisma, or SQLAlchemy application projects;
- production deployment, queue, preview-environment, replica-lag, or capacity
  simulation;
- an embedded Docker/testcontainers dependency inside the Go suite;
- PGlite as a replacement for native PostgreSQL acceptance or the 15–18
  matrix;
- a declarative scenario DSL;
- generic mutation-testing infrastructure;
- exhaustive cross-products of every PostgreSQL object feature;
- unattended production execution; or
- new CLI lifecycle commands or protocol versions.

Framework and deployment simulations remain useful future goals. They should
build on this acceptance kit rather than compensate for its absence.

## Final handoff

The completed milestone report must show:

1. the claim-to-receipt coverage index;
2. one command transcript from each difficulty rung;
3. exact legacy and new SQL executed after expand;
4. premature contract refusal and successful post-drain readiness;
5. a rebase/restack transcript retaining and invalidating the correct choices;
6. PostgreSQL 15–18 acceptance results with zero skipped scenarios;
7. proof that harness self-tests catch incomplete client contracts and broken
   overlap SQL;
8. the PGlite capability/benchmark report and its paired native receipts;
9. clean-room documentation and website results; and
10. blind-agent and PostgreSQL-specialist verdicts.
