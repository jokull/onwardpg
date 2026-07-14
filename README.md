# onwardpg

onwardpg is a forward-only PostgreSQL schema-diff planner built for coding
agents.

It is built for the way features actually evolve: declarative schema code
changes several times, developers apply partial work to imperfect local
databases, other migrations merge underneath the branch, and the PR should
still contain one reviewable migration from the latest accepted ground to the
schema that would exist after merge.

```text
one evolving feature migration
      +
replayable PostgreSQL history
      +
agent-owned SQL for product-specific work
      +
disposable clone verification
```

**onwardpg never applies SQL to development, staging, or production.** It
generates plans and verifies them only in databases it creates. The developer
or coding agent owns real execution because it has the application,
deployment, traffic, and operational context.

The approach comes from [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive migrations
from schema state, keep manual control, expose drift, and prove difficult work
on a clone.

## The model

onwardpg keeps four schemas deliberately separate:

| Name | Meaning | Used for |
| --- | --- | --- |
| **H** | Schema from replaying accepted onwardpg bundles | Durable migration baseline |
| **W** | CREATE-statement DDL exported from current code | Desired schema after merge |
| **D** | Actual development database catalog | Deliberate local reconciliation |
| **P** | Production catalog | Optional, read-only drift audit |

```text
durable PR migration:   H ──diff──▶ W
local workspace SQL:    D ──diff──▶ W
periodic drift audit:   H ──diff──▶ P
```

The first path becomes the one migration in the feature branch. The second is
never used as a migration baseline. A long-lived dev database may be stale,
contain test data, have partial old feature work, or retain objects from a
branch the developer just left.

### The plain-English version

| README term | Plain meaning |
| --- | --- |
| **History** | The migrations already accepted by the project |
| **Working schema** | The complete DDL exported from the code today |
| **Development database** | The database a developer is using locally |
| **Plan** | One named feature change that is regenerated as the feature evolves |
| **Bundle** | The folder committed for that plan, including SQL and receipts |
| **Receipt** | A record proving which inputs and exact files were checked |
| **Workspace mode** | Keep local leftovers; do not delete them just because this branch does not use them |

The letters H, W, D, and P are useful shorthand in the deeper sections, but
you can use onwardpg without memorising them.

## Install

The preview targets PostgreSQL 15–18 and requires Go 1.26 or newer:

```sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
onwardpg version
```

## Configure one target

Your ORM or application only needs to provide the authoritative
feature-development PostgreSQL DDL: use a checked-in file, or point onwardpg
at a command that produces it, such as `drizzle-kit export`. onwardpg does not
read Drizzle migration snapshots or journals.

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

Use `schema_file = "schema.sql"` when DDL is already a file. Django, Prisma,
SQLAlchemy, Drizzle, and handwritten DDL all work through this same boundary;
there are no framework adapters.

`scratch_database_env` is an administrative PostgreSQL URL on a server where
onwardpg may create and drop disposable databases. `dev_database_env` is read
only from onwardpg's perspective. Keep scratch credentials away from
production.

`dev_mode = "workspace"` is the default. It preserves dev-only surplus objects
instead of proposing drops merely because the current branch's DDL does not
contain them. Use `strict` only for an intentionally disposable database.

```sh
onwardpg config check
```

## Five minutes: from a diff to a migration rollout

This example starts with an easy change, then keeps evolving the same feature
until an ordinary add/drop diff is no longer enough. It assumes a development
database and a scratch PostgreSQL server where onwardpg may create disposable
databases:

```sh
export ONWARDPG_DEV_DATABASE_URL='postgres://postgres:secret@localhost:5432/myapp_dev?sslmode=disable'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:secret@localhost:5432/postgres?sslmode=disable'
```

For a local trial both URLs may use one server. The scratch credentials must
never point at production.

### 1. Start with the obvious additive change

Assume the accepted schema contains `app.accounts`, plus a lookup table:

```sql
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint PRIMARY KEY,
  occurred_at timestamp NOT NULL
);
CREATE TABLE app.profile_kinds (
  id bigint PRIMARY KEY
);
```

Set `schema_file = "schema.sql"` for this example—or point `schema_command` at
the authoritative feature-development DDL exporter, such as
`drizzle-kit export`—then establish the baseline once:

```sh
onwardpg config check
onwardpg init --target primary
```

The feature first adds profiles and an index. After changing the complete DDL,
start one feature plan:

```sh
onwardpg plan customer-profile --target primary
```

The generated `phases/expand.sql` contains the compatible additions:

```sql
-- EXPAND: run before application code starts using customer profiles.
CREATE TABLE "app"."customer_profiles" (
  "id" bigint NOT NULL,
  "kind_id" bigint NOT NULL,
  "biography" text,
  CONSTRAINT "customer_profiles_pkey" PRIMARY KEY ("id"),
  CONSTRAINT "customer_profiles_kind_id_fkey"
    FOREIGN KEY ("kind_id") REFERENCES "app"."profile_kinds" ("id")
);
CREATE INDEX "customer_profiles_kind_id_idx"
  ON "app"."customer_profiles" USING "btree" ("kind_id" NULLS LAST);
```

That is useful, but it is still table-stakes schema diffing.

### 2. Let the feature become awkward

The feature evolves: `accounts` should really be `customers`, and
`occurred_at timestamp` should become `occurred_at date`. Two schemas cannot
prove whether that is a rename or replacement, and they cannot know the
product's timestamp-to-date rule. onwardpg stops and offers bounded choices.
The agent already knows the feature intent, so it can answer both on the next
call:

```sh
onwardpg plan --target primary \
  --hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}' \
  --hint '{"kind":"type_change","name":["app","accounts","occurred_at"],"strategy":"manual_sql"}'
```

There is still one folder in the PR:

```text
migrations/onward/primary/customer-profile/
├── manifest.json
├── plan.json
├── decisions.json
├── verify.sql
└── phases/
    ├── expand.sql
    ├── migrate.sql
    └── contract.sql
```

`expand.sql` keeps the compatible profile additions. onwardpg leaves the
product-specific cast as an explicit TODO in `migrate.sql`; the agent replaces
it with reviewed SQL:

```sql
-- MIGRATE: run while the application still uses app.accounts.
-- Product decision: reporting dates use the stored timestamp's calendar date.
ALTER TABLE "app"."accounts"
  ALTER COLUMN "occurred_at" TYPE date
  USING "occurred_at"::date;
```

The rename is isolated in `contract.sql` because it changes the application
contract and must wait until compatible code is ready:

```sql
-- CONTRACT: run only when deployed code is ready for app.customers.
ALTER TABLE "app"."accounts" RENAME TO "customers";
```

The agent can add a Boolean assertion to `verify.sql`:

```sql
-- onwardpg:assert occurred_at_is_date
SELECT data_type = 'date'
FROM information_schema.columns
WHERE table_schema = 'app'
  AND table_name = 'customers'
  AND column_name = 'occurred_at';
```

Then onwardpg executes the exact edited phase files on a disposable clone and
requires the final catalog to match the exported DDL:

```sh
onwardpg verify --target primary
```

### 3. Now let `main` move underneath the feature

While the branch is open, a teammate merges a migration adding
`app.audit_log`. The coding agent rebases using Git and runs the same command:

```sh
onwardpg plan --target primary
```

onwardpg does not append a second speculative feature migration. It:

- replays the newly accepted history;
- moves the same logical `customer-profile` plan onto that new parent;
- carries forward still-valid rename intent;
- preserves the agent's exact `migrate.sql` and `verify.sql` edits;
- reports any incoming data-only work the catalog cannot prove; and
- clone-verifies the newly stacked result against current exported DDL.

The JSON receipt makes that movement explicit:

```json
{
  "durable": {
    "bundle_id": "customer-profile",
    "parent_changed": true,
    "answer_rebind": { "carried": ["rename_table:table:app:accounts"] },
    "edit_reconciliation": {
      "outcome": "reconciled",
      "preserved": ["phases/migrate.sql", "verify.sql"]
    }
  }
}
```

Meanwhile, the developer database may already contain yesterday's version of
the feature but not the teammate's change. The separate local output contains
only what that particular database still needs:

```sh
onwardpg plan --target primary --output sql
```

```sql
-- onwardpg development workspace reconciliation
-- this is not the cumulative PR migration
CREATE TABLE "app"."audit_log" ("id" bigint);
```

That combination is the point: one reviewed feature migration keeps evolving
over a moving accepted history, agent-owned data SQL survives the restack, and
local development receives a direct residual without becoming migration
history. That is the “whoa” moment: the hard part is no longer generating one
`ALTER TABLE`; it is preserving product intent and one merge-ready migration
while the code, local database, and accepted history all move independently.
onwardpg never applies any of it to a caller-owned database.

## The everyday workflow

### Establish a baseline once

```sh
onwardpg init --target primary
```

This creates a verified baseline from empty PostgreSQL to the current
W. For an existing application, it declares the ground beneath future onwardpg
migrations; it is not a command to run against production.

History is content-addressed. Parent and entry digests—not timestamps,
filenames, Git commits, or an ORM journal—determine ordering.

### Start one feature migration

```sh
onwardpg plan booking-dates --target primary
```

The first invocation creates one logical plan and records a small gitignored,
worktree-local anchor under `.onwardpg/`. Its bundle gets a stable `plan_id` in
`manifest.json`.

If a branch switch removes that bundle from the checkout, start or resume the
other branch’s named plan with `onwardpg plan NAME`. onwardpg parks the old
local PlanID and restores it when its bundle returns; it does not inspect Git
or infer ownership from schema differences. If the old bundle is still present,
starting a second mutable plan is blocked explicitly.

The command then:

1. excludes only that selected bundle from candidate base history;
2. validates and replays every remaining bundle in disposable PostgreSQL;
3. exports the schema and loads it into temporary PostgreSQL for a real catalog
   comparison;
4. plans the committed migration from accepted history to the exported schema;
5. plans the independent D → W development reconciliation; and
6. writes the bundle when its generated part is reviewable, leaving any
   product-specific TODO or decision explicit.

The default JSON response contains separate `durable` and `development`
outcomes. This matters: a complete PR bundle and an ambiguous local database
are different facts, not one blended status.

### Let onwardpg ask only what it cannot know

If `occurred_at` disappeared and `occurred_on` appeared, schema state cannot
prove whether that is a rename or a deliberate replacement. onwardpg returns
bounded semantic choices such as:

```json
{
  "kind": "rename",
  "object": "column",
  "from": ["public", "booking", "occurred_at"],
  "to": ["public", "booking", "occurred_on"]
}
```

The agent can provide known intent on the first call or rerun with it:

```sh
onwardpg plan --target primary \
  --hint '{"kind":"rename","object":"column","from":["public","booking","occurred_at"],"to":["public","booking","occurred_on"]}'
```

Hints state only product intent unavailable from catalog state: rename,
destructive intent, a supported type strategy, or a deliberate manual SQL
handoff. They are validated against the live graph diff, fingerprint-bound to
the relevant objects, and receipted in the bundle. Stale, contradictory,
impossible, and unused hints are rejected.

The nested `development` report can independently need a decision because D
is not H. This is most common for a deliberately disposable `strict` database
or an incompatible same-name object. Its `next_action` is
`rerun_plan_with_dev_hints`; use the same semantic shape, but make the scope
explicit:

```sh
onwardpg plan --target primary \
  --dev-hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}'
```

`--hint` only answers the durable H → W migration. `--dev-hint` only answers
the local D → W reconciliation. Neither is silently reused for the other.
In the default `workspace` mode, an `accounts` table absent from W is preserved
as possible work from another branch; onwardpg will not ask to rename or drop
it merely to make D exact.

### Apply local SQL deliberately

When both comparisons are ready, ask for a pure SQL stream:

```sh
onwardpg plan --target primary --output sql > /tmp/booking-dates.dev.sql
```

stdout contains only the D → W workspace reconciliation. It begins with:

```sql
-- onwardpg development workspace reconciliation
-- this is not the cumulative PR migration
```

The developer or agent reviews and applies that SQL through its existing
database tooling. onwardpg has not run it.

If D already contains an older version of the feature and main has advanced,
the local SQL is planned directly from D to W. It is not a concatenation of
incoming migration files and feature increments. That distinction matters when
old and incoming changes overlap.

### Keep iterating

After changing the declarative schema again:

```sh
onwardpg plan --target primary
```

The same logical bundle is replaced with the cumulative H → new-W transition.
The local reconciliation is only whatever this particular D database still
needs. Applying an earlier draft to D does not advance history and does not
lock the feature plan.

## What gets merged, and when it runs

The PR contains **one logical feature migration**, not one giant SQL statement.
The quickstart showed why it is split into ordinary files: a safe rollout often
crosses more than one application release.

```text
expand.sql    make the new shape available while old code still works
migrate.sql   move or validate data while both shapes are supported
contract.sql  remove the old shape after old code is gone
```

In a squash-merge workflow, the feature PR still carries this one folder. The
developer or deployment system may run `expand.sql` before the application
merge, deploy code that understands both shapes, run the reviewed data work,
and wait to run `contract.sql` until the old code is gone. The exact cutover is
an application and traffic decision, not something onwardpg guesses. The
important review question is visible in the files: “what can run before the
merge, what needs the compatibility window, and what must wait until after it?”

A phase may be empty. onwardpg drafts the structural work and verifies the
complete bundle on a disposable clone; the developer or agent owns
product-specific SQL and the deployment system owns when each phase runs.

## A multi-day branch and incoming migrations

Suppose your active plan was based on A. A teammate's accepted bundle B lands
on main while your feature is open:

```text
old checkout:    A ──▶ your active plan
after rebase:    A ──▶ B
                     ╲
                      └──▶ your active plan (still names A)
```

The developer or agent performs the fetch/rebase/merge using normal Git tools.
Then it simply runs:

```sh
onwardpg plan --target primary
```

onwardpg excludes the explicitly active plan, sees the one remaining accepted
chain ending at B, replays B, and replaces the active bundle with B → W. No
copied Git SHA, merge-base, `head_ref`, `--after`, `--create`, or Git command
is needed in the normal path.

If removing the selected plan leaves multiple possible base heads, altered
history, a missing parent, or accepted descendants, onwardpg blocks. The
exceptional `--onto` path is reserved for an explicitly supplied
content-bound head once that ambiguity is implemented; onwardpg never guesses
which fork is accepted.

`onwardpg status --target primary` is the read-only way to inspect the active
plan and its base relationship. `history status` remains an expert diagnostic
for the raw hash chain. `draft --after --create` remains available during the
preview as a lower-level compatibility surface.

## Long-lived development databases

Development is intentionally not a strict migration path.

In workspace mode onwardpg:

- creates or changes objects required by W;
- reports objects found only in D as `preserved` surplus state;
- does not ask to drop those objects merely because the current branch lacks
  them; and
- blocks or asks for intent when an existing same-name object is incompatible.

It also records, rather than rebuilds, a PostgreSQL column-order mismatch that
can arise when an incoming migration adds a column before columns already
appended in D. The required column is added at PostgreSQL’s reachable end of
the table and the result is marked `workspace_compatibility`; strict history
and clone verification still require exact physical order.

This makes branch switching safe by default. If branch A added
`booking.cancellation_reason` and branch B's schema does not mention it,
switching to B reports that column as surplus; it does not emit `DROP COLUMN`.

Exact equality is still required in disposable clone verification. A periodic
strict audit or a deliberately rebuilt sandbox is how abandoned dev state is
cleaned up.

### Data work is not catalog state

Desired DDL cannot recover a data-only operation:

```sql
UPDATE customer SET email = lower(email);
```

It also cannot reconstruct a product-specific backfill from a target column
definition. When the accepted base advances, onwardpg inventories every
incoming phase: generated structural phases are distinct from generated
`migrate`/`manual` phases, agent-authored phases, and verification assertions.
The latter categories are marked `requires_review` because their data or
operational effects cannot be proven from D's catalog. They are warnings for
the agent or developer to review, not SQL silently replayed into dev.

This is deliberate. A schema-compatible dev database is not proof that every
historical data operation ran in the same order.

### Optional evidence for an upstream data step

An upstream bundle can attach a Boolean postcondition to a product backfill.
Most `verify.sql` assertions run only in disposable clone verification. Mark
one explicitly when it is also safe to inspect against a caller-owned dev
database:

```sql
-- onwardpg:assert normalized_customer_email
-- onwardpg:dev-postcondition
SELECT count(*) = 0
FROM app.customer
WHERE email IS DISTINCT FROM lower(email);
```

After a restack, `plan` runs only marked checks inside PostgreSQL
**read-only transactions**. It reports `passed`, `failed`, or a read-only query
error in the development portion of its JSON; it never runs an unmarked
assertion, migration phase, or write against D. A failed check means “review
the upstream data effect,” not “infer a repair from the catalog.”

```text
-- accepted development postcondition passed:
--   normalize-email/verify.sql#normalized_customer_email
```

This is deliberately a tiny opt-in annotation, not an orchestration DSL.
Product backfills still live as ordinary SQL in the migration folder.

## Review the durable bundle

The PR contains ordinary SQL files:

```text
migrations/onward/primary/booking-dates/
├── manifest.json       # plan ID, chain receipts, fingerprints, file digests
├── decisions.json      # accepted semantic intent
├── plan.json           # generated graph plan and hazards
├── verify.sql          # agent-owned boolean assertions
└── phases/
    ├── expand.sql
    ├── migrate.sql
    └── contract.sql
```

Comments explain why a statement exists, phase sequencing, locking, rewrite,
validation, availability, and data-loss hazards. The sections are intended to
be handed to an agent or developer:

```sql
-- EXPAND
-- Purpose: introduce replacement representation before compatible code deploys.
-- Timing: run before dual-write application code.

ALTER TABLE public.payments ADD COLUMN settled_on date;
```

```sql
-- MIGRATE
-- Product-specific conversion is not derivable from schema state.
-- ONWARDPG TODO: supply reviewed backfill and a verification assertion.
```

```sql
-- CONTRACT
-- Run after old application versions no longer use payments.settled_at.
-- Hazard: data loss and ACCESS EXCLUSIVE lock.

ALTER TABLE public.payments DROP COLUMN settled_at;
```

The agent may replace TODOs, add batching, write deduplication or backfill SQL,
and add assertions to `verify.sql`. There is no JSON DSL for application
orchestration. SQL is the last-mile extension point.

Use `-- onwardpg:dev-postcondition` only for an assertion that is meaningful
against a non-linear developer database. It receives the same Boolean-query
contract as every `verify.sql` assertion, but onwardpg additionally uses a
read-only transaction before treating its result as development evidence.

Regeneration preserves agent-only phase edits and `verify.sql`. If both the
generator and agent change the same phase, onwardpg leaves the current bytes in
place and reports a conflict with the old and new generated candidates. It
does not attempt a semantic SQL merge.

## Verify the exact bytes

```sh
onwardpg verify --target primary
```

With an active local plan, no bundle name is needed. In CI or a fresh checkout,
select it explicitly:

```sh
onwardpg verify --target primary --bundle booking-dates --check
```

Verification creates disposable PostgreSQL databases, replays immutable parent
history, runs the exact edited phase files in order, evaluates `verify.sql`,
and requires an empty residual diff against W. A successful receipt proves
structural convergence of those bytes on a clone. It does not prove production
traffic, deployment timing, application compatibility, or data-volume safety.

`--check` is read-only and rejects stale desired DDL, changed history,
unanswered decisions, TODOs, unreceipted edits, failed assertions, and residual
diffs.

## Production drift is optional and separate

```sh
onwardpg drift check \
  --target primary \
  --database "$PRODUCTION_DATABASE_URL"
```

This compares replayed H with P read-only. It is useful for a periodic check
that production has not gained an unrecorded index or missed a migration. It
is not part of ordinary feature planning and never becomes the PR migration
baseline.

## Command surface

```text
onwardpg init             establish a replayable baseline
onwardpg plan [name]      create, revise, or restack the active feature plan
onwardpg status           inspect the local active plan and hash-chain state
onwardpg verify           clone-verify the active plan
onwardpg drift check      read-only history-to-live-catalog audit

onwardpg diff             low-level explicit source-to-source diff
onwardpg history status   raw hash-chain diagnostic
onwardpg dev plan         direct development reconciliation diagnostic
onwardpg draft            lower-level explicit-anchor compatibility command
```

The first group is the intended developer-preview loop. The second group is
kept for diagnostics and transition; none of these commands applies SQL to a
caller database.

## Current PostgreSQL coverage

onwardpg is PostgreSQL-only. It inventories catalog state through PostgreSQL
itself and blocks unknown or unmodeled state rather than treating absence from
the graph as equivalence.

The current preview covers the core relation and migration families used in
the workflow: schemas, tables, columns, defaults, generated/identity columns,
comments, collations, constraints, foreign-key cycles, ordinary and partial
indexes, sequences, enums, extensions, routines, triggers, views,
materialized views, common partitions/index relationships, RLS/policies, and
ordinary table privileges within the documented boundaries.

The authoritative detailed matrix is [supported features](docs/supported-features.md)
and the machine-readable inventories under [`parity/`](parity). A feature is
always described as modeled, manually completable, explicitly blocked, or
unverified—never silently ignored.

## Philosophy and boundaries

onwardpg is intentionally narrow:

- forward-only; no down migrations;
- no automatic application to any caller-owned database;
- no migration runner, deployment controller, or environment journal;
- no framework adapters or ORM journal integration;
- no embedded coding agent or plugin API;
- no Git mutation, merge-base discovery, or PR hosting integration; and
- no guessing of renames, drops, casts, backfills, rollout timing, or business
  invariants.

Git remains useful to the agent: it fetches, rebases, resolves code conflicts,
and provides the checkout. onwardpg remains useful because it proves whether
the migration files currently in that checkout form one replayable history and
one safe path to W.

## How it compares

| Tool | Strength | Difference from onwardpg |
| --- | --- | --- |
| [Migra](https://github.com/djrobstep/migra) | Direct PostgreSQL schema diff | Python, no durable intent protocol, phased SQL ownership, clone receipts, or feature-plan restacking |
| [Atlas](https://atlasgo.io) | Broad multi-dialect declarative diff and migration tooling | Much broader ecosystem; onwardpg concentrates on PostgreSQL, forward-only handoff, explicit ambiguity, and one evolving feature bundle |
| [Stripe pg-schema-diff](https://github.com/stripe/pg-schema-diff) | Excellent Go/PostgreSQL online DDL reference | Broader in several native planner families; onwardpg keeps a typed graph plus rename intent, editable phased bundles, no-apply boundary, and branch-lifecycle workflow |
| Drizzle Kit | TypeScript schema declaration, export, and migration snapshots | Drizzle is a DDL compiler for onwardpg; `generate` compares schema snapshots, while onwardpg replays real PostgreSQL bundles and sees live catalog drift |
| Alembic / Django migrations | Mature ORM state and migration runners | Own application framework state and execution; onwardpg accepts exported DDL and leaves execution with the developer or agent |

## Documentation

- [Architecture](docs/architecture.md)
- [Safety model](docs/safety-model.md)
- [CLI and JSON protocol](docs/protocol.md)
- [Supported PostgreSQL features](docs/supported-features.md)
- [Atlas parity inventory](docs/atlas-postgres-parity.md)
