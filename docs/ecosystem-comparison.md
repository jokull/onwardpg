# Ecosystem comparison

This document compares onwardpg with tools that answer several different
questions commonly hidden behind the phrase “database migrations.” It is a
research snapshot, not a compatibility certification. It was checked on
2026-07-13 against project documentation, source, and tests.

The short version is:

- [Migra](https://github.com/djrobstep/migra) established the direct
  PostgreSQL state-diff workflow that inspired onwardpg.
- [pgmig](https://github.com/Apakottur/pgmig) is a compact, actively developed
  Python project in the same state-diff tradition.
- [Stripe pg-schema-diff](https://github.com/stripe/pg-schema-diff) is the
  closest peer to onwardpg's Go planner core and is currently stronger at
  several difficult online PostgreSQL operations.
- [Alembic](https://alembic.sqlalchemy.org/), [Django migrations](https://docs.djangoproject.com/en/6.0/topics/migrations/),
  and [Drizzle Kit](https://orm.drizzle.team/docs/kit-overview) are mature
  framework migration systems. Their model or migration history is part of
  the contract; they are not general catalog-to-catalog PostgreSQL planners.
- onwardpg's distinct bet is the developer-and-agent decision lifecycle:
  PostgreSQL-materialized desired state, explicit intent, phased forward
  plans, no apply command, clone receipts, and one restackable logical bundle
  per pull request.

## Compare the right categories

The tools fall into three overlapping categories:

| Category | Primary question | Examples |
| --- | --- | --- |
| State-diff planner | “What SQL gets this PostgreSQL schema to that state?” | onwardpg, Migra, pgmig, Stripe pg-schema-diff, Atlas, psqldef |
| Framework migration system | “What revision or direct sync follows from this framework schema state?” | Alembic, Django, Drizzle Kit, Prisma Migrate |
| Runner / rollout engine | “Which recorded migration should execute next?” | Flyway, Liquibase, Goose, Sqitch; pgroll also manages an expand/contract runtime |

A runner can execute perfect handwritten SQL and still have no schema-diff
algorithm. An ORM migration system can be excellent for its own model surface
without representing every PostgreSQL catalog object. A state-diff planner can
produce excellent SQL while having no deployment history. The distinctions
matter more than a single “supports migrations” checkbox.

## Philosophy and workflow

| Tool | Authority for desired state | Main artifact | Applies to a caller database? | Rename policy | History / PR restacking |
| --- | --- | --- | --- | --- | --- |
| **onwardpg preview** | Live PostgreSQL or exported DDL materialized by PostgreSQL | Semantic hints, phased SQL, hazards, and fingerprinted bundle receipts | **Never** | Credible candidate becomes an explicit semantic choice; evidence is bound internally | Hash-chained bundles; one logical bundle can be regenerated from a newer base |
| **Migra** | Two PostgreSQL schemas, or `EMPTY` | Ordered SQL | CLI no; Python API can | Name-keyed diff, generally drop/add or rebuild | None |
| **pgmig** | Two live PostgreSQL databases | Ordered SQL | No | No table/column rename; some structurally equal objects are paired automatically | None |
| **Stripe pg-schema-diff** | PostgreSQL database or directory of DDL | Flat ordered statements with timeouts and hazards | CLI can; library leaves it to caller | Names are identity, so rename is drop/add | None |
| **Alembic** | SQLAlchemy `MetaData`; live database is the current side | Editable Python revision | Yes, through Alembic | Table/column renames are not detected as such by autogenerate | Revision DAG; merge revisions and branch labels |
| **Django** | Django model state plus Django migration graph | Editable Python migration | Yes, through Django | Interactive autodetector can ask about credible model/field renames | Per-app dependency graph, merge, update, optimize, and squash tooling |
| **Drizzle Kit** | Drizzle TypeScript schema plus snapshots/journal | SQL migration and metadata | `migrate`/`push` can | `generate` prompts for supported rename ambiguities | Drizzle migration journal; no onwardpg-style PR/base restacking |

onwardpg makes “the planner must never deploy” a permanent product boundary.
Migra's CLI and pgmig also support generation-only workflows, but Migra's
Python API can apply and neither project couples that boundary to phased PR
handoff. onwardpg's disposable databases exist to materialize DDL and prove
plans, not as an indirect application path.

## Design thesis

The workflow differences above are not neutral defaults; they follow from a
small set of load-bearing bets. Each carries a cost, and each is a position
other reasonable tools decline to take.

**1. Expand/contract is eventually unavoidable, so it should be the default
unit rather than something you remember to assemble.** An accurate final-state
diff is still eventually an operationally wrong migration: the first rename,
required column, or type cutover that must survive overlapping application
versions turns a single-shot `ALTER` into a broken deploy. Every pure
state-diff planner—Migra, pgmig, Stripe pg-schema-diff—hands over the endpoint
and leaves phasing to the caller; the ORMs offer partial online helpers
(Django's concurrent-index and `NOT VALID` operations) but no
application-spanning expand/migrate/contract sequence. onwardpg makes the
phased window the primary artifact. The cost is scope honesty: it automates
only a subset of transitions today and blocks the rest instead of labeling a
breaking cutover “safe.”

**2. Branch conflicts are inevitable and must be handled, not wished away.** On
any team two branches will fork the same base and each add schema. Django and
Alembic treat this as first-class and *join* the heads with a merge
revision—which asserts the branches are compatible without proving it,
deferring discovery of a real conflict to apply time. Prisma and Drizzle Kit
resolve by regenerating. Neither camp pretends history stays linear-clean, and
a tool with no answer here is unfinished.

**3. Regenerating from scratch is lossy, so regeneration must preserve human
work.** The regenerate camp's failure mode is that “drop the migration and run
generate again”—or Prisma's shadow-database reset—discards hand-authored data
migrations and assertions along with the stale diff. onwardpg regenerates the
*bundle* against the new base while preserving agent-owned `migrate.sql` and
`verify.sql` and still-valid decisions, invalidating only what actually
changed. Lossless restacking is the whole point; a regeneration that throws
away the backfill someone wrote is a step behind even a merge node.

**4. Automatic down migrations are not worth generating.** A synthesized
`downgrade` is a plausible-looking artifact that cannot restore dropped data,
cannot un-run a backfill, and was never proven against anything—so it invites
trust it has not earned. Alembic and Django carry downgrade paths; onwardpg
declines to fabricate one.

**5. A rollback is just another stacked plan.** Before contract runs, the
preserved old interface *is* the rollback path—nothing destructive has
happened yet, so no reverse migration is required. After contract, recovery is
a new forward plan, re-derived from the desired DDL and clone-proven like any
other, authored by the operator who holds the traffic and data context. It is
the same machinery pointed forward, not a special reverse mode.

**6. A migration kit cannot—and should not—enforce linearly applied history,
because reality will not cooperate and the resulting drift should be visible
rather than assumed away.** onwardpg's *planning* chain is deliberately linear
and blocks on ambiguity; that is a provable, in-repository baseline. The
*environment* is a different object, and this is where the framework kits
quietly overreach. Their applied-migration ledger assumes each environment
advances cleanly, while in practice contracts get deferred across releases,
cleanup gets skipped, a `DROP` gets postponed “for now,” and an environment
sits half-migrated for weeks. The ledger then diverges from the catalog
silently. onwardpg's answer is threefold: it does not own the environment
ledger at all—that belongs to the operator; it keeps the desired schema
re-derivable from real DDL so any environment can be re-anchored; and it makes
divergence a *detectable audit*—`drift check`, comparing replayed accepted
history against the live catalog—rather than a hidden assumption. It reports a
mid-rollout partial state as drift on purpose, because the alternative, quietly
maintaining phase exceptions, is precisely how subtle drift accrues in the
first place.

Taken together, these bets are why onwardpg is forward-only, never applies to a
caller-owned database, keeps one evolving bundle per feature, and refuses to
become a runner or environment journal. They are bets, not proofs; the
honest-gaps section below keeps score of where they have not yet paid for
themselves.

## PostgreSQL planner coverage

This table compares the four PostgreSQL state-diff planners, where an
object-family comparison is meaningful. It intentionally distinguishes
current implementation from roadmap. “Partial” can still cover common cases;
the linked [supported-feature ledger](supported-features.md) describes the
current documented boundary; its catalog inventory is still incomplete.

| PostgreSQL family | onwardpg preview | Migra final release | pgmig current `main` | Stripe pg-schema-diff v1.0.7 |
| --- | --- | --- | --- | --- |
| Schemas / extensions | Create/drop; extension version/schema; comments within modeled boundary | Create/drop/filter; extension version | Create/drop/comments; extension version/schema | Create/drop; extension upgrades |
| Tables / columns | Broad common DDL; typed rename/cast/drop questions | Broad common DDL, inheritance, identity/generated; rename is drop/add | Ordinary tables and basic columns; no table/column rename, identity, generated, collation, or persistence | Broad common DDL, identity/generated/collation; rename is drop/add |
| PK / unique / check / FK | Yes; cycles, `NOT VALID`, validation, staged `NOT NULL` | Yes; good staged constraint fixtures | Yes; several validation/deferrability transitions absent | Yes; mature online constraint and staged `NOT NULL` paths |
| Exclusion constraints | Partial | Yes | No | No |
| Indexes | Structural indexes, concurrent mode, partition propagation, and continuous same-name standalone replacement; advanced attached/constraint/partition-parent cases remain narrower | Ordinary and materialized-view indexes; no concurrent mode | Ordinary indexes and optional concurrent mode; no materialized-view indexes | Strong ordinary, materialized, local, and partitioned coverage; continuous same-name concurrent replacement |
| Sequences | Create/drop/parameters and typed `OWNED BY` transitions; complete identity options with confirmed removal | Create/drop/ownership changes | Create/drop/parameters; no `OWNED BY` transition | Create/drop/parameters/type/cycle/`OWNED BY` |
| Enums | Create/drop/add; ambiguous/destructive changes stop | Create and replacement-oriented changes | Create/drop/add | Create/drop/add |
| Domains | Blocked | No verified diff family | **Create/drop and common alterations** | Not modeled |
| Composite / range types | Blocked | No verified diff family | Composite create/drop on current `main`; range blocked | Not modeled |
| Views | Create/replace/drop and explicit rename within documented dependency proofs | Ordinary views with dependency-aware recreation | Create/drop/rebuild; view-on-view cases blocked | Create/drop and rebuild-style changes |
| Materialized views | Create/drop/rename/rebuild with explicit destructive intent; refresh contracts | Create/drop/rebuild and indexes | Basic create/drop/rebuild on current `main`, always `WITH NO DATA`; indexes blocked | Create/drop/rebuild/options/tablespace/indexes |
| Functions / procedures / triggers | Modeled common lifecycle with typed edges; arbitrary procedural-body dependencies remain unknowable | Functions/triggers; historical coverage | Functions, procedures, triggers; incomplete global ordering | Functions, procedures, triggers; non-SQL dependencies become hazards |
| Partitioning | Create/hierarchy/attach/detach and propagated resources; reconfiguration requires manual contract | Historical create/attach/detach support | Explicitly blocked | Strong creation/attachment and partition/local-index coverage; difficult reconfiguration rejected |
| Ownership / grants / RLS | Typed table grants and RLS/policies with intent questions; ownership deviations, default/column/non-table ACLs, and grant chains block | Optional privileges and RLS/policies; sequence ownership | Table ownership on current `main`; other security blocked | Table privileges and RLS/policies; ownership generally absent |
| Unknown catalog state | Inventoried blockers stop explicitly; preview inventory is not exhaustive | No general unknown-object stop | A few explicit blockers, not a complete unknown-state inventory | Unmodeled families are outside its snapshot rather than a general stop |
| PostgreSQL versions | 15–18 | Historical/deprecated; no current support policy | 14–18 in CI | 14–17 in documented CI |

Reference points:

- Migra was reviewed at [`3450382`](https://github.com/djrobstep/migra/tree/345038271b9fd76296c89e01c7256d0d38e2ce83),
  corresponding to the deprecated final public line.
- pgmig was reviewed at current `main` [`4685ada`](https://github.com/Apakottur/pgmig/tree/4685adaf1636aca2e1303effc1d5f130795708e1).
  Some capabilities above—materialized views, table ownership, and composite
  types—are newer than its published `v0.0.5` release.
- Stripe pg-schema-diff was reviewed at released commit [`6208f8f`](https://github.com/stripe/pg-schema-diff/tree/6208f8f3ceccae8ca634055dc47907a6a864cb76),
  tagged `v1.0.7`.
- Alembic behavior was checked against the 1.18.5 documentation and source
  commit [`c88fa5a`](https://github.com/sqlalchemy/alembic/tree/c88fa5afaf2b9783a58a918f3fc73abc44daa0a9).
- Drizzle Kit source tests were checked at commit [`9d64532`](https://github.com/drizzle-team/drizzle-orm/tree/9d6453215d18705986c2081124437bb6a03fb943);
  its migration format is actively evolving.
- Django behavior is explicitly scoped to the versioned Django 6.0
  documentation linked below.

## Framework migration coverage

The ORM/framework rows need a different interpretation. “Built in” means the
feature can be represented by that framework's normal schema/migration API,
not that it can diff arbitrary PostgreSQL DDL found in a database.

| Capability | Alembic autogenerate | Django migrations | Drizzle Kit |
| --- | --- | --- | --- |
| Tables / columns / nullability | Built in through SQLAlchemy metadata | Built in through Django model state | Built in through Drizzle schema |
| Table / column rename | Not detected; generated as add/drop and edited manually | Autodetector can ask; explicit `RenameModel` / `RenameField` operations | Interactive resolution in supported flows; generated SQL remains reviewable |
| PK / indexes / unique / FK | Basic indexes, named unique, and basic FK detection; PK changes are not a built-in autogenerate promise | Rich model operations; PostgreSQL index APIs include expressions, conditions, include, opclasses, and specialized methods | Common constraints and indexes; PostgreSQL schema surface is broader than older Drizzle releases |
| Checks / exclusion | Check and exclusion changes are not generally built-in autogenerate | Check, unique, and PostgreSQL exclusion constraints are model operations | Checks are supported; advanced PostgreSQL-only forms vary by schema API/version |
| Sequences / free-standing types | Sequences are not built-in autogenerate; third-party extensions cover some PG types | Auto-field sequences are managed; arbitrary standalone catalog types are usually custom SQL | Sequences, identity, enums, and generated columns are supported in current PostgreSQL schema APIs |
| Views / routines / triggers / RLS | Not core autogenerate; projects use `RunSQL`-style operations or extensions such as `alembic-utils` | Not normal model-state objects; use `RunSQL`, custom operations, or packages | Ordinary/materialized views and RLS policies/roles are modeled; routines, triggers, and extension lifecycle generally need custom SQL |
| Data migration | Editable revision code, including arbitrary operations | First-class `RunPython` and `RunSQL`, historical app models, non-atomic migrations | Custom SQL migrations; application code remains external |
| Online PostgreSQL helpers | Operator-authored or extension-specific | Concurrent index operations and `NOT VALID` / later validation for check constraints | Generated SQL and push warnings; no general expand/contract planner |
| Drift / CI | `alembic check` detects new upgrade operations | `makemigrations --check` detects model changes without migrations; `migrate --check` detects unapplied revisions | `check`/`generate`/`push` workflows vary; snapshots and journal are framework state |
| Apply / rollback | Built-in upgrade and downgrade runner | Built-in apply and unapply runner | Built-in migrate/push paths |

Sources: [Alembic autogenerate limitations](https://alembic.sqlalchemy.org/en/latest/autogenerate.html),
[Django migration commands](https://docs.djangoproject.com/en/6.0/ref/django-admin/#makemigrations),
[Django PostgreSQL migration operations](https://docs.djangoproject.com/en/6.0/ref/contrib/postgres/operations/),
[Drizzle generate](https://orm.drizzle.team/docs/drizzle-kit-generate), and
[Drizzle push](https://orm.drizzle.team/docs/drizzle-kit-push).

## Tool-by-tool notes

### Migra: the philosophical ancestor

Migra's enduring insight is that migrations can be derived from PostgreSQL
state rather than remembered as a chain of model edits. Its deployment guide
recommends deriving SQL from the production schema, reviewing and editing it,
testing it, and structurally verifying the result. That is very close to the
workflow behind onwardpg and the [original onwardpg blog
post](https://www.solberg.is/sql-diff-migrations).

Migra is still impressively broad: inheritance, collations, and historical
partition/object transitions exceed parts of onwardpg's current surface. Its
final line also covers identity/generated columns, sequence ownership,
optional grants, and RLS policies, which onwardpg now models through a more
explicit typed/intent-oriented architecture. Migra also has useful
dependency-aware recreation for selectable objects.

The limitations are architectural and maintenance-related:

- the project is deprecated and has had no release since 2022;
- the CLI compares PostgreSQL schemas rather than directly materializing DDL;
- renames are generally name-keyed drop/add or rebuilds;
- `--unsafe` is a coarse rendered-`DROP` gate, not typed intent;
- it has no hazard taxonomy, transaction batching, rollout phases, durable
  answers, clone receipts, or PR restacking; and
- the Python API can apply changes, while onwardpg permanently stops at the
  reviewed handoff.

See the narrower [Migra compatibility guide](compatibility.md) for an
object-by-object view.

Primary evidence: Migra's [README and deprecation
notice](https://github.com/djrobstep/migra/blob/345038271b9fd76296c89e01c7256d0d38e2ce83/README.md),
[deployment workflow](https://github.com/djrobstep/migra/blob/345038271b9fd76296c89e01c7256d0d38e2ce83/docs/deploy_usage.md),
[change ordering](https://github.com/djrobstep/migra/blob/345038271b9fd76296c89e01c7256d0d38e2ce83/migra/changes.py),
and [convergence test](https://github.com/djrobstep/migra/blob/345038271b9fd76296c89e01c7256d0d38e2ce83/tests/test_migra.py).

### pgmig: the compact newcomer

pgmig keeps the interface admirably small: give it two live PostgreSQL DSNs
and receive SQL. Both sources are inspected read-only in repeatable-read
transactions. It has no project configuration, migration history, or apply
command. Its integration tests apply a diff and assert that the next diff is
empty—a strong and relevant correctness habit.

Its current implementation uses catalog-backed Python dataclasses and fixed
global statement phases. Global dependency-topological ordering remains on
the [roadmap](https://github.com/Apakottur/pgmig/issues/8). Destructive changes
are emitted without a typed approval gate. Concurrent indexes are optional,
but the user must honor the warning to run them outside a transaction.

pgmig also proves why the comparison must stay current. Its `main` now covers
three concrete areas onwardpg does not:

- domains;
- standalone composite-type create/drop; and
- table ownership.

onwardpg is ahead on declarative-DDL materialization, typed dependency and
transaction planning, rename/cast/drop/backfill questions, hazards, phased
application handoffs, a broader explicit blocker inventory, clone receipts, and PR
restacking. It should not claim 100% pgmig coverage.

Primary evidence: pgmig's [current
README](https://github.com/Apakottur/pgmig/blob/4685adaf1636aca2e1303effc1d5f130795708e1/README.md),
[snapshot API](https://github.com/Apakottur/pgmig/blob/4685adaf1636aca2e1303effc1d5f130795708e1/src/pgmig/api.py),
[phase engine](https://github.com/Apakottur/pgmig/blob/4685adaf1636aca2e1303effc1d5f130795708e1/src/pgmig/_diff/_core.py),
[unsupported-state query](https://github.com/Apakottur/pgmig/blob/4685adaf1636aca2e1303effc1d5f130795708e1/src/pgmig/_build/queries/unsupported.sql),
and [roadmap](https://github.com/Apakottur/pgmig/issues/8).

### Stripe pg-schema-diff: the closest planner-core peer

Stripe's project is a released Go library and CLI. Like onwardpg, it can
materialize declarative SQL in temporary PostgreSQL and validate plan
convergence. Its SQL-operation dependency graph, per-statement timeouts, and
hazard model are mature.

It is currently ahead of onwardpg where the immediate PostgreSQL operation is
hard:

- same-name index replacement keeps the old index available while the new
  index builds concurrently;
- staged `NOT NULL`, `NOT VALID` constraints, and validation are mature;
- partitioned and local-index cases are broader;
- sequence `OWNED BY`, detailed identity changes, RLS policies, and table
  privileges are modeled (onwardpg now covers these too, with stricter
  explicit semantic authorization contractions, bound internally to the exact
  graph state); and
- its released acceptance corpus is substantially larger.

Its workflow makes different choices:

- names are object identity, so a rename becomes drop/add;
- a plan is a flat statement list, not an application-spanning
  expand/migrate/contract sequence;
- hazards warn or gate execution but do not capture missing developer intent;
- its catalog fetch is explicitly non-atomic;
- unknown object families are generally outside its model; onwardpg explicitly
  stops for its inventoried unsupported families, though its own inventory is
  not exhaustive yet;
- the CLI has an `apply` command; and
- it has no durable answers, migration history, or PR/base-restacking model.

The fairest summary is: Stripe is ahead where the DDL operation itself is
difficult; onwardpg is aiming ahead where the developer-and-agent decision
lifecycle is difficult.

Primary evidence: Stripe's [design and online-DDL
examples](https://github.com/stripe/pg-schema-diff/blob/6208f8f3ceccae8ca634055dc47907a6a864cb76/README.md),
[DDL materialization](https://github.com/stripe/pg-schema-diff/blob/6208f8f3ceccae8ca634055dc47907a6a864cb76/pkg/diff/schema_source.go),
[plan and hazard protocol](https://github.com/stripe/pg-schema-diff/blob/6208f8f3ceccae8ca634055dc47907a6a864cb76/pkg/diff/plan.go),
[CLI application boundary](https://github.com/stripe/pg-schema-diff/blob/6208f8f3ceccae8ca634055dc47907a6a864cb76/cmd/pg-schema-diff/apply_cmd.go),
and [acceptance harness](https://github.com/stripe/pg-schema-diff/blob/6208f8f3ceccae8ca634055dc47907a6a864cb76/internal/migration_acceptance_tests/acceptance_test.go).

### Alembic: revision control for SQLAlchemy schemas

Alembic is not a general PostgreSQL state-diff planner. Autogenerate compares
the live database with SQLAlchemy `MetaData` and writes a candidate Python
revision that the developer must review. Its own documentation explicitly
says autogenerate is not perfect.

It reliably covers the common SQLAlchemy model surface, but its documented
core limitations are important: table and column renames become add/drop,
anonymous constraints are problematic, and primary-key, exclusion, check,
and sequence changes are not general built-in autogenerate capabilities.
Third-party projects such as
[`alembic-utils`](https://github.com/olirice/alembic_utils) add PostgreSQL
functions, views, triggers, and policies.

Alembic is far ahead of onwardpg as a mature Python revision framework:
editable migration code, upgrade/downgrade execution, data operations,
branching and merge revisions, multi-database environments, and an extensive
extension ecosystem. onwardpg's advantage is orthogonal: it compares the
PostgreSQL state produced by any DDL source, refuses missing intent, explains
rollout phases, and does not own deployment.

Primary evidence: Alembic's [autogenerate scope and
limitations](https://alembic.sqlalchemy.org/en/latest/autogenerate.html),
[branch model](https://alembic.sqlalchemy.org/en/latest/branches.html),
[operation API](https://alembic.sqlalchemy.org/en/latest/ops.html), and
[offline SQL mode](https://alembic.sqlalchemy.org/en/latest/offline.html).

### Django: a particularly relevant omission

Django belongs in the primary comparison. It combines a model-state
autodetector with a dependency graph of editable Python operations. It can ask
whether a model or field was renamed, represents the answer as an explicit
`RenameModel` or `RenameField`, detects conflicting graph leaves, and supports
merge migrations.

Its workflow capabilities are especially relevant to onwardpg:

- `makemigrations --update` folds new model edits into the latest migration;
- `squashmigrations` optimizes a historical range into fewer replacement
  migrations while old and new histories coexist;
- `RunPython` and `RunSQL` make data work first-class;
- `SeparateDatabaseAndState` handles advanced cases where physical SQL and
  Django's model state must intentionally differ;
- PostgreSQL operations include concurrent index create/drop and deferred
  check-constraint validation; and
- CI can use `makemigrations --check` and `migrate --check`.

Those are real strengths, but the unit of truth is still Django's historical
model/migration state. Advanced PostgreSQL objects outside the Django model
surface require handwritten operations or packages, drift can exist between
the database and Django's state, and the runner applies/unapplies migrations.
Django's squashing also solves a different problem: compressing an established
revision range safely across deployed environments. onwardpg regenerates the
same *unexecuted* PR bundle from the latest base before merge.

That distinction is worth preserving. onwardpg can consume DDL exported from
a Django project, but a framework adapter is deliberately out of scope.

Primary evidence: Django's [migration
model](https://docs.djangoproject.com/en/6.0/topics/migrations/),
[command reference](https://docs.djangoproject.com/en/6.0/ref/django-admin/#makemigrations),
[operation API](https://docs.djangoproject.com/en/6.0/ref/migration-operations/),
[PostgreSQL operations](https://docs.djangoproject.com/en/6.0/ref/contrib/postgres/operations/),
and [advanced migration guide](https://docs.djangoproject.com/en/6.0/howto/writing-migrations/).

### Drizzle Kit: code-first schema and an integrated workflow

Drizzle Kit is close to the motivating developer experience because the
desired schema is TypeScript and SQL stays visible. `generate` compares the
current schema with its prior snapshot and writes SQL migrations; `push`
introspects a database, computes a diff, shows warnings, and applies it. Custom
SQL migrations cover work outside the declarative model.

Drizzle is far ahead as a TypeScript/ORM workflow and supports a broad modern
PostgreSQL declaration surface. Current source tests cover schema/table/column
renames and moves, common and expression constraints, rich indexes, enums,
sequences, identity and generated columns, ordinary and materialized views,
RLS policies, and optionally managed roles. It is not, however, a general
catalog-complete PostgreSQL planner: routines, triggers, extension lifecycle,
partitions, domains, composite/range types, grants, and ownership remain
outside the normal managed-entity surface or require custom SQL. Its journal
and snapshots are Drizzle-specific, and its application commands are
intentionally part of the product.

Drizzle's snapshot ancestry lets `drizzle-kit check` detect snapshot-parent
collisions after branches diverge; the developer then resolves and regenerates
the affected migration. That is more branch-aware than a filename-only runner,
but it is not a proof that arbitrary migrations commute and should not be
described as onwardpg-style PR restacking. Drizzle can render concurrent-index
syntax, but the PostgreSQL migrator normally wraps pending migrations in a
transaction, so the author still owns transaction-sensitive execution.

onwardpg treats Drizzle only as one possible DDL producer. This keeps the
planner independent of ORM journals and lets the same catalog graph compare a
reliable project-supplied full-schema export, handwritten DDL, or a live
database. Not every framework—including Django—ships such an export command;
replay into scratch PostgreSQL followed by a schema dump is also a valid
project implementation.

Primary evidence: Drizzle Kit's [workflow
overview](https://orm.drizzle.team/docs/kit-overview),
[`generate`](https://orm.drizzle.team/docs/drizzle-kit-generate),
[`push`](https://orm.drizzle.team/docs/drizzle-kit-push),
[v1 snapshot changes](https://orm.drizzle.team/docs/v0-v1-changes), and current
PostgreSQL tests for [indexes](https://github.com/drizzle-team/drizzle-orm/blob/9d6453215d18705986c2081124437bb6a03fb943/drizzle-kit/tests/indexes/pg.test.ts),
[views](https://github.com/drizzle-team/drizzle-orm/blob/9d6453215d18705986c2081124437bb6a03fb943/drizzle-kit/tests/pg-views.test.ts),
and [RLS policies](https://github.com/drizzle-team/drizzle-orm/blob/9d6453215d18705986c2081124437bb6a03fb943/drizzle-kit/tests/rls/pg-policy.test.ts).

## Adjacent tools not in the main table

- [Atlas](https://atlasgo.io/) is the broadest direct commercial/open-source
  reference for declarative schema diff, linting, migration directories, and
  multiple database engines. onwardpg previously used a pinned Atlas commit as
  a behavior-discovery corpus; it no longer claims one-for-one parity.
- [sqldef / psqldef](https://github.com/sqldef/sqldef) is another important
  direct diff peer, implemented in Go. It compares plain SQL desired state
  with PostgreSQL or another SQL file, supports dry-run and direct apply, and
  accepts explicit rename annotations for tables, columns, indexes, and enum
  values. Its multi-database, parser-backed approach and apply-oriented CLI are
  philosophically different from onwardpg's PostgreSQL-materialized graph and
  no-apply boundary; it deserves a dedicated future feature audit.
- [Prisma Migrate](https://www.prisma.io/docs/orm/prisma-migrate) is worth
  comparing when the desired source is a Prisma schema. It uses migration
  history plus a shadow database to detect drift and generate migrations,
  supports editable/custom SQL, and has explicit development and deployment
  commands. Like Drizzle, it is framework-bound and includes apply behavior.
- [pgroll](https://github.com/xataio/pgroll) is relevant to expand/contract but
  lives on the execution side of the boundary. It manages versioned schemas,
  triggers, backfills, and reversible rollouts in the database. onwardpg emits
  a plan and deliberately leaves execution and traffic choreography to the
  developer's existing system.
- Flyway, Liquibase, Goose, and Sqitch are important runners, but they do not
  answer the same catalog-diff and intent-recovery question. They can consume
  SQL produced after onwardpg's handoff.

## What onwardpg should learn—and what it should not become

The comparison suggests concrete planner work:

1. Extend the adopted Stripe-quality continuous index replacement and
   per-statement timeout/lock guidance through constraint-backed, attached,
   partition-parent, and materialized-view edge cases.
2. Close the explicitly useful object gaps: domains, composite types, sequence
   ownership, table ownership, grants, and RLS—only when they can satisfy the
   unknown-state and convergence rules.
3. Keep strengthening real-PostgreSQL convergence and failure tests; both
   pgmig and Stripe have good second-diff practices.
4. Make the answer-writing loop and PR restacking as obvious as Django's best
   interactive migration ergonomics, while retaining durable fingerprints.
5. Keep framework-export examples simple, but do not build adapters or absorb
   ORM journals.

It should not add a production apply command, down migrations, framework
plugins, or a general deployment runner. Those would blur the most useful
line in the product: onwardpg explains and proves how to get from here to
there; the developer or agent with CI, deployment, traffic, and data context
decides how and when to execute it.

## Honest current gaps

onwardpg is a developer preview, not the winner of a feature-count table.
Today:

- Stripe pg-schema-diff is broader, released, better exercised, and stronger
  on several online-DDL and security/partition cases.
- Migra's final surface still includes inheritance, collations, and historical
  transitions onwardpg lacks; grants, RLS/policies, and sequence ownership are
  no longer blanket onwardpg gaps, though case-level parity remains unproven.
- pgmig current `main` has domains, basic composites, and table ownership that
  onwardpg does not plan; onwardpg explicitly blocks those unmodeled states.
- Alembic, Django, and Drizzle have mature framework integration, migration
  runners, custom-data hooks, installed bases, and years of operational use.
- onwardpg has no published binaries, checksums, stable release record, broad
  performance evidence, or equivalently large real-PostgreSQL corpus yet.
- several typed graph families remain partial, especially advanced partition
  reconfiguration and arbitrary view/routine dependency rewrites.
- the catalog blocker inventory remains incomplete at its long tail and at
  some modeled-object attributes, although ownership/ACL, rules, text search,
  event triggers, publications, statistics, and FDW/server families now
  produce explicit blockers.

The preview's claim is narrower: when a schema is inside its modeled and
inventoried boundary, onwardpg aims to preserve intent and produce a
deterministic, reviewable, forward-only migration handoff. Catalog-family
inventory completeness remains a prerequisite for a production safety claim.
