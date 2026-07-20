---
title: Comparison
description: See why onwardpg plans one compatibility-safe PostgreSQL migration around one merge and one rolling application deployment.
---

_Reviewed 20 July 2026 against the primary documentation linked on this page._

Most migration tools answer one question: **how do I move this database to the
schema my code declares?**

onwardpg answers the production question hiding behind it: **how can the code
running now and the code about to deploy both use the database while that move
happens?**

That changes the release sequence:

```text
change the schema in your feature branch
        ↓
onwardpg works out how old and new code can share the database
        ↓
APPLY EXPAND — before the pull request merges
        ↓
merge and deploy one application version
        ↓
wait until old instances and workers are gone
        ↓
APPLY CONTRACT — only when there is cleanup to perform
```

Applying SQL before merge sounds aggressive until you see what expand means.
It is designed not to break the code already running. For a breaking change,
expand temporarily makes the database **more permissive than both the old and
new schema**. Old code keeps working, new code gets what it needs, and both can
run during the rollout. You still review locks, data volume, and database
capacity; compatible does not mean free.

For a purely additive change there may be no compatibility problem and no
contract at all. onwardpg emits only the non-empty phase, so the common case is
still boring.

## Why this almost never breaks locally

A local migration often looks atomic: stop one process, change the schema,
start the new process. The database may be disposable, nearly empty, or rebuilt
from scratch. There are no old containers, long-lived connection pools, queue
workers, scheduled jobs, rollback instances, or forgotten preview deployments
continuing to use the old schema.

Production has all of those. The schema and application do not change at the
same instant.

A normal new column is exactly as boring as you expect. If `status` is nullable,
onwardpg adds it in expand and there is nothing to contract. Old code does not
know the column exists, and that is fine.

A required column with a database default is also compatible in one step. Old
inserts omit `status`; PostgreSQL supplies the default.

The interesting case is a required column **without** a database default—say
the application computes a booking status that cannot honestly be invented by
the database:

```sql
status text NOT NULL
```

Production is still running the old version, and that version inserts bookings
without `status`. PostgreSQL treats the omitted value as `NULL`; add `NOT NULL`
immediately and those requests start failing.

So onwardpg adds the column without `NOT NULL` before the PR merges:

```sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;
```

The new application deploys knowing how to write `status` and tolerates it being
temporarily nullable while the two releases overlap. Old code still creates
`NULL` rows until it drains. At this point onwardpg has **not** chosen `pending`:
it knows what old code writes, but it cannot know what those rows mean to the
product.

The next `plan` says exactly what is missing. It offers three choices:

- `assert_only` if application code or an earlier operation will fill every row;
- `manual_sql` if a developer or agent can supply the rule now; or
- `split_plan` if the honest answer is “not in this release.”

Choosing `manual_sql` does not smuggle a guessed value into the plan. The CLI's
copyable choice is equivalent to:

```sh
onwardpg plan add-booking-status \
  --hint '{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}' \
  --hint '{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}'
```

onwardpg then stops with `needs_sql_edits` and names
`phases/contract.sql`. Inside that file is a marked pocket asking for the
post-drain rule, followed by an onwardpg-owned check that no `NULL` values
remain. For this example, the developer decides that old bookings mean
`pending` and replaces only the pocket:

```sql
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
```

That rule could just as easily be a `CASE`, a join to another table, or a
separately run batched operation. An agent can also attach the reviewed SQL and
its Boolean postcondition as structured `work` on the `manual_sql` hint. Either
way, `onwardpg verify` runs the exact edited plan on disposable PostgreSQL; an
unfilled pocket cannot pass.

Before turning `NOT NULL` on, onwardpg checks that no rows are still missing a
status. Then it applies the final change:

```sql
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
```

A rename makes the stakes clearer. Rename the column first and old code fails.
Deploy code that expects the new name first and new code fails. Without a bridge,
there is no ordering that avoids an application error window—it is literal
downtime dressed up as a one-line `ALTER TABLE`.

onwardpg keeps both names working and their values in sync. After one application
deploy and drain, it removes the old name. The same idea applies to changed
checks, required columns, type conversions, and dependent views: the temporary
schema exists to get the release out safely, not as accidental debt.

## Where the other tools stop

