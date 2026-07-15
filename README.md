# onwardpg

**The PostgreSQL schema-diff planner that generates the compatibility window,
not just the final `ALTER` statements.**

![A traveler surveying the safe path onward through a changing landscape](docs/assets/onwardpg.png)

Most diff tools answer “what SQL makes these schemas equal?” onwardpg also
asks “how can old and new application versions overlap while we get there?” It
turns accepted migration history and current exported CREATE DDL into one
forward-only feature rollout:

```text
accepted history
      │
      ▼
EXPAND       add the new interface without removing the old one
      │      old and new application versions can overlap here
      ▼
MIGRATE      a deployment boundary for product-specific data work
      │
      ▼
CONTRACT     remove the old interface after stale code is gone
      │
      ▼
desired schema
```

An accurate final-state diff can still be an operationally wrong migration. A
direct rename breaks stale application instances; a required column breaks old
writers; a column replacement may need dual writes and a product-aware backfill.
onwardpg stages a supported transition, asks for intent that schema state
cannot contain, and blocks rather than disguise a breaking cutover with a safe
phase label.

Repeated `plan` calls replace the same feature bundle as its DDL evolves or new
migrations land underneath it. Developer- or agent-authored SQL survives the
regeneration. `verify` executes the exact files on disposable PostgreSQL and
proves structural convergence to the exported DDL.

It does **not** prove production traffic compatibility, backfill performance,
deployment timing, or business correctness. It never applies SQL to a
development, staging, or production database.

The approach comes from [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive migrations
from schema state, keep manual control, expose drift, and test difficult work
on a clone.

## Who owns what

A coding agent is useful but optional. “Agent” below means the developer or
coding agent carrying the product and feature context.

| Owner | Responsibility |
| --- | --- |
| **onwardpg** | Catalog diff, specific machine-readable questions, generated structural SQL, feature-bundle regeneration, receipts, and disposable-clone convergence |
| **Developer or coding agent** | Product intent, application compatibility, backfills, assertions, and review of every generated statement |
| **Deployment tooling and operator** | Executing phases, tracking each environment’s progress, traffic gates, observation, and rollback decisions |

The extension point is ordinary SQL, not an orchestration DSL. onwardpg may
leave a clearly marked backfill pocket in `migrate.sql`; the agent edits that
file and adds Boolean assertions to `verify.sql`.

`migrate.sql` is not a way to split `expand.sql` around a `BEGIN` block. Each
file already carries explicit transactional and non-transactional batch
markers. `migrate` exists because data work normally belongs after compatible
code (often dual writes) is deployed and before destructive cleanup. It is
only written when that boundary has work.

## Developer-preview boundary

onwardpg is an MIT-licensed developer preview targeting PostgreSQL 15–18. Its
catalog model covers many PostgreSQL object families, but catalog recognition
does not imply an automatic online transition for every mutation.

Today’s demonstrated compatibility strategies include ordinary additive DDL,
staged required columns, concurrent index planning, and a shape-preserving
table rename that keeps the old physical table available through expand. A
column-type change is recognized but handed to editable phase SQL because its
dual-write and conversion rules are product-specific. Confirmed column, enum,
view, and routine renames become editable compatibility handoffs; extension
schema moves remain explicit blockers.

The detailed, test-linked boundary lives in [supported
features](docs/supported-features.md). Unsupported state is reported; it is not
silently discarded.

## Five-minute path

### Install and configure

The preview requires Go 1.26 or newer:

```sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
onwardpg version
```

Give onwardpg authoritative feature-development DDL as a checked-in file or a
command. For example, `schema_command` can run `drizzle-kit export`:

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_file = "schema.sql"
# Or: schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
# Exact provider-owned objects that are deliberately outside this target.
# ignore = ["extension:pg_stat_statements", "schema:auth"]
```

Django, Prisma, SQLAlchemy, Drizzle, or handwritten schema code can supply DDL
through this interface. onwardpg does not read their migration journals and
has no framework adapters.

Managed PostgreSQL often includes provider-owned objects—Supabase builtins are
a common example—that should neither enter onwardpg history nor appear as
feature work. Put reviewed catalog selectors in the target's `ignore` list.
`onwardpg config check` requires every configured selector to match the
authoritative DDL or development catalog and reports the exact excluded
objects. An ignored schema does not implicitly ignore everything inside it;
use exact selectors, and treat each one as an explicit blind spot.

```sh
export ONWARDPG_DEV_DATABASE_URL='postgres://postgres:secret@localhost:5432/myapp_dev?sslmode=disable'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:secret@localhost:5432/postgres?sslmode=disable'
onwardpg config check
```

The scratch URL must be an administrative URL on a server where onwardpg may
create and drop disposable databases. It must never point at production. The
development URL is inspected read-only.

### Establish the history floor

Suppose `schema.sql` and the local development database contain:

```sql
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint PRIMARY KEY
);
CREATE TABLE app.customer_events (
  id bigint PRIMARY KEY,
  occurred_at timestamp NOT NULL
);
```

Initialize the replayable migration chain once:

```sh
onwardpg init --target primary
```

For an existing application, this records the complete starting point for
future onwardpg bundles. It does not connect to or mutate production.

### Plan one additive feature

Add this table to the complete exported DDL:

```sql
CREATE TABLE app.customer_profiles (
  id bigint PRIMARY KEY,
  biography text
);
```

Start one named feature plan:

```sh
onwardpg plan customer-profile --target primary
```

The generated `phases/expand.sql` contains the compatible addition:

```sql
-- EXPAND: run before application code relies on customer profiles.
CREATE TABLE "app"."customer_profiles" (
  "id" bigint NOT NULL,
  "biography" text
);
ALTER TABLE "app"."customer_profiles"
  ADD CONSTRAINT "customer_profiles_pkey" PRIMARY KEY ("id");
