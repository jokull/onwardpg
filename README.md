# onwardpg

**A PostgreSQL schema-diff planner that generates the compatibility window,
not just the final ALTER statements.**

![A traveler surveying the safe path onward through a changing landscape](docs/assets/onwardpg.png)

A rename can break a draining process; a required column can break an old
writer. Backfills and casts depend on product meaning PostgreSQL cannot infer.
onwardpg plans one forward-only bundle around one application deployment:

~~~text
compare -> expand -> deploy -> drain -> contract -> prove convergence
~~~

Expand runs while old code is live. Contract runs only after old instances,
workers, queues, connection pools, and stale write paths have drained. The new
application version must work on both sides of contract. If that needs two
deployments, onwardpg says so instead of hiding one inside a phase name.

onwardpg never applies SQL to production. It writes reviewable bundles and
replays them in restricted disposable PostgreSQL databases.

**Documentation:** [onwardpg.solberg.is](https://onwardpg.solberg.is)

The [plan-command walkthrough](https://onwardpg.solberg.is/concepts/plan-command/)
climbs from a nullable column through required-column staging and an online
rename to a type change beneath views, a materialized view, and its index. Every
displayed phase fragment is tied to a checked-in CLI or PostgreSQL receipt.

**Using a coding agent?** Point it at the
[onwardpg skill](https://onwardpg.solberg.is/skill.md) before it changes the
schema. The skill is the concise operating contract; the
[agent guide](https://onwardpg.solberg.is/agents/agent-assisted-planning/)
explains the reasoning, team workflow, and restricted production-evidence
pattern for humans.

~~~text
Read and follow https://onwardpg.solberg.is/skill.md for this migration.
Maintain one evolving onwardpg plan, supply only evidence-backed decisions,
never apply to production, verify the exact bundle, and report the operational
gates verification cannot prove.
~~~

## Five-minute start

Install the preview on macOS or Linux:

~~~sh
brew install jokull/tap/onwardpg
onwardpg version
~~~

Go developers can instead run:

~~~sh
go install github.com/jokull/onwardpg/cmd/onwardpg@v0.1.0-preview.1
~~~

Export your framework's authoritative CREATE-statement DDL to a file or
command. Django, Drizzle, Prisma, SQLAlchemy, and other tools remain responsible
for turning application models into PostgreSQL DDL.

~~~toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_file = "schema.sql"
# Or: schema_command = ["pnpm", "--silent", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
~~~

The development URL is catalog-inspected read-only. The scratch URL is a
control-plane administrator for a dedicated local or CI PostgreSQL cluster. It
must be able to create and force-drop databases and short-lived login roles; it
must never point at production or a shared application cluster.

Each materialization uses a random, one-hour login that owns only its random
database and lacks SUPERUSER, CREATEDB, CREATEROLE, REPLICATION, and BYPASSRLS.
DDL runs through that login; the administrator only creates and cleans up.
Databases clone pristine `template0` while explicitly copying the connected
control database's encoding, locale provider, locale, collation, and ctype.
Creation fails if PostgreSQL reports a different collation version.

~~~sh
export ONWARDPG_DEV_DATABASE_URL='postgres://readonly:secret@localhost/myapp_dev'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:secret@localhost/postgres'

onwardpg config check
onwardpg init
onwardpg plan add-booking-status
~~~

Answer plan questions with the printed fingerprinted hints and rerun. Editable
product-specific SQL must pass clone verification before acceptance.

For this exact required-column example, the initial generated receipt is:

~~~text
migrations/onward/app/add-booking-status/
├── manifest.json
├── plan.json
└── phases/
    ├── expand.sql
    └── contract.sql
~~~

`decisions.json` appears only when a semantic hint is consumed. Add
`verify.sql` when the plan needs Boolean assertions. Review every statement
and hazard, edit the reported SQL pocket, then run:

~~~sh
onwardpg verify
~~~

`verify` selects the worktree's active plan. In a clean or multi-plan checkout,
select it explicitly with `onwardpg verify --bundle add-booking-status`.

## Example: rename without guessing

Two snapshots cannot prove that a missing column and a new column represent the
same data. Confirm the identity explicitly:

~~~sh
onwardpg plan rename-display-name \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}'

# For a known-small table only; manual_sql and split_plan are the alternatives.
onwardpg plan \
  --hint '{"kind":"rename_backfill","name":["app","accounts","display_name"],"strategy":"single_transaction"}'
~~~

For an eligible same-type rename, onwardpg creates a temporary second column
and deterministic dual-write trigger. It then asks how existing rows will be
backfilled:

- manual_sql is the default: provide reviewed SQL plus a boolean equality
  query, using whatever batching strategy production volume requires;
- single_transaction explicitly accepts an unbounded UPDATE and surfaces table
  scan, long transaction, WAL, and replica-lag hazards; or
- split_plan keeps both contracts until a later application deployment.

Contract first asserts that both values agree. Only then does it remove the
bridge and perform the native rename. Defaults, generated values, partitions,
existing trigger ordering, RLS, and other shapes that make this bridge
ambiguous remain explicit refusals or editable handoffs.

## Example: change a type without guessing

Changing `age text` to `age integer` does not tell onwardpg what an empty,
malformed, or product-specific value means. It identifies the dependent views,
materialized views, and indexes, then asks for reviewed expand/contract SQL.
For `manual_sql`, the actual generated edit pockets say:

~~~sql
-- expand.sql
-- ONWARDPG TODO: replace this comment with reviewed EXPAND SQL for column:app:accounts:age (text -> integer).
-- Establish both old and new interfaces, synchronization/conflict behavior, and any initial backfill while old code is live.
-- Do not use a direct ALTER TYPE here: this plan surrounds one rolling application deployment.

-- contract.sql
-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for column:app:accounts:age (text -> integer).
-- After pre-deployment writers drain, perform final catch-up/assertions, remove compatibility objects, and converge to PostgreSQL type integer.
~~~

The application-owned bridge might use `NULLIF(trim(age), '')::integer` as its
conversion rule, but a bare in-place `ALTER TYPE` is not a rolling-deployment
plan. The edited expand SQL must preserve both interfaces; contract performs
the final catch-up and cutover after old writers drain. onwardpg orders the
surrounding dependency closure, refreshes statistics, and requires the edited
bundle to converge. It never guesses whether invalid input should become
`NULL`, fail, or map to a product-specific value. The checked-in
[`type-change` receipts](docs/receipts/type-change/) are regenerated through
the current CLI in CI.

## What onwardpg proves

- Desired SQL is materialized by real PostgreSQL, then represented as typed
  objects and dependency edges rather than parsed by a partial SQL grammar.
- Creation follows desired dependencies; destruction reverses current
  dependencies. Catalog-normal dependencies are projected after every object
  is known, including domain checks, range functions, routine signature types,
  table row types and arrays, defaults, generated expressions, constraints,
  indexes, policies, and views. References to atomic extension members project
  to their Extension node. If a provider runs in contract, newly created
  dependents move with it so phase grouping cannot invert that edge.
- Unknown inventoried families fail closed. A checked-in PostgreSQL 15–18
  attribute ledger classifies every live pg_catalog table column as modeled,
  blocked, derived, environmental, runtime, or secret. Auxiliary TOAST options
  and tablespaces, ordinary-view defaults, unmodeled subobject comments, and
  non-owner grant chains are explicit blockers. So are customized implicit
  serial/identity sequence metadata, interrupted concurrent partition detaches,
  exceptional PostgreSQL 18 NOT NULL inheritance, retained stored dependents
  of a changed routine, and uncoordinated shared-namespace kind transitions.
- Extensions are an intentional atomic boundary: equal package name, version,
  and schema stands in for equal extension-owned members. onwardpg assumes the
  installed package contents for one version are identical; it does not audit
  member definitions or upgrade-path history. Because an opaque upgrade can
  stale stored users, a version change with any retained catalog-proven
  dependent fails closed for extension-specific handling.
- Rename, destructive, cast, authorization, and backfill decisions are bound
  to exact graph fingerprints. Stale or unused answers fail.
- Verification independently replays the selected checkpoint and full
  continuation, runs boolean assertions, compares final graph fingerprints,
  and requires an empty residual diff.

## What remains yours

Catalog convergence does not prove product meaning, data validity, table size,
lock duration, traffic compatibility, WAL volume, replica lag, drain timing,
or rollback readiness.

Every column added to an existing table carries an application row-shape
hazard. Old writes must list target columns; readers must tolerate an additional
result column or avoid rigid positional SELECT *. onwardpg cannot inspect your
application queries.

PostgreSQL 15–18 are supported independently. Each bundle is receipted to the
scratch server's major and cannot be replayed as evidence for another major.
`dev plan` likewise requires the development and scratch servers to have the
same major.
Scratch must provide referenced roles, languages, and extension packages.
Superuser extensions and external `ALTER ... OWNER TO` targets are incompatible
with the restricted login unless provisioned outside the database-local
boundary. Non-owner grant chains also fail closed because the login cannot
inherit them. Non-table owner and ACL equality is deliberately relative to the
execution role: default `current_user` state is accepted; deviations block.

New triggers and policies, plus policy changes or RLS enable/force changes on
an existing table, are contract work: deploy compatible code first, then drain
old traffic before behavior or authorization changes. Policy changes precede
dependent RLS tightening. Their hazards still require application review.

Concurrent index mode applies only to ordinary standalone indexes. Adding a
primary-key, unique, or exclusion constraint to an existing table still asks
PostgreSQL to build its backing index synchronously; the plan reports a blocking
index build and access-exclusive-lock hazard even when concurrent mode is on.

Composite attribute removal or type change uses PostgreSQL `CASCADE` only after
a fingerprint-bound confirmation and reports data-loss, implicit-cast, and
dependent-rewrite hazards.

Changing a routine used by a retained expression/partial index, stored
generated column, or validated constraint stops for explicit rebuild,
recomputation, or revalidation work; catalog equality cannot prove stored data.

Arbitrary SQL hidden inside procedural strings is not a catalog dependency and
is never claimed as one. Unsupported operations remain visible; an ignore
selector is an accepted blind spot, not equality.

## Commands and deeper reference

~~~text
onwardpg init             establish replayable history
onwardpg plan [name]      create, revise, or restack one active bundle
onwardpg status           inspect active plan and history
onwardpg verify           replay, assert, and prove the active bundle
onwardpg history status   inspect the accepted hash chain
onwardpg dev plan         reconcile a developer database without applying SQL
onwardpg drift check      compare production read-only state with accepted history
onwardpg config check     validate sources, PostgreSQL majors, and ignores
~~~

See [migration workflow](docs/migration-workflow.md),
[supported features](docs/supported-features.md),
[safety model](docs/safety-model.md), [CLI reference](docs/cli.md), and
[bundle format](docs/bundles.md). onwardpg is an MIT-licensed developer preview
with no production apply command, embedded agent, framework adapter, or down
migration generator.