| Kind of tool | What it does well | What it leaves to you |
| --- | --- | --- |
| **ORM sidecars** — Drizzle, Django, Alembic, Prisma | Turn application models into SQL and track migrations. | Work out how old and new application versions will share the database. |
| **Diff engines** — Atlas, Stripe `pg-schema-diff`, migra | Calculate the SQL needed to move from one PostgreSQL schema to another. | Account for what the code currently running can still read and write. |
| **Compatibility runtime** — pgroll | Keep old and new database interfaces running side by side. | Describe what the change means and make each app version select the right interface. |

## What onwardpg plans—and when it asks for help

onwardpg already knows how to handle many changes that routinely bite teams in
production. It writes the expand/contract SQL and pauses only when it needs a
business decision or data cleanup that the schema cannot explain:

- **Required columns** arrive nullable so old inserts keep working. Cleanup, a
  query that checks every row, and `SET NOT NULL` wait until contract.
- **Check constraints** are widened immediately when the new rule accepts more
  values, retained until contract when the new rule is narrower, or temporarily
  relaxed when neither rule contains the other.
- **Defaults** are added before a new writer starts omitting a value and removed
  only after the old default-dependent writer drains.
- **Unique, primary-key, foreign-key, and exclusion changes** loosen the old
  rule while both app versions are live, then check the data and install the new
  rule after drain.
- **Column and table renames** become overlap bridges after the developer or
  agent confirms identity. For an eligible column rename, onwardpg generates
  the second column, two-way synchronization trigger, catch-up, equality gate,
  cleanup, and final native rename.
- **Indexes and dependent objects** are handled in the order PostgreSQL needs:
  concurrent replacement where supported, coordinated foreign keys, and safe
  removal and recreation of views and materialized views when a dependency
  changes.

PostgreSQL can describe tables, constraints, and dependencies. It cannot tell
onwardpg whether two names mean the same thing, which duplicate row should win,
what a blank string means during a type conversion, whether a backfill must run
in batches, or how your application will read through a more complex change.
When onwardpg reaches one of those questions, it asks instead of guessing.

onwardpg is deliberately designed for a developer or coding agent to supply
that missing work. An agent that has read the feature code can answer those
questions early, run small read-only queries to see what production values look
like, and write a complex backfill or conversion in the clearly marked part of
the SQL file. onwardpg then checks what it can: that the SQL is in the right
phase, dependencies are handled in the right order, transaction boundaries and
hazards are visible, live checks run before contract, and the resulting schema
is the one the application asked for.

This is not a free-form escape hatch. Unresolved TODOs fail verification. If the
schema changes underneath a decision, onwardpg asks again. The exact edited SQL
must run on a disposable PostgreSQL database and leave no schema difference
behind. onwardpg cannot decide whether a business rule is correct for your
product, but custom SQL cannot quietly bypass the plan.

## ORM tools: they tell onwardpg what the app needs