```

The same folder is regenerated as the feature changes:

```text
migrations/onward/primary/customer-profile/
├── manifest.json       # input fingerprints and exact file digests
├── decisions.json      # accepted product intent
├── plan.json           # generated operations, batches, and hazards
├── verify.sql          # agent-owned Boolean assertions
└── phases/
    ├── expand.sql         # present when the plan has expand work
    ├── migrate.sql        # present only for migrate/data-work batches
    └── contract.sql       # present when the plan has delayed cleanup
```

That is the entire basic loop: export DDL, run `plan`, review ordinary SQL,
edit product-specific work, and run `verify`.

## The compatibility-window showcase

Now let the same feature become difficult:

- `app.accounts` should become `app.customers`;
- events need a replacement `occurred_on date` column while the old
  `occurred_at timestamp` remains available through a compatibility window; and
- historical timestamps need a product-specific conversion.

Two schemas cannot prove that the table change is a rename, that the old event
column may be destroyed, or what “date” means to the product. This example is
a column replacement—not a direct type conversion. A direct `ALTER COLUMN ...
TYPE` change is separately handed to editable `manual_sql`, because its
conversion and rollout rules are product-specific. `plan` returns
bounded questions. The agent can supply known intent immediately or rerun with
the offered hints:

```sh
onwardpg plan --target primary \
  --hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}' \
  --hint '{"kind":"drop","object":"column","name":["app","customer_events","occurred_at"]}'
```

An answer records intent; it does not waive the compatibility rules.

### `expand.sql`

For this supported table shape, onwardpg leaves the old physical table in
place and introduces the new name as a temporary compatibility view. It also
adds the required event column as nullable so old writers keep working:

```sql
-- Add the destination without breaking old inserts.
ALTER TABLE "app"."customer_events"
  ADD COLUMN "occurred_on" date;

-- Expose the new name while stale instances retain the physical old table.
CREATE VIEW "app"."customers" WITH (security_invoker = true) AS
SELECT "id" FROM "app"."accounts";
```

Simple PostgreSQL views are automatically updatable, but they are not
universal table aliases. New code using the temporary view cannot assume
table-only behavior such as `ON CONFLICT`, named constraints, triggers, or
table-shaped catalog introspection. onwardpg copies view-capable runtime grants
and blocks the automatic strategy when the table shape or privileges cannot
survive the bridge.

### `migrate.sql`

After deploying code that maintains both event columns, the agent replaces the
generated TODO with the product-aware conversion:

```sql
-- MIGRATE: run after dual-write code is deployed.
-- Product decision: reporting dates use the stored timestamp's calendar date.
UPDATE "app"."customer_events"
SET "occurred_on" = "occurred_at"::date
WHERE "occurred_on" IS NULL;
```

It also adds an assertion to `verify.sql`:

```sql
-- onwardpg:assert event_dates_backfilled
SELECT count(*) = 0
FROM "app"."customer_events"
WHERE "occurred_on" IS NULL;
```

### `contract.sql`

Only after old application instances and the rollback window are gone does the
contract enforce the final schema:

```sql
-- CONTRACT: no application instance may still use app.accounts or occurred_at.
ALTER TABLE "app"."customer_events"
  ALTER COLUMN "occurred_on" SET NOT NULL;
