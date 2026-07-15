# onwardpg

**A PostgreSQL schema-diff planner that generates the compatibility window,
not just the final `ALTER` statements.**

![A traveler surveying the safe path onward through a changing landscape](docs/assets/onwardpg.png)

Most migration generators answer “what SQL makes these schemas equal?” That is
necessary, but it is not enough during a rolling deployment. A direct column
rename can break an old process that is still draining. A required column can
break an old writer. A correct data conversion may depend on product meaning
that no database catalog contains.

onwardpg plans one forward-only migration bundle around exactly one application
deployment:

1. **Compare** accepted migration history with the authoritative exported
   `CREATE`-statement DDL.

2. **Expand** by running `expand.sql` while the old application is still live.

3. **Deploy** one application version against the compatible expanded schema.

4. **Drain** old instances, requests, workers, connection pools, queues, and
   stale write paths.

5. **Contract** by running `contract.sql` only after the old application is
   gone.

6. **Converge** on the desired schema with no residual diff.

`expand.sql` must be safe while the old application is live. `contract.sql`
runs after old instances and write paths are gone. The newly deployed version
must work on both sides of contract. If that is impossible, the feature needs
two application deployments—and therefore two onwardpg plans. onwardpg says so
instead of hiding a second deployment inside a phase name.

This follows the philosophy in [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive forward plans
from real schema state, review the SQL, expose drift, and test difficult work on
a clone. There are no generated down migrations. A rollback is another forward
plan.

## Install

Homebrew is the recommended installation path on macOS and Linux:

```sh
brew install jokull/tap/onwardpg
onwardpg version
```

Go developers can install the same tagged preview directly:

```sh
go install github.com/jokull/onwardpg/cmd/onwardpg@v0.1.0-preview.1
```

Checksummed binaries for macOS, Linux, and Windows are available from [GitHub
Releases](https://github.com/jokull/onwardpg/releases). See [installation and
release verification](docs/installation.md) for details.

## Why exported SQL is the boundary

Django, Drizzle, Prisma, SQLAlchemy, and other tools already turn declarative
code into PostgreSQL DDL. onwardpg does not need framework adapters or their
private migration journals. Point it at a complete SQL file, **or point it at a
command that gives the authoritative feature-development DDL, such as
`drizzle-kit export`**.

That gives onwardpg one language- and framework-independent input: PostgreSQL
`CREATE` statements. It materializes that DDL in disposable PostgreSQL and
compares typed catalog graphs, rather than relying on a partial SQL parser.

## Five-minute tour

### 1. Configure one database

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_file = "schema.sql"
# Or: schema_command = ["pnpm", "--silent", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

The development database is inspected read-only. The scratch URL must allow
onwardpg to create and drop disposable databases; it must never point at
production.

```sh
export ONWARDPG_DEV_DATABASE_URL='postgres://postgres:secret@localhost/myapp_dev'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:secret@localhost/postgres'
onwardpg config check
onwardpg init
```

`init` records the replayable ground floor once. It does not connect to or
change production. When a repository configures exactly one target, `--target`
is unnecessary. Multi-database repositories select one explicitly.

Managed services often add objects you do not own. A reviewed target-level
ignore list can exclude exact provider-owned objects such as a Supabase
extension:

```toml
ignore = ["extension:pg_stat_statements", "schema:auth"]
```

Every selector is validated and reported. It is an explicit blind spot, not a
wildcard excuse to discard unknown catalog state.

### 2. Start with an additive change

Add a table to the complete exported DDL, then start one logical feature plan:

```sh
onwardpg plan customer-profile
```

The new bundle contains an `expand.sql` like this:

```sql
-- EXPAND — run before the one application deployment anchored to this plan.
-- onwardpg:batch transactional
CREATE TABLE "app"."customer_profiles" (
  "id" bigint NOT NULL,
  "biography" text
);
ALTER TABLE "app"."customer_profiles"
  ADD CONSTRAINT "customer_profiles_pkey" PRIMARY KEY ("id");
```

There is no `contract.sql`: nothing has to wait for old code to drain.

### 3. Escalate to a real rolling-deploy problem

Now rename `app.accounts.display_name` to `full_name` in the exported DDL.
Two schema snapshots cannot prove whether this is a rename or a drop plus a new
column, so `plan` returns a small, machine-readable decision. A developer can
choose it, or an agent that already understands the feature can rerun the same
command with the offered intent:

```sh
onwardpg plan \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}'
```

For an eligible same-type column, onwardpg does not emit a dangerous direct
rename. It generates a writable overlap.

`expand.sql` adds the new name, installs a deterministic trigger, and backfills
after synchronization is active:

```sql
ALTER TABLE "app"."accounts" ADD COLUMN "full_name" text;

CREATE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"()
RETURNS trigger LANGUAGE plpgsql AS $onwardpg$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW."display_name" IS NULL THEN
      NEW."display_name" := NEW."full_name";
    ELSIF NEW."full_name" IS NULL THEN
      NEW."full_name" := NEW."display_name";
    ELSIF NEW."display_name" IS DISTINCT FROM NEW."full_name" THEN
      RAISE EXCEPTION 'onwardpg column bridge conflict on app.accounts: display_name and full_name differ'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW."display_name" IS DISTINCT FROM OLD."display_name"
    AND NEW."full_name" IS NOT DISTINCT FROM OLD."full_name" THEN
    NEW."full_name" := NEW."display_name";
  ELSIF NEW."full_name" IS DISTINCT FROM OLD."full_name"
    AND NEW."display_name" IS NOT DISTINCT FROM OLD."display_name" THEN
    NEW."display_name" := NEW."full_name";
  ELSIF NEW."display_name" IS DISTINCT FROM OLD."display_name"
    AND NEW."full_name" IS DISTINCT FROM OLD."full_name"
    AND NEW."display_name" IS DISTINCT FROM NEW."full_name" THEN
    RAISE EXCEPTION 'onwardpg column bridge conflict on app.accounts: display_name and full_name differ'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END
$onwardpg$;

CREATE TRIGGER "onwardpg_sync_column_4cff936be08db67c"
BEFORE INSERT OR UPDATE OF "display_name", "full_name"
ON "app"."accounts"
FOR EACH ROW EXECUTE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();

UPDATE "app"."accounts"
SET "full_name" = "display_name"
WHERE "full_name" IS DISTINCT FROM "display_name";
```

Old and new application instances can now insert and update through their own
column name while the deployment rolls. A statement that supplies two
different values fails visibly instead of silently choosing a winner.

After the old application has drained, `contract.sql` catches up once more and
removes the bridge in one transactional batch:

```sql
UPDATE "app"."accounts"
SET "full_name" = "display_name"
WHERE "full_name" IS DISTINCT FROM "display_name";

DROP TRIGGER "onwardpg_sync_column_4cff936be08db67c" ON "app"."accounts";
DROP FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();
ALTER TABLE "app"."accounts" DROP COLUMN "full_name";
ALTER TABLE "app"."accounts"
  RENAME COLUMN "display_name" TO "full_name";
```

The last two statements may look surprising. The temporary new-name column is
dropped and the original column receives the final name. That preserves the
original column identity and PostgreSQL-managed dependencies. The transaction
keeps the newly deployed application from observing a moment without
`full_name`.

This automatic bridge is deliberately bounded. Defaults, identity/generated
values, PostgreSQL 18 named `NOT NULL` constraint identity, partitions,
existing trigger ordering, RLS, or an unproven dependent rewrite produce an
explicit blocker instead of speculative SQL.

### 4. Hand product meaning to the agent—without inventing a DSL

A change such as `timestamp -> date` is not a reversible mechanical cast.
onwardpg asks for one of two honest outcomes:

- edit reviewed expand/contract SQL that makes one application version work on
  both sides of contract; or
- split the feature into two plans because it needs two deployments.

For the first outcome, generated files contain stable edit pockets:

```sql
-- onwardpg:edit begin stmt-sha256-…
-- ONWARDPG TODO: establish the old/new interfaces and synchronization here.
-- onwardpg:edit end stmt-sha256-…
```

The agent replaces only the bytes inside the markers and may add Boolean
product assertions to `verify.sql`:

```sql
-- onwardpg:assert event_dates_backfilled
SELECT count(*) = 0
FROM "app"."customer_events"
WHERE "occurred_on" IS NULL;
```

onwardpg never invents the cast, backfill value, reverse transform, conflict
precedence, or proof that old traffic has stopped. It receipts the exact edited
SQL and proves only that those bytes execute and reach the desired catalog on a
disposable clone.

### 5. Verify both checkpoints

```sh
# Exercise expand and report the expected remaining contract work.
onwardpg verify --through expand

# Exercise the complete bundle and require an empty final diff.
onwardpg verify
```

Verification replays accepted history in onwardpg-created disposable databases.
It never applies SQL to development, staging, or production. It cannot prove
production data volume, lock tolerance, traffic drain, deployment timing, or
business correctness; the developer and deployment system own those facts.

## What the bundle looks like

```text
migrations/onward/app/customer-profile/
├── manifest.json       # source fingerprints and exact file digests
├── decisions.json      # accepted rename/drop/strategy intent
├── plan.json           # generated operations, batches, hazards, and timeouts
├── verify.sql          # optional developer-owned Boolean assertions
└── phases/
    ├── expand.sql      # before the one application deployment
    └── contract.sql    # after pre-deployment code and writers have drained
```

Empty phase files are omitted. Transactional and non-transactional batches are
marked inside either file; file boundaries are deployment timing, not
`BEGIN`/`COMMIT` boundaries.

## One evolving migration per feature

Run `plan` repeatedly while the feature changes. onwardpg replaces the same
unexecuted logical bundle rather than stacking every intermediate schema idea.

When a teammate lands a migration underneath your branch, your Git-aware
developer or coding agent fetches and rebases. onwardpg itself does not inspect
or mutate Git. The next `plan` sees the history now present in the checkout and
restacks the feature:

```text
before:  A ──▶ feature
after:   A ──▶ teammate-change ──▶ regenerated feature
```

Fingerprint-bound decisions survive when their relevant objects are unchanged.
SQL inside stable edit pockets is transplanted into newly generated surrounding
SQL. If the agent edited generator-owned bytes and the generator also changed
that phase, onwardpg stops with the old generated SQL, current SQL, and new
generated SQL. It never guesses the merge.

The hash-chained history rejects forks, stale parents, changed entries, missing
entries, and ambiguous ordering. The intent is “one migration per squash-merged
feature,” without coupling the planner to branch names or a hosting service.

## Development databases and drift

The durable feature bundle is always the diff from replayed accepted history to
authoritative exported DDL. A long-lived development database is different: it
may contain testing data, migrations from other branches, or cleanup that has
not happened yet.

`plan --output sql` also reports the read-only development-catalog-to-DDL
reconciliation. Workspace mode preserves surplus development objects so merely
switching branches does not suggest drops. The developer or agent decides what
is appropriate to apply locally; onwardpg never does it automatically.

If you already applied an unmerged column and then rename it in code, the two
comparisons deliberately diverge. The durable H → W bundle collapses the
abandoned name and adds only the final column. D → W asks whether the applied
development column was renamed; after an explicit `--dev-hint`, its local SQL
is one direct `ALTER TABLE … RENAME COLUMN`. Choosing `preserve` instead keeps
the old development column. No intermediate name leaks into the production
plan, and no local column is renamed by guesswork.

Production is not consulted for every PR. An occasional read-only audit can
surface drift that accumulated outside the accepted chain:

```sh
onwardpg drift check --database "$PRODUCTION_READ_ONLY_URL"
```

## Ownership and limits

| Owner | Responsibility |
| --- | --- |
| onwardpg | Typed catalog diff, ambiguity questions, generated compatibility SQL, deterministic bundle regeneration, receipts, and disposable-clone convergence |
| Developer or coding agent | Product intent, application compatibility, edit-pocket SQL, assertions, and review of every statement |
| Deployment tooling and operator | Executing files, tracking environments, draining traffic and workers, observing locks/backfills, and deciding rollback |

onwardpg is an MIT-licensed developer preview for PostgreSQL 15–18. It models a
broad set of PostgreSQL objects, but it does not claim an online strategy for
every mutation. The test-linked [feature matrix](docs/supported-features.md)
lists supported transitions and honest gaps; the [phase
classification](docs/phase-classification.md) records why generated work runs
before or after the one deployment. Unsupported catalog state remains
visible; it is never silently treated as equality.

The project intentionally has no production apply command, embedded agent,
framework adapter, plugin system, down-migration generator, or deployment
orchestrator.

## Commands

```text
onwardpg init             establish the replayable history floor
onwardpg plan [name]      create, revise, or restack one active feature bundle
onwardpg status           inspect the active bundle and history relationship
onwardpg verify           clone-verify exact bundle files
onwardpg drift check      compare accepted history with a live catalog read-only
```

In CI, name the bundle explicitly and use read-only check mode to reject stale
DDL, altered history, unanswered decisions, TODOs, unreceipted edits, failed
assertions, or residual schema differences:

```sh
onwardpg verify --bundle customer-profile --check
```

See the [CLI reference](docs/cli.md), [migration
workflow](docs/migration-workflow.md), [bundle format](docs/bundles.md), and
[safety model](docs/safety-model.md) for the detailed contracts.
