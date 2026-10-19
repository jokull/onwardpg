# How onwardpg compares

This document places onwardpg beside Drizzle Kit, Atlas, and pgroll, with a
shorter note on Django, Alembic, Prisma Migrate, and the plain diff planners. It
compares operating models and boundaries, not feature counts. Claims about other
tools were checked against their current documentation on 2026-10-19; the
links sit next to each claim. The pinned Migra, pgmig, and Stripe evidence has
its own documents and dates.

The short version: a schema diff tells you the destination. A rolling
deployment needs a compatibility route, and a feature branch needs that route
to stay reviewable while both its desired schema and the accepted base move.
onwardpg plans that route as one evolving, evidence-bound bundle. It does not
run it.

Drizzle Kit formalizes a TypeScript schema and migration workflow. Atlas
formalizes declarative and versioned schema management, safety analysis, and
application workflows. pgroll formalizes an in-database compatibility runtime.
onwardpg formalizes the planning and evidence lifecycle around one forward
expand/contract migration and leaves production execution to the delivery
system that can observe rollout and drain.

## A diff is the destination, not the route

Every tool here can tell you that `bookings.status` should become `NOT NULL`.
That is the destination. During a rolling deployment the old application keeps
inserting rows without a status until it drains, so an immediate
`ADD COLUMN ... NOT NULL` is correct for the final schema and wrong for the
deployment. The route is: add the column nullable, deploy an application that
writes it and tolerates nulls, drain the old writers, fill the old rows, check
that none are null, then enforce the constraint.