ALTER TABLE "app"."customer_events" DROP COLUMN "occurred_at";
DROP VIEW "app"."customers";
ALTER TABLE "app"."accounts" RENAME TO "customers";
```

The view drop and physical rename share a transactional contract batch, so new
code keeps the same `app.customers` name across the cutover.

Finally:

```sh
onwardpg verify --target primary
```

Verification replays accepted history, executes the exact edited phase files
and assertions in disposable PostgreSQL, and requires the final catalog to
match current exported DDL. This proves structural convergence of those bytes;
it does not simulate production data volume, application traffic, or rollout
timing.

## How the three phases live in a deployment

onwardpg produces a rollout artifact. It deliberately does not become the
deployment runner or environment journal. The three files are deployment
boundaries, while the batch comments *inside* each file describe whether a
statement can run in a transaction.

A typical handoff is:

```text
1. PR review       generate and verify the complete bundle
2. Pre-deploy      operator runs expand.sql
3. App deploy      old and new versions overlap; enable dual writes if needed
4. Data work       operator runs migrate.sql and observes its assertions
5. Compatibility  wait for stale instances and the rollback window to end
6. Cutover         operator runs contract.sql
7. Complete        deployment ledger records the bundle as fully executed
```

The application merge can happen between these steps according to the team’s
deployment process. The committed folder describes the whole transition; the
external runner or operator records which phases each environment has actually
executed.

Three states must not be conflated:

| State | Meaning |
| --- | --- |
| **Accepted history** | The valid bundle chain in the current checkout, usually after Git merge |
| **Planning baseline** | The terminal schema after replaying every phase of that accepted chain |
| **Environment progress** | The last phase actually executed in a particular environment |

onwardpg history is a planning chain, not an environment ledger. Bundle B is
planned and clone-verified after all phases of bundle A, including A’s
contract. It does not currently verify arbitrary interleaving such as
`A.expand → B.expand → A.contract`.

Therefore a deployment system must not start a later bundle in an environment
that has not completed its earlier bundle unless the operator independently
proves that interleaving safe. A contract can wait across application releases,
and later PRs can still be planned and merged, but their execution remains
ordered behind that pending contract.

Likewise, `drift check` compares production with the final schema represented
by the current repository chain. It is meaningful after production has fully
executed that chain. During an intentional partial rollout it will report the
partial state as a difference; onwardpg does not maintain hidden phase
exceptions.

The ground floor is a **logical** baseline derived from exported DDL, not an
adoption record for every physical name in an older production catalog. For
example, an older Drizzle migration may have a PostgreSQL-truncated foreign-key
name where a newer export uses a compact hash suffix. `init` still establishes
H = W because onwardpg plans from declarative history; `drift check` reports
the physical-name difference. It is deliberately not hidden or auto-renamed.
If a later migration must alter that legacy constraint, the developer or agent
must review and supply an explicit physical-to-declarative transition in the
bundle (which can safely account for both names during clone verification),
rather than asking onwardpg to infer a production alias.

There are no down migrations. Before contract, the preserved old interface is
the application rollback path. After contract, recovery is a new forward plan
owned by the operator and application team.

## One evolving migration per feature

While the feature is open, changing exported DDL and running `plan` again
replaces the same unexecuted logical bundle. It does not stack a migration for
every intermediate idea.

If a teammate’s migration lands underneath the branch:

```text
before rebase:  A ──▶ feature

after rebase:   A ──▶ B ──▶ regenerated feature
```

The developer or coding agent performs the Git fetch/rebase. onwardpg does not
inspect or mutate Git. The next `plan` call replays the chain now present in
the checkout, moves the active feature bundle from A to B, preserves still-valid
intent and agent-owned SQL, and invalidates changed decisions explicitly.

If history is forked, edited, missing an entry, or ambiguous after excluding
the selected feature bundle, onwardpg blocks rather than guess which chain is
accepted. The bundle’s content-addressed parent and file receipts make the
restack reviewable.

Regeneration treats `migrate.sql` and `verify.sql` as agent-editable. If both
the generator and agent changed the same phase, onwardpg preserves the current
bytes and reports a conflict with the old and new generated candidates. It
does not attempt a semantic SQL merge.

## Development databases are separate

onwardpg keeps three everyday comparisons distinct:

```text
accepted history ──▶ exported DDL     committed feature bundle
development DB   ──▶ exported DDL     local reconciliation only
accepted history ──▶ production       optional read-only drift audit
```

The development database never becomes migration history. It can be stale,
contain test data, have yesterday’s feature shape, or retain work from another
branch.

The default `workspace` mode preserves objects that exist only in development
instead of proposing destructive cleanup after a branch switch. `strict` is
for an intentionally disposable database. Durable and development questions
are scoped separately with `--hint` and `--dev-hint`; an answer for one is
never silently reused for the other.

For the first local exercise, review and run the actual bundle phases through
your existing database tooling. If the development database has already
diverged, ask for its remaining reconciliation:

```sh
onwardpg plan --target primary --output sql > /tmp/customer-profile.dev.sql
```

This stream is not “run everything now.” It contains explicit EXPAND, MIGRATE,
and CONTRACT banners plus transactional batch boundaries. The developer or
agent must execute only the phases appropriate to the local application state.
onwardpg writes the stream but does not apply it.

Catalog equality also cannot prove that a historical data-only migration ran.
An accepted bundle can opt a Boolean assertion into read-only development
inspection:

```sql
-- onwardpg:assert normalized_customer_email
-- onwardpg:dev-postcondition
SELECT count(*) = 0
FROM app.customer
WHERE email IS DISTINCT FROM lower(email);
```

`plan` evaluates only explicitly marked assertions, inside read-only
transactions. A failed check asks the agent to review the upstream data effect;
it does not authorize onwardpg to repair the database.

## The four schema states

The deeper documentation uses four short names:

| Name | Meaning | Role |
| --- | --- | --- |
| **H** | Terminal schema from replaying accepted bundles | Durable planning baseline |
| **W** | Complete CREATE DDL exported from current code | Desired schema after merge |
| **D** | Actual development catalog | Local reconciliation only |
| **P** | Production catalog | Optional read-only drift audit |

```text
durable feature bundle: H ──diff──▶ W
local reconciliation:   D ──diff──▶ W
periodic drift audit:    H ──diff──▶ P
```

You do not need these letters for the normal CLI loop; they are shorthand for
protocol and architecture discussions.

## Commands

```text
onwardpg init             establish a replayable baseline
onwardpg plan [name]      create, revise, or restack the active feature bundle
onwardpg status           inspect the active plan and hash-chain state
onwardpg verify           clone-verify exact bundle files
onwardpg drift check      read-only history-to-live-catalog audit

