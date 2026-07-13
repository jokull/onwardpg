# Working-tree review — graph-architecture reset

> Historical review. This document captures the pre-reset working tree and is
> intentionally retained for provenance, but its line references and several
> findings have been superseded by the typed graph implementation. Use the
> resolved-findings table and dependency-aware frontier in `PLAN.md` as the
> current resume ledger; do not treat an unannotated finding below as a claim
> about the present code.

Date: 2026-07-12. Scope: entire uncommitted working tree (~8k LOC) on top of
`004f6d5`, reviewed against README.md and docs/architecture.md (PLAN.md was
absent). `go build ./...` and `go vet ./...` are clean. Process: 8 finder
angles → 42 candidates → dedup to 21 → one adversarial verifier each.
19 survived (15 correctness, 4 cleanup), 2 refuted as intentional design.

Updated 2026-07-12 for the PostgreSQL 14–18 support range
(`validatePostgresVersion`): finding 15 is resolved, finding 13 narrowed.

## Correctness findings (ranked most severe first)

### 1. DefaultEquivalent executes SQL on the live `--from` database — CONFIRMED
`cmd/onwardpg/main.go:65`

`Options.DefaultEquivalent` builds `SELECT (left) IS NOT DISTINCT FROM (right)`
from catalog-derived default expressions and runs it on `comparatorURL`, which
prefers the `--from` URL (production) over `--dev-url`, on a plain autocommit
connection — outside the read-only inspection transaction the architecture
promises.

- Side effects: `nextval('a_seq')` vs `nextval('b_seq')` at equal counters is
  evaluated on production, consuming sequence values on every plan run.
- Wrong plan: the two calls return equal values, so
  `filterEquivalentDefaults` (`internal/graphplan/plan.go:290`) silently drops
  the required `SET DEFAULT` migration.
- `--from file://`: the comparator connects to the bare `--dev-url`
  maintenance DB after the temp database was already dropped, so any default
  referencing a user type/sequence errors and the whole plan fails.

Fix direction: compare defaults on the dev database only, inside a read-only
transaction, and never evaluate volatile expressions by value.

### 2. Substring-based dependent detection suppresses column renames → data loss — PLAUSIBLE (constructible)
`internal/graphplan/plan.go:555` (`hasColumnDependents`)

Dependents are detected via raw `strings.Contains(definition, column.Name)`
with no quoting or word-bounding. Renaming column `id` on a table with any
constraint/index mentioning `paid_at` or `valid_until` is a false positive:
the rename candidate is skipped at plan.go:485, `destructiveQuestions` offers
only `drop`, and an approved answer emits `DROP COLUMN` where
`RENAME COLUMN` would have preserved data. Same flaw in
`containsEnumReference` (line 440) and the blind `ReplaceAll` in
`normalizeEnumReference`/`normalizeTableReference`. The snapshot already has
typed index→column edges (`internal/source/graph.go:745`) that should be used
instead; constraint→column needs real parsing, not substrings.

### 3. FK-referenced tables can never be renamed → data loss — CONFIRMED
`internal/graphplan/plan.go:653` (`equivalentTableForRename`)

`hasExternalReference` (lines 694–702) disqualifies any table referenced by a
foreign key from another table, so `public.users` (referenced by
`orders.user_id`) renamed to `public.accounts` never yields a `rename_table`
question — even with a pre-supplied answer. The only planned outcome is
drop+create, destroying the table. PostgreSQL follows FK references across
`ALTER TABLE ... RENAME` automatically; self-referencing FKs are already
rewritten (lines 719–722), so the external-reference guard is over-broad, not
required for soundness.

### 4. Extension drops render zero SQL, plan still `planned` — CONFIRMED (reproduced)
`internal/graphplan/plan.go:1154` (`renderDropWithOptions` default case)

`destructiveQuestions` asks `drop:extension:public:pgcrypto`, but the render
switch has no `pgschema.Extension` case: default returns nil statements and
nil unsupported markers. Reproduced: after answering `drop`, the next run
returns `status=planned`, 0 statements — no `DROP EXTENSION` is ever emitted;
with a persisted answer the plan is a silent no-op forever. Asymmetric with
extension creates, which at least return an `extension_change` unsupported
marker (plan.go:975).

### 5. Partition children planned as detached plain tables — PLAUSIBLE (constructible)
`internal/source/graph.go:240`

Table inspection selects `relkind IN ('r','p')` with no `relispartition`
handling; `pg_inherits` is never read; the blocker sweep only covers
v/m/p relkinds. A `CREATE TABLE events_2026 PARTITION OF events ...` in the
desired DDL snapshots as a plain table and renders as a bare
`CREATE TABLE events_2026 (...)` — `status=planned`, applied parent has zero
partitions, inserts into the parent fail at runtime. Convergence tests only
cover parents with no partitions.

### 6. Position-only column Modify renders nothing; diff never converges — PLAUSIBLE (verified end-to-end)
`internal/graphplan/plan.go:1330` + `internal/source/graph.go:280,332`