[Drizzle Kit](https://orm.drizzle.team/docs/kit-overview),
[Django migrations](https://docs.djangoproject.com/en/6.0/topics/migrations/),
[Alembic](https://alembic.sqlalchemy.org/en/latest/autogenerate.html), and
[Prisma Migrate](https://www.prisma.io/docs/orm/prisma-migrate/workflows/development-and-production)
should continue to own the application schema. onwardpg is not trying to replace
them. Their SQL output tells onwardpg what the database should look like when
the feature is finished.

For example, [`drizzle-kit export`](https://orm.drizzle.team/docs/drizzle-kit-export)
prints the SQL for the tables and constraints in your TypeScript schema. Django
can build the database described by its migration state; Alembic can replay its
selected [revision graph](https://alembic.sqlalchemy.org/en/latest/branches.html);
Prisma can produce SQL from its schema. onwardpg plans the rollout to that
result while the framework remains the source of truth.

### Why not stop there?

Framework migration generators see the schema you want. They do not see the old
binary still writing `NULL`, the worker using the old column name, or the product
rule for malformed values. Their normal unit is one migration attached to one
application release.

The conventional safe workaround is therefore two feature cycles:

```text
PR 1 / deploy 1: add permissive shape and deploy bridge code
PR 2 / deploy 2: tighten or remove the old shape after drain
```

Drizzle supports [custom SQL migrations](https://orm.drizzle.team/docs/kit-custom-migrations),
and the other frameworks have comparable escape hatches. You can hand-author
the steps yourself, but the tool does not preserve them as one evolving plan tied
to one release.

With onwardpg, the application schema can say what it really wants from the
start. onwardpg changes the database in phases:

```text
one branch, one merge, and one application deploy
  before merge: expand.sql
  after drain:  contract.sql, if non-empty
```

As the feature changes or rebases, `onwardpg plan` updates the same migration.
It keeps decisions and edited SQL only while the schema still supports them.

## Diff engines: excellent SQL, but they cannot see the rollout

These tools are closest to the engine inside onwardpg:

- [Atlas `schema diff`](https://atlasgo.io/declarative/diff) accepts database,
  SQL, HCL, or migration-directory states and generates a SQL plan.
- [Stripe `pg-schema-diff`](https://github.com/stripe/pg-schema-diff) uses
  PostgreSQL-native online techniques, validates plans in a temporary database,
  and reports hazards.
- [migra](https://github.com/djrobstep/migra) established the elegant “database
  A to database B” CLI model. It is now officially deprecated.

Atlas also sells Cloud/Pro database-management, registry, policy,
[lint](https://atlasgo.io/versioned/lint), drift, and apply tooling. That is a
broader control plane; the relevant comparison here is its very capable diff
CLI.

### Why not stop at the diff?

These tools write **SQL that moves the database from A to B**, not a release
plan that keeps two application versions working. They can make a constraint
lock-friendlier, build an index concurrently, or warn about a drop. They cannot
tell that the old application still depends on behavior the new schema removes.

Online DDL is not application compatibility. A rename can take a tiny lock and
still produce instant downtime. A hazard warning can tell you an operation is
dangerous; it cannot invent the product-specific bridge, decide what data means,
or know when old writers have drained.

onwardpg uses the diff as the beginning. It works out what can happen before
deploy, what must wait until old code is gone, which questions need an answer,
and whether the exact reviewed SQL reaches the requested schema.

## pgroll: real compatibility, heavier machinery

[pgroll](https://github.com/xataio/pgroll#how-pgroll-works) is the only adjacent
tool here that genuinely keeps old and new database interfaces available at the
same time. You describe the change in YAML or JSON and provide
[up/down mappings](https://github.com/xataio/pgroll/blob/main/docs/guides/updown.mdx)
for how values move in both directions. pgroll builds versioned schemas, views,
shadow columns, backfills, synchronization triggers, completion, and active
rollback. That is impressive engineering.

### Why choose a planner instead?

pgroll solves compatibility by adding a runtime layer to the database.
Application versions must select their matching schema, commonly through
`search_path`, as its
[client integration guide](https://github.com/xataio/pgroll/blob/main/docs/guides/clientapps.mdx)
shows. Your application configuration, pgroll's versioned views and triggers,
and the rollout now have to move together.

You still have to tell pgroll what the change means. Its
[`convert` workflow](https://github.com/xataio/pgroll/blob/main/docs/guides/orms.mdx)
translates existing ORM SQL into pgroll operations, but its documentation says
developers must still supply `up`/`down` mappings and sometimes consolidate the
result manually.

pgroll is a strong choice when you want the database to route old and new
clients and provide active rollback. onwardpg is a strong choice when you want
the application to keep using its normal PostgreSQL schema, with reviewed SQL
before and after one application rollout.

## The onwardpg advantage

The pitch is not “we have a better diff.” It is the release model around it:

- **Compatibility lands before code.** Expand can be applied before merge
  because current production code remains valid against it.
- **One feature stays one migration.** Model edits, decisions, SQL, rebase, and
  verification evolve in one bundle instead of accumulating draft fixups.
- **One merge gets one deployment.** Contract follows drain as post-SQL; an
  empty contract disappears instead of demanding a ceremonial second release.
- **Keep using your framework schema.** Drizzle, Django, Alembic, or Prisma
  still says what the finished database should look like.
- **Test the actual SQL on PostgreSQL.** onwardpg runs the reviewed files,
  follows real dependencies, and checks that the final schema matches.

Running the plan is intentionally boring. Your release system applies reviewed
expand SQL before merge/deploy and contract SQL after drain. onwardpg does not
need production write access, and your deployment scripts do not have to work
out the safe order themselves.

:::caution[One production operation, one DDL executor]
Do not let a framework migration runner, `drizzle-kit push`, Atlas apply, and
onwardpg phase files independently own the same schema operation. Choose one
production execution path.
:::

onwardpg still cannot prove production capacity, acceptable lock duration,
application correctness, or that every old process drained. It makes those
boundaries explicit—and generates the schema bridge that makes waiting for the
evidence possible without taking the application down.
