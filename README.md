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
Where it has a supported transition, onwardpg stages it; otherwise it asks for
intent or blocks rather than disguising a breaking cutover with a safe phase
label.

Repeated `plan` calls replace the same feature bundle as its DDL evolves or new
migrations land underneath it. Still-valid developer- or agent-authored SQL
survives regeneration; conflicting edits stop for review. `verify` executes
the exact files on disposable PostgreSQL and proves structural convergence to
the exported DDL.

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
catalog inventory recognizes many PostgreSQL object families, but recognition
does not imply an automatic online transition for every mutation.

Where onwardpg has a supported transition, it generates it. Where schema state
cannot establish a safe path, it asks for intent, creates an editable SQL
handoff, or blocks. It does not label a direct cutover “safe” merely because
the final catalog would match.

The detailed, test-linked boundary lives in [supported
features](docs/supported-features.md). Unsupported state is reported; it is not
silently discarded.

## Quickstart

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
`onwardpg config check` validates every selector and reports what it excludes.
Use exact selectors: each one is an explicit blind spot.

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

Simple PostgreSQL views are not universal table aliases: new code cannot assume
table-only behavior such as `ON CONFLICT` or named constraints. onwardpg uses
this bridge only for supported shapes and blocks the rest; see the [feature
matrix](docs/supported-features.md).

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
match current exported DDL. It proves structural convergence for that clone and
those bytes—not production data volume, traffic, or rollout timing.

## Deployment handoff

The files describe deployment timing, not transaction scope: `expand.sql` is
run before compatible code relies on the new shape; `migrate.sql` is the
post-deploy data-work handoff; and `contract.sql` waits for the compatibility
window to close. Batch comments inside each file mark transactional versus
non-transactional SQL.

onwardpg is neither the deployment runner nor an environment journal. The
operator records phase progress and gates traffic, dual writes, locks, and
rollback windows. It must not execute a later bundle in an environment whose
earlier bundle has not completed unless that interleaving is independently
proven safe. Read the [migration workflow](docs/migration-workflow.md) for the
full deployment model, including partial-rollout drift and legacy physical-name
differences.

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
the checkout, moves the active feature bundle from A to B, and preserves
still-valid intent and agent-owned SQL. A conflicting phase edit blocks for
review rather than receiving a speculative SQL merge.

The hash-chained bundle history blocks forks, edits, missing entries, and
ambiguous ordering. See the [workflow guide](docs/migration-workflow.md) for
restacking and conflict details.

## Development and drift

Planning compares accepted history with exported DDL. Development reconciliation
and production drift are separate, read-only comparisons; neither becomes
migration history. The default workspace mode preserves development-only
objects so branch switching does not suggest drops. For a local reconciliation:

```sh
onwardpg plan --target primary --output sql > /tmp/customer-profile.dev.sql
```

This is not “run everything now”: it contains phase and batch markers, and the
developer or agent executes only SQL appropriate to local application state.
Read [development and drift semantics](docs/migration-workflow.md#development-without-pretending-it-is-history)
before using it on a long-lived database.

## Commands

```text
onwardpg init             establish a replayable baseline
onwardpg plan [name]      create, revise, or restack the active feature bundle
onwardpg status           inspect the active plan and hash-chain state
onwardpg verify           clone-verify exact bundle files
onwardpg drift check      read-only history-to-live-catalog audit
```

These are the intended preview workflow. None applies SQL to a caller-owned
database; lower-level compatibility commands are in the [CLI reference](docs/cli.md).

In CI, select the bundle explicitly and reject stale DDL, altered history,
unanswered decisions, TODOs, unreceipted edits, failed assertions, or residual
schema differences:

```sh
onwardpg verify --target primary --bundle customer-profile --check
```

## PostgreSQL coverage

The current preview inventories schemas, tables, columns, defaults,
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

onwardpg is complementary to declarative schema tools and migration runners:
it accepts their exported PostgreSQL DDL, plans one reviewable forward bundle,
and leaves application-aware SQL and execution with the team. The detailed,
evidence-linked comparison with Migra, Atlas, Stripe pg-schema-diff, Drizzle,
Alembic, and Django is in [the ecosystem comparison](docs/ecosystem-comparison.md).

## Documentation

- [Architecture](docs/architecture.md)
- [Safety model](docs/safety-model.md)
- [CLI and JSON protocol](docs/protocol.md)
- [Supported PostgreSQL features](docs/supported-features.md)
- [Atlas parity inventory](docs/atlas-postgres-parity.md)
- [MIT license](LICENSE)