onwardpg's unit of work is that route. The expand phase runs before the deploy,
the contract phase after the drain, and the decisions in between (how old rows
get a status, whether a text-to-integer cast may fail, whether two columns are
one renamed column) are recorded with the bundle. The second problem the bundle
solves is that both ends move while the branch is open: the desired schema
changes as the feature develops, and the accepted base changes as other
branches merge. See [restacking](https://onwardpg.solberg.is/#restacking)
for what carries forward.

## Four schema roles

onwardpg keeps four schema states apart, and the comparison with other tools
mostly comes down to which of them each tool treats as authoritative.

| Role | What it is | Authority | Use |
| --- | --- | --- | --- |
| H, accepted history | The accepted bundles replayed in a disposable PostgreSQL database | Durable migration baseline | Source of the H → W migration |
| W, desired schema | Deterministic exported DDL materialized in a disposable database and read as a catalog graph | The final schema the feature declares | Destination of H → W and D → W |
| D, development database | Long-lived, branch-worn local state | Convenience only | Direct local reconciliation to W; never a production baseline |
| P, production evidence | A read-only observation of production or a replica | External evidence | Drift and contract-readiness checks; never a planner target |

```text
accepted history ── replay in disposable PostgreSQL ── H ─┐
                                                          ├─ decisions ── expand.sql / contract.sql ── verify
desired DDL ─────── materialize in disposable PostgreSQL ─ W ─┘

developer database D ──────────────────────────────────────── direct D → W reconciliation only
production P ───────────────────────────────────────────────── read-only drift / readiness evidence
```

H → W is the durable migration. D → W is optional local convenience. P is
evidence about a real environment, never authority for the planner to change
it.

The local database matters because it is where the branch-worn state lives.
An unmerged `quote_mode` column applied to D, then renamed to `pricing_mode`
in the feature before anyone accepts it, is an addition in H → W because H
never contained either column. D may need a dev-only rename, which `plan`
asks about with `--dev-hint`; that answer never authorizes the durable bundle.
Production evidence is read in two places. `drift check` compares the observed
catalog with the replayed accepted history, the declared baseline. `contract
check` compares it with the receipted post-expand checkpoint and runs the
generated data gates against the writer evidence your release system supplies.
Neither gives onwardpg write access.

## What onwardpg can and cannot prove

`verify` replays the bundle in disposable databases, runs its assertions, and
checks that the contracted schema equals W with no remaining diff. That is
strong evidence about the SQL and the final catalog. It is not evidence about
production: data validity, drain, lock duration, replica or WAL capacity,
and rollback readiness are outside the clone. A text-to-integer conversion is
the clear case. The catalog cannot say whether blanks, signs, or decimals
exist; a read-only query against a replica can classify the values, your
reviewed conversion SQL captures the policy, and the generated contract gate
tests the live precondition before enforcement.

## Drizzle Kit: owns the model, can supply W

Drizzle Kit is a schema-driven CLI. `generate` writes SQL migrations from the
Drizzle schema, `migrate` applies them, `push` applies schema changes directly
and, unless `--force` auto-accepts them, confirms data-loss statements first,
and `export` prints the schema as SQL DDL, which the documentation describes as
a way to let "external tools like Atlas" handle migrations
([kit overview](https://orm.drizzle.team/docs/kit-overview),
[export](https://orm.drizzle.team/docs/drizzle-kit-export),
[push](https://orm.drizzle.team/docs/drizzle-kit-push)). The v1 migration
format groups each migration's SQL and snapshot in its own folder, drops
`journal.json`, derives branch structure from snapshot `prevIds`, and has
`check` detect non-commutative migrations across branches and `generate` flag
incompatible branch changes
([v0 to v1 changes](https://orm.drizzle.team/docs/v0-v1-changes)). Drizzle
has a branch story; do not read this document as saying otherwise.

A Drizzle schema is a good source for W. `drizzle-kit export` produces the
DDL, onwardpg materializes it in PostgreSQL and uses it as the desired
catalog, and Drizzle stays the owner of the application-facing model. The
[Drizzle example](../examples/frameworks) shows the export wrapper. One
integration requirement: `export` emits what is reachable from the configured
schema paths, so functions, triggers, extensions, policies, or grants owned
outside Drizzle need a deterministic wrapper if they belong in W.

The difference in operating model is narrow and specific. Drizzle's migration
is a generated file that you may edit; onwardpg's is one unaccepted H → W
bundle that is regenerated as the base and the feature move, keeping the
decisions and SQL pockets whose scope still holds and asking again for the
rest. Drizzle treats the development database as the thing `push` brings up to
date; onwardpg treats it as D, reconciled separately and never promoted to
history.

Choose one executor for a production DDL change. `drizzle-kit migrate`,
`drizzle-kit push`, and the onwardpg phase files must not independently own
the same change.

## Atlas: a schema-management control plane

Atlas is the broadest adjacent tool. Declarative apply re-inspects the target
on every run and plans a transition from the current state to the desired one,
or executes an approved pre-planned transition from `atlas schema plan` when
one matches the starting state
([declarative apply](https://atlasgo.io/declarative/apply)). The versioned
workflow moves planning to development and CI: `atlas migrate lint` analyzes
migration files for destructive operations, breaking changes, and locks or
rewrites, simulates them in a dev database, and selects the files to analyze
by registry state, `--latest N`, or `--git-base`
([lint](https://atlasgo.io/versioned/lint)). Pre-apply drift detection
compares the expected state at the latest applied revision with the inspected
target ([drift detection](https://atlasgo.io/versioned/drift-detection)).
Both lint and drift detection are documented as Atlas Pro features; lint since
v0.38. Atlas handles several database engines.

If your team wants Atlas to own that control plane, with diffing, policy,
registry, drift, and application in one workflow, Atlas alone is likely the
better fit, and that is true whether or not onwardpg exists.

The narrow point onwardpg adds: a lint finding can identify a destructive,
locking, or backward-incompatible operation. It does not by itself establish
the old/new application contract, the semantics of a cast or backfill, the
evidence required before contract, or how one unmerged compatibility plan
retains and invalidates those decisions as its base changes. onwardpg makes
those artifacts first-class: typed decisions bound to the schema scope of their
question, edit pockets that survive regeneration, explicit expand and contract
phases, and a contract gate fed by writer evidence. The distinction is not
rebase versus no rebase; Atlas documents rebasing a migration directory and
ships an auto-rebase CI action
([CI/CD setup](https://atlasgo.io/versioned/setup-cicd)). It is one
regenerated logical bundle with fingerprint-bound decisions.

Atlas can be the chosen runner for the SQL onwardpg emits, or a policy and
reference tool beside it. The one rule is that each production DDL operation
has exactly one executor.

## pgroll: a compatibility runtime in the database

pgroll is the closest expand/contract peer and a different approach. A
migration is a list of operations such as `add_column` or `alter_column`.
For a data-changing transition the author supplies the part DDL cannot
express, an `up` and a `down` expression:

```yaml
up: trunc(salary)
down: salary
```

pgroll then does the rest at runtime. It creates a new physical column,
backfills it with the `up` expression, installs triggers that propagate writes
between the old and new representation, and exposes the old and new schema
versions at the same time as views in versioned schemas. A client picks its
version, for example with `SET search_path TO 'public_initial_migration'`.
`complete` removes the old version once no client uses it; `rollback` removes
the new one while the migration is active. An existing database is imported
with `baseline`. A `sql` operation runs arbitrary SQL, and the documentation
says pgroll cannot guarantee such a migration is safe
([how pgroll works](https://github.com/xataio/pgroll#how-pgroll-works),
[up and down expressions](https://github.com/xataio/pgroll/blob/main/docs/guides/updown.mdx),
[raw SQL](https://github.com/xataio/pgroll/blob/main/docs/operations/raw_sql.mdx)).

That is a strong fit when you want the database to route clients between the
two interfaces and to own rollback of an active migration. Application code
does not normally dual-read or dual-write; compatibility moves into database
interface routing. The costs are the ones of any runtime layer: migration
state in the database, a version-aware connection contract for each
application version, and views, triggers, and write amplification while a
migration is active.

onwardpg works with one ordinary database interface and makes the overlap
application-visible. It plans the compatible physical shape, requires the
newly deployed application to understand it until old code drains, and emits
reviewed expand and contract SQL for the existing deployment system to run.
In return it needs no version-schema routing and takes no execution
authority. Where pgroll compiles a semantic operation into a live two-version
interface, onwardpg compiles two catalog states and your decisions into a
deployment plan. Pick by which authority you want in the database.

## Framework migration systems

Django, Alembic, and Prisma Migrate each own a model, a migration history, and
a runner. Django compares models against the state it rebuilds from the
migration files, not the database, and uses `RunPython` for data migrations
([Django migrations](https://docs.djangoproject.com/en/6.0/topics/migrations/)).
Alembic autogenerates a candidate revision that its documentation says it is
"always necessary to manually review and correct", renders table and column
renames as add/drop pairs, and merges revision heads in its DAG
([Alembic autogenerate](https://alembic.sqlalchemy.org/en/latest/autogenerate.html)).
Prisma Migrate records a migration history and splits development from
deployment; its current documentation describes a changed command set
(`prisma db migrate` replaces Prisma 7's `migrate deploy`), so the
[Prisma recipe](../examples/frameworks/prisma) in this repository is the
reference for the concrete commands
([Prisma workflows](https://www.prisma.io/docs/orm/prisma-migrate/workflows/development-and-production)).
These are mature systems, and for applications where one of them is
sufficient, it should stay the migration system.

onwardpg's relation to each is the same as to Drizzle: the framework stays the
source of W through a deterministic export (the
[framework recipes](../examples/frameworks) cover Drizzle, Prisma, and Django),
onwardpg plans H → W around the deployment, and one system executes the
production DDL. onwardpg does not synthesize a down migration; before
contract, the preserved old interface is the rollback path, and after it,
recovery is a new forward plan.

## Diff planners

[Migra](https://github.com/djrobstep/migra), [pgmig](https://github.com/Apakottur/pgmig),
and [Stripe pg-schema-diff](https://github.com/stripe/pg-schema-diff) answer
the catalog-to-catalog question onwardpg's planner core also answers, and
Migra is where the idea came from. They emit an ordered statement list for the
destination and leave phasing, intent, and history to the caller. Stripe's
project, at its pinned v1.0.7, has broader online-DDL coverage for several
hard operations.
The object-family evidence lives in the [Migra compatibility guide](compatibility.md),
the [Stripe gap inventory](stripe-gap-inventory.md), and the
[pgmig parity plan](pgmig-parity-plan.md), each pinned to a commit and dated.

## Choose the operating model

| Situation | First look | Why |
| --- | --- | --- |
| TypeScript model-driven schema and ordinary generated migrations | Drizzle Kit | Native application-schema workflow; `export` can supply W to other tools |
| One control plane for diff, lint, registry, drift, and apply, possibly across engines | Atlas | Declarative and versioned management with policy and CI integration |
| Old and new interfaces served by the database, with rollback of an active migration | pgroll | Versioned-schema compatibility runtime |
| Django, SQLAlchemy, or Prisma application where the framework's migrations and runner are enough | That framework | Mature model-driven history and data-migration support |
| A PostgreSQL compatibility plan that must survive feature edits, rebases, and local drift, with product decisions on record, while deploy stays external | onwardpg | Explicit H/W/D/P roles and one evolving H → W bundle |

The table is guidance, not exclusivity. Drizzle or a framework can supply W,
and Atlas can remain a policy or reference tool. The hard rule is single
ownership of each production DDL operation.

## Where onwardpg stands

onwardpg is a PostgreSQL migration planner, not a production migration runner.
It replays accepted history, materializes the desired DDL in PostgreSQL,
compares typed catalog graphs, and asks for explicit intent where the schema
cannot prove it: renames, destructive transitions, casts, backfills, and
timing. It renders expand and contract phases, keeps valid decisions and
developer-owned SQL across restacks, and clone-verifies convergence.

The production boundary is deliberate. The deployment system executes the
reviewed SQL because it is the system that can observe rollout, queues,
workers, stale clients, and drain. A green clone is structural evidence; it is
not a production safety oracle.

It is also a preview. The modeled catalog surface has gaps, listed in
[supported features](supported-features.md), and the planner blocks on
unsupported state instead of guessing. Stripe pg-schema-diff has a larger
acceptance corpus and broader online-DDL coverage for several operations at
the pinned v1.0.7. The
frameworks above have years of operational use. onwardpg's claim is narrower:
inside its modeled boundary it produces a deterministic, reviewable,
forward-only handoff and keeps your decisions attached to it as the schema
moves.