`Column.Position` is raw `pg_attribute.attnum` (sparse after historical
`DROP COLUMN`) and participates in `reflect.DeepEqual` diffing and
fingerprints, but `renderColumnModify` has no Position branch and no
unhandled-difference fallthrough. Any long-lived DB (attnums a=1, c=3) vs a
fresh `file://` materialization (a=1, c=2) produces a permanent Modify that
renders zero SQL while every run reports `status=planned` — silent,
unresolvable divergence. Fix direction: rank columns
(`row_number() over (order by attnum)`) or exclude Position from equality.

### 7. `--ignore` selectors are validated per side, so one-sided ignores abort planning — PLAUSIBLE (confirmed in code)
`internal/source/graph.go:1025` (`ignoreTracker.Validate`) + `cmd/onwardpg/main.go:44,48`

The same ignore list goes to both `LoadGraph` calls and each side's tracker
errors on selectors that matched nothing there. The primary use case —
ignoring a prod-only object absent from the desired DDL — always fails with
"unused ignore selectors". The README's own example
(`--ignore extension:pgcrypto` with `--to file://schema.sql`) fails unless the
DDL file also creates pgcrypto. Fix direction: validate usage across the union
of both sides (or per-side opt-out).

### 8. Combined type+default change applies new default against old type — PLAUSIBLE (code-confirmed)
`internal/graphplan/plan.go:1375,1381` + `rebuildBatches` (1672–1695)

The type change is phase `migrate`, the new `SET DEFAULT` phase `expand`, and
batches are hard-ordered expand→migrate with no per-column linkage. For
`n integer DEFAULT 0` → `n text DEFAULT 'abc'`, `SET DEFAULT 'abc'` executes
while `n` is still integer; PostgreSQL coerces defaults at DDL time →
`invalid input syntax for type integer` at apply.

### 9. Same-named object moved between tables: create-before-drop collision — CONFIRMED (reproduced)
`internal/change/change.go:112` + phase bucketing in `internal/graphplan/plan.go`

Index `idx` on `public.a` → `idx` on `public.b` is not a rename candidate
(`resolveIndexRenames` requires `from.Table == to.Table`, plan.go:790), so it
diffs to create (expand) + drop (contract). Reproduced statement order:
`CREATE INDEX "idx" ON public.b` before `DROP INDEX public."idx"`. Indexes
share the per-schema relation namespace, so the apply fails with
`relation "idx" already exists`. No name-collision check exists anywhere in
the planner. Same failure shape for standalone sequences.

### 10. Serial change early-return discards other column modifications — PLAUSIBLE
`internal/graphplan/plan.go:1337–1341`

When `Serial` differs, `renderColumnModify` returns
`renderSerialModify(...)` unconditionally, skipping the default (1377–1383),
NOT NULL (1384–1407), and comment (1408) handling. `id serial NOT NULL` →
`id integer` nullable emits no `DROP NOT NULL`; the plan reports `planned`
but is incomplete. Self-heals on a re-plan (Serial is then nil on both
sides), but a single generated plan is wrong while claiming success.

### 11. Standalone→constraint-backed index attaches old index without structural comparison — PLAUSIBLE
`internal/graphplan/plan.go:1176–1180` + `renderConstraintCreateUsingExistingIndex` (1477–1506)

The `before.Constraint == "" && next.Constraint != ""` case returns nil before
the `Parts` comparison is reached, and the USING INDEX path checks only
existence/type, never columns. Unique index on `(a)` becoming a
constraint-backed index on `(a,b)`: `ADD CONSTRAINT ... USING INDEX` attaches
the (a)-only index — the applied migration enforces the wrong uniqueness
while reporting success. Converges via drop+recreate on the next plan.

### 12. `--unsorted-dump` never receives an order — broken flag — PLAUSIBLE (reproduced)
`cmd/onwardpg/main.go:52`

Nothing populates `Options.UnsortedOrder` (only the struct definition and a
unit test reference it; the adapter has no mechanism to convey order), so
`orderUnsortedChanges(changes, nil)` falls back to alphabetical ID order.
Reproduced: `constraint:public:t:pk` sorts before `table:public:t`, emitting
`ALTER TABLE ... ADD CONSTRAINT` before `CREATE TABLE` — neither dump order
(as README/help claim) nor dependency-valid.

### 13. `transactional` flag discarded; batching re-derives it by SQL substring sniff — PLAUSIBLE
`internal/graphplan/plan.go:1663` (`_ = transactional`), 1682, 1703

Every renderer computes `transactional` at ~50 call sites; `statement()`
throws it away and both `rebuildBatches` and `rebuildUnsortedBatches` sniff
`strings.Contains(sql, " INDEX CONCURRENTLY ")`. Constructible today:
`COMMENT ON TABLE t IS '... CREATE INDEX CONCURRENTLY ...'` is wrongly marked
non-transactional in the protocol output. Fragile tomorrow: any future
non-transactional statement without the substring
(`DETACH PARTITION CONCURRENTLY`, `REINDEX CONCURRENTLY`) gets wrapped in a
transaction and fails at apply. (`ALTER TYPE ... ADD VALUE` at plan.go:1269 is
fine on the supported PG 14–18 range.) Fix: carry the flag through
`statement()`.

