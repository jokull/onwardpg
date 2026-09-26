---
title: onwardpg
description: Plan and verify PostgreSQL schema migrations for rolling deployments.
seo:
  title: onwardpg — PostgreSQL migration quick start
  description: Install onwardpg, plan a PostgreSQL schema change, and verify the SQL. Learn how expand and contract fit around a rolling application deployment.
---

onwardpg turns your desired PostgreSQL schema into SQL for a rolling deployment.
It generates **expand** SQL to run before the application deploys and **contract**
SQL to run after the old processes have drained. It verifies both in disposable
PostgreSQL databases. Your deployment system runs the SQL in production.

Jump to the [quick start](#quick-start), or read on for the deployment problems
it addresses.

## Why do I need this?

Locally, you can stop the app, migrate the database, and restart it. During a
rolling deployment, old and new instances share the same database. A worker
may still be processing yesterday's job while the new release serves requests.
A migration can execute successfully and still break those processes.

These changes need more than a single schema cutover:

- **Renaming a column.** Rename `display_name` to `full_name` before deploying,
  and old queries fail. Deploy first, and new queries fail. For supported
  same-type renames, onwardpg adds a second column and a trigger that keeps
  writes through either name synchronized. You choose how to backfill existing
  rows. Contract checks that the values agree and removes the bridge after old
  callers drain.
- **Adding a required field.** `status text NOT NULL` can reject inserts from
  old code that doesn't supply a status. Expand adds it as nullable. The new
  release writes the field and tolerates nulls during rollout; contract runs
  your cleanup, checks for remaining nulls, and applies `NOT NULL`.
- **Changing allowed values.** Suppose a CHECK accepts `email` and `slack`,
  while the new release writes `slack` and `push`. Either endpoint constraint
  rejects one version's writes. Expand can remove the old check; contract uses
  your rule for retired values, checks the data, and restores enforcement.
  That temporarily weakens the database guarantee, which the plan reports.
- **Changing a type beneath views and indexes.** Converting `age text` to
  `age integer` needs a rule for blanks and malformed values, a way to support
  both application versions, and work on dependent views. A materialized view
  may also need rebuilding or refreshing. onwardpg identifies those
  dependencies, asks for the conversion and compatibility SQL, and verifies
  the completed bundle.

The order is **expand → deploy → drain → contract**. The new application must
handle the overlap schema and the final schema, including values old writers
can still produce. If that cannot fit one deployment, split the change across
releases. onwardpg asks for the missing decisions and SQL; it cannot infer
application behavior or a backfill's business meaning from the schema.

## Quick start

### Install

```sh
brew install jokull/tap/onwardpg
onwardpg version
```

This is a preview release. [Release archives](https://github.com/jokull/onwardpg/releases)
and [installation from Go or source](https://github.com/jokull/onwardpg/blob/main/docs/installation.md)
are also available.

You'll need PostgreSQL 15–18 for scratch databases. Use a dedicated local or CI
cluster: onwardpg creates and drops databases and temporary login roles there.
The scratch login needs permission to manage both. Never use a production or
shared application cluster for scratch.

For a local walkthrough with Docker:

```sh
docker run --name onwardpg-scratch \
  -e POSTGRES_PASSWORD=onwardpg \
  -p 127.0.0.1:55432:5432 -d postgres:18

export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:onwardpg@localhost:55432/postgres'
```

Wait for PostgreSQL to accept connections before continuing.

### Describe the starting schema

In an empty project directory, create `schema.sql`:

```sql
CREATE SCHEMA app;

CREATE TABLE app.bookings (
  id bigint PRIMARY KEY
);
```

Add `.onwardpg.toml` alongside it:

```toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
```

Check the configuration and record the starting schema:

```sh
onwardpg config check
onwardpg init
```

`init` creates and verifies the baseline migration history. In an existing
project, initialize from the schema you have **before** the feature change.
It does not change your application database.

### Add a column

Suppose bookings now need a status. Replace `schema.sql` with:

```sql
CREATE SCHEMA app;

CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  status text
);
```

The file always describes the complete desired schema. Run:

```sh
onwardpg plan add-booking-status
```

The bundle is written to `migrations/onward/app/add-booking-status/`. Its
`plan.json` lists the statements and hazards; `phases/expand.sql` contains:

```sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;
```

There is no contract phase for this change. Old code can keep inserting rows
without a status. Check that inserts name their columns and that readers can
handle an extra result column if they use `SELECT *`.

### Review and verify

Read the generated SQL and hazards, then run:

```sh
onwardpg verify
```

Verification replays the migration in disposable databases, runs its assertions,
and checks that the final schema matches `schema.sql` with no remaining diff.
Unresolved SQL placeholders fail verification.

Commit the schema, configuration, and generated migration files together. In CI,
select the bundle explicitly and check its saved verification evidence:

```sh
onwardpg verify --bundle add-booking-status --check
```

Use the same PostgreSQL major for scratch and production. Scratch also needs
the roles, languages, and extension packages referenced by your schema. Project
SQL runs as a restricted database owner, so extensions requiring superuser
execution need separate provisioning.

## Required columns

Before merging the feature, suppose you decide every booking must have a status.
Change the column in `schema.sql` to `status text NOT NULL` and rerun:

```sh
onwardpg plan
```

This revises the same bundle. Expand still adds a nullable column: the old
application will omit `status` until it drains. The new application must write
a status and tolerate nulls when reading during that overlap.

The planner asks how those nulls will be resolved. Choose `assert_only` if
another operation will fill them, `manual_sql` to supply cleanup in this plan,
or `split_plan` to defer enforcement to a later release.

For this example, assume old bookings should become `pending` and the table is
small enough for one update. Supply that decision:

```sh
onwardpg plan \
  --hint '{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}' \
  --hint '{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}'
```

The command reports `needs_sql_edits` and names the file to edit. In
`phases/contract.sql`, replace the placeholder **inside the edit markers** with:

```sql
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
```

Keep the surrounding generated SQL. After this cleanup it checks that no nulls
remain, then enforces the constraint:

```sql
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
```

Run `onwardpg verify` again. The cleanup rule is yours to review; the schema
cannot tell onwardpg what an old booking means. For a large table, use an
`operator_batched` operation with a completion check, or split the work across
releases. See the [contract reference](https://github.com/jokull/onwardpg/blob/main/docs/contract-readiness.md)
for those options.

## Day-to-day use

Run `onwardpg plan` as you edit the schema. It updates the active migration
instead of adding a migration for every draft. After rebasing onto new migration
history, run it again. Decisions and SQL edits carry forward when they still
apply; changes to their underlying schema can require another answer or review.

```sh
onwardpg plan       # Revise the active plan
onwardpg status     # Inspect it
onwardpg verify     # Verify the current files
```

For renames, type conversions, and destructive changes, follow the questions in
`next_actions`. Answer with the reported `--hint` and edit only the named SQL
sections. Exit code `2` means the plan needs input or SQL edits. A rename needs
an explicit identity decision; a type conversion needs a rule for the data.
Adding another compatible column can introduce a new rename candidate, so a
previous rename decision may need confirming again. That keeps a changed set
of possible identities from silently reusing an earlier answer. Use the complete
suggested command (`argv`) for each answer: it can carry confirmed decisions
that have not yet been saved while your edited SQL waits for the remaining choices.

### Use your framework's schema

Instead of maintaining SQL by hand, export your framework's complete desired
PostgreSQL DDL. Replace `schema_file` with a command that writes it to stdout:

```toml
schema_command = ["pnpm", "--silent", "schema:export"]
```

`schema:export` is a script you provide. It must produce the same DDL for the
same models, including everything needed to build the schema from empty.
See the [schema input requirements](https://github.com/jokull/onwardpg/blob/main/docs/schema-inputs.md)
and [existing-project recipes for Drizzle, Prisma, and Django](https://github.com/jokull/onwardpg/tree/main/examples/frameworks).
Choose one executor for production DDL; running both framework migrations and
the onwardpg bundle would apply the same change twice.

Adopt from an already-deployed schema: export the old models and run `init`
before the feature edit. Keep existing framework migration history, but do not
replay the onwardpg baseline against an existing database. Django's example
exports migration state, so run `makemigrations` after model changes and use
reviewed state-only migrations when onwardpg takes over their DDL.

### Connect a development database

Optionally add these fields to `[targets.app]`:

```toml
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
dev_mode = "workspace"
```

Set that environment variable to your development database URL. onwardpg reads
its catalog and reports separate SQL to bring it up to date. In workspace mode,
objects left by another branch are preserved. Migration history still comes
from accepted bundles. The development and scratch databases must use the same
PostgreSQL major.

This comparison has its own `--dev-hint` decisions. A ready durable bundle can
still return exit code `2` because local reconciliation needs an answer; inspect
the `durable`, `development`, and `next_actions` sections separately.

### Work with a coding agent

Give the agent <a href="/skill.md">the onwardpg skill</a>:

```text
Read and follow https://onwardpg.solberg.is/skill.md.
Plan this schema change, answer decisions from the application code,
verify the bundle, and report the remaining deployment checks.
```

## Deployment

Each bundle surrounds one application deployment:

```text
expand → deploy application → drain old processes → contract
```

1. **Apply expand** before deploying the new code. Review lock, scan, index-build,
   and transaction requirements in the plan. Keep nontransactional batches out
   of transactions.
2. **Deploy the application.** It must work both before and after contract.
3. **Drain old processes.** Include workers, scheduled jobs, queues, connection
   pools, and any other client that could still write using the old schema.
4. **Check readiness, then apply contract.** Finish the specified data cleanup
   and require a ready result before enforcing constraints or removing old interfaces.

For bundles with contract work, your release system supplies expiring writer
records in `deploy-readiness.json`. The file identifies the exact plan, bundle,
environment, and release. Check it against production through a restricted
read-only observer:

```sh
onwardpg contract check \
  --target app \
  --bundle add-booking-status \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json
```

`ready` permits proceeding. `reconciliation_required` identifies cleanup your
release runner must complete before checking again. `needs_evidence`, `blocked`,
and `stale` need resolution. The [contract reference](https://github.com/jokull/onwardpg/blob/main/docs/contract-readiness.md)
defines the evidence file, observer permissions, and cleanup order. Contract SQL
repeats the data assertions at enforcement time.

Verification proves the migration runs and reaches the requested schema in
scratch. It does not prove your production data is valid, locks will be short,
replicas can keep up, or old processes have drained. Review those against the
actual workload. Plans are forward-only; arrange your application rollback
procedure before deploying.

## Reference

Use `onwardpg <command> --help` for flags. The repository has the detailed
[CLI reference](https://github.com/jokull/onwardpg/blob/main/docs/cli.md),
[supported features](https://github.com/jokull/onwardpg/blob/main/docs/supported-features.md),
[bundle format](https://github.com/jokull/onwardpg/blob/main/docs/bundles.md), and
[safety model](https://github.com/jokull/onwardpg/blob/main/docs/safety-model.md).

onwardpg is [MIT licensed](https://github.com/jokull/onwardpg/blob/main/LICENSE).
[Source and issues](https://github.com/jokull/onwardpg) are on GitHub.