onwardpg diff             low-level explicit source-to-source diff
onwardpg history status   raw hash-chain diagnostic
onwardpg dev plan         development-reconciliation diagnostic
onwardpg draft            lower-level explicit-anchor compatibility command
```

The first group is the intended preview workflow. The second is available for
diagnostics and transition. None applies SQL to a caller-owned database.

In CI, select the bundle explicitly and reject stale DDL, altered history,
unanswered decisions, TODOs, unreceipted edits, failed assertions, or residual
schema differences:

```sh
onwardpg verify --target primary --bundle customer-profile --check
```

## PostgreSQL coverage

The current preview models schemas, tables, columns, defaults,
generated/identity columns, comments, collations, constraints, foreign-key
cycles, ordinary and partial indexes, sequences, enums, extensions, routines,
triggers, views, materialized views, common partition/index relationships,
RLS/policies, and ordinary table privileges within documented boundaries.

Broad modeling protects the diff from silently losing catalog state. It does
not mean every mutation has an overlap-safe automatic plan. The authoritative
matrix is [supported features](docs/supported-features.md), with
machine-readable evidence under [`parity/`](parity).

Explicit index-key collations such as `COLLATE "C"` are part of an index's
typed identity. Changing one follows the same concurrent replacement path as
another index-definition change; onwardpg does not treat a database server's
default collation as a migration it can safely apply.

## Philosophy and boundaries

onwardpg is intentionally narrow:

- forward-only; no down migrations;
- no execution against caller-owned databases;
- no migration runner, deployment controller, or environment journal;
- no framework adapters or ORM journal integration;
- no embedded coding agent or plugin API;
- no Git mutation, merge-base discovery, or PR hosting integration; and
- no guessing of renames, drops, casts, backfills, rollout timing, or business
  invariants.

## How it compares

| Tool | Strength | Difference from onwardpg |
| --- | --- | --- |
| [Migra](https://github.com/djrobstep/migra) | Direct PostgreSQL schema diff | No durable intent, generated compatibility window, clone receipts, or feature-bundle restacking |
| [Atlas](https://atlasgo.io) | Broad multi-dialect declarative diff and migration tooling | Much broader ecosystem; onwardpg concentrates on PostgreSQL, explicit ambiguity, editable phased handoff, and one evolving feature bundle |
| [Stripe pg-schema-diff](https://github.com/stripe/pg-schema-diff) | Excellent Go/PostgreSQL online DDL reference | Broader in several planner families; onwardpg adds typed rename intent, editable phase files, a no-apply boundary, and feature restacking |
| Drizzle Kit | TypeScript schema declaration, export, and migration snapshots | Drizzle can compile DDL for onwardpg; onwardpg replays actual PostgreSQL bundles and inventories live catalogs |
| Alembic / Django migrations | Mature ORM state and migration runners | They own framework state and execution; onwardpg accepts exported DDL and hands ordinary SQL back to the developer |

## Documentation

- [Architecture](docs/architecture.md)
- [Safety model](docs/safety-model.md)
- [CLI and JSON protocol](docs/protocol.md)
- [Supported PostgreSQL features](docs/supported-features.md)
- [Atlas parity inventory](docs/atlas-postgres-parity.md)
- [MIT license](LICENSE)