### 14. Unknown phase strings silently delete statements — PLAUSIBLE (latent)
`internal/graphplan/plan.go:1672–1696`

`rebuildBatches` nils out `result.Statements`/`Batches` and rebuilds them by
iterating only the hardcoded `{"expand","migrate","contract","manual"}`. A
statement whose phase is any other string (renderer typo) vanishes from both
lists while `Status` is still set to `Planned` unconditionally (plan.go:245).
All ~51 current call sites use valid literals; there is no validation. Fix:
typed phase constants + error on unknown phase.

### 15. ~~`DROP DATABASE ... WITH (FORCE)` vs version gate~~ — RESOLVED 2026-07-12
`internal/source/graph.go:33` + `validatePostgresVersion` (111–119)

The version gate now enforces PostgreSQL 14–18, where `WITH (FORCE)` is valid
syntax, so the original leak-on-PG-10-12 scenario is gone. Residual nit: the
deferred drop still discards its error (`_, _ = admin.Exec(...)`), so any
cleanup failure (permissions, lingering connections despite FORCE) silently
leaks an `onwardpg_ddl_*` database — worth logging.

## Refuted candidates (intentional design — do not "fix")

- **Union (not symmetric-difference) gating of unsupported objects**
  (`plan.go:70`): docs/atlas-postgres-parity.md states verbatim that the gate
  intentionally blocks an unchanged unknown object, because name-only blocker
  selectors cannot prove definitions match. The old planner's
  symmetricDifference assumed they did.
- **Staged question rounds without partial statements** (`plan.go:91–151`):
  later questions depend on earlier answers (a confirmed table rename consumes
  child changes; enum renames consume column modifies), and the old planner's
  "partial statements" included `CREATE TABLE` for a pending rename target —
  wrong SQL if the answer was "rename". Multi-round is the sound contract.

## Cleanup (verified, lower priority)

- **Rename-resolver quadruplication** (`plan.go:349–828` + four identical
  Build blocks at 95–134): ~300 genuinely duplicated lines sharing a verbatim
  skeleton; the copies have already drifted (enum consumes dependent column
  modifies, table consumes children, column checks dependents). A generic
  `resolveRenames[T pgschema.Object]` with an equivalence closure and
  on-accept hook is idiomatic and would net ~200 lines. Would also be the
  natural place to fix findings 2 and 3 once.
- **`quote` defined three times** (identical bodies:
  `internal/source/source.go:57`, `internal/graphplan/plan.go:1728`,
  `internal/differential/postgres_integration_test.go:346`). Shared home:
  pgschema (already imported by all three).
- **`modeled` map drift hazard** (`internal/source/graph.go:770–774`): five
  dead keys (`schema_comment`, `relation_comment`, `column_comment`,
  `identity_column`, `column_collation`) that no blocker selector emits; the
  hand-keyed map is decoupled from both `inspectGraphBlockerSelectors` and
  `graphOutsideCoreQuery`, so inspector/map drift can produce objects that
  are neither typed nodes nor blockers — silent omission. A single registry
  should drive both.
- **Unpopulated fields in fingerprints** (`pgschema/graph.go:236,276`):
  `Index.Concurrent` and `Partition.Parts` are never assigned by inspection
  yet participate in `CanonicalJSON`/`DeepEqual` — unlike `Index.Definition`,
  which is deliberately excluded (`json:"-"` + zeroed in `objectsEqual`). The
  first adapter-supplied snapshot that sets them produces phantom Modifies.
  Also dead until a consumer exists: `Domain`, `Composite`, `View`, `Routine`
  structs and the KindView/MatView/Routine/Trigger/Policy/ForeignTable
  constants (docs declare them staged scope — keep or cut deliberately).
  Related dead code: `unsupportedAggregatesQuery`
  (`internal/source/graph.go:905`) and `Answers.Lookup`
  (`internal/protocol/protocol.go:72`, bypasses every Resolver guarantee).
- **Quadratic hot spots** (matter at hundreds of tables / thousands of
  columns): rename helpers do full-snapshot scans inside drop×create loops
  (`plan.go:491+`); `sortIDs` re-allocates `ID.String()` per comparison on
  every `Objects()`/`IDs()`/`Dependencies()` call (`pgschema/graph.go:608`);
  `Batches()` re-sorts the ready queue on every pop (`pgschema/graph.go:529`);
  `addForeignKeyKeyDependencies` is O(FK×constraints)
  (`internal/source/graph.go:553`); `implicitConstraintIndexDrop` is
  O(changes²) and called twice (`plan.go:1557`); three of the five
  `inspectGraphBlockerSelectors` queries are discarded or duplicate
  `graphOutsideCoreQuery` rows (`internal/source/graph.go:804`).
- **Test-infra duplication**: `executeOnwardPlan`
  (`internal/differential/postgres_integration_test.go:314`) re-implements
  `applyPlan` (`convergence_integration_test.go:1958`) — the reference
  executor for protocol semantics should be one helper; the advisory-lock
  constant `731095702114` is declared three times across packages.
