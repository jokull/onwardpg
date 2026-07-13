# onwardpg

`onwardpg` is a forward-only PostgreSQL schema-diff and migration-planning tool
for developers and coding agents.

You describe the schema you want in Drizzle, Django, Prisma, SQLAlchemy, or
plain PostgreSQL DDL. onwardpg compares that desired state with the schema that
actually exists, reports drift, and drafts a reviewable plan to get from here
to there. It never applies the plan for you.

The project is built around the workflow described in [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive migrations from
the real difference between a declarative schema and a real database; commit
and review forward SQL; test difficult work on a clone; and treat migrations as
an operational change, not an opaque CI side effect.

## The experience we are building

The ideal interaction is not “ask an agent to write some `ALTER TABLE`
statements and hope.” It is a tight, inspectable loop:

1. An application or coding agent changes the declarative schema while feature
   work is still in motion.
2. A schema adapter produces DDL or a typed snapshot. onwardpg materializes DDL
   in a disposable PostgreSQL database and catalog-inspects both sides, so the
   comparison is against PostgreSQL semantics rather than a partial SQL parser.
3. onwardpg generates a deterministic plan from the current production-like
   schema to the desired schema, including accidental drift from abandoned or
   concurrent work.
4. If intent cannot be known from the two schemas, onwardpg asks a typed,
   fingerprint-bound question. An agent or reviewer answers explicitly in a
   checked-in file, then reruns the exact plan.
5. The reviewed SQL is committed as a forward migration. It is tested on a
   clone, deployed in compatible stages, and re-diffed until the residual is
   empty.

The tool should make the safe path the convenient path, while keeping a human
or agent accountable for the decisions that require knowledge of data,
application behavior, traffic, and rollout timing.

```text
declarative schema ──► disposable PostgreSQL ──► desired catalog graph
                                                        │
live / clone database ───────────────────────────► current catalog graph
                                                        │
                                                        ▼
                                             typed dependency diff
                                                        │
                               questions / checked-in answers / hazards
                                                        │
                                                        ▼
                                      reviewable forward migration plan
```

## Forward only, in readable stages

There are no generated down migrations. A production rollback is a new,
reviewed forward migration. That is deliberate: real recovery and safe rollout
usually require more thought than reversing a list of DDL statements.

Plans are divided into stages that should remain obvious even after an agent
has handed the migration to a person for review:

```sql
-- ============================================================================
-- EXPAND — run before deploying code that relies on the new shape.
-- Keep this compatible with the currently deployed application.
-- ============================================================================
CREATE INDEX CONCURRENTLY "users_email_idx" ON "public"."users" ("email");

-- ============================================================================
-- MIGRATE — deploy compatible code, then run and observe the data work.
-- onwardpg will not invent a cast or backfill it cannot prove from schema.
-- ============================================================================
-- Application-owned backfill example:
-- UPDATE "public"."events" SET "occurred_on" = "occurred_at"::date
-- WHERE "occurred_on" IS NULL;

-- ============================================================================
-- CONTRACT — run only once old code no longer needs the prior shape.
-- ============================================================================
ALTER TABLE "public"."events" ALTER COLUMN "occurred_on" SET NOT NULL;
```

`onwardpg plan --sql` emits `EXPAND`, `MIGRATE`, `CONTRACT`, and `MANUAL`
sections with transaction-batch comments. The plan makes application release
boundaries, lock/rewrite/validation hazards, and explicit operator work visible
in both JSON and committed SQL. A data backfill is never silently generated
merely because it would make a schema diff apply.

## Explicit intent, not clever guessing

Schema state alone cannot tell us whether `users.name` became
`users.full_name`, whether a column removal is intentional, or how to convert
existing timestamps into dates. A tool that guesses those things can preserve
the wrong data or destroy the right data.

onwardpg detects credible rename candidates for tables, columns, indexes, and
enums. It does not accept them automatically. It returns a machine-readable
question, and the answer binds to the current and desired schema fingerprints:

```json
{
  "protocol_version": "onwardpg.plan/v1",
  "current_fingerprint": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "answers": [
    {
      "kind": "rename_column",
      "key": "column:public:users:name",
      "value": "column:public:users:full_name"
    }
  ]
}
```

Stale, contradictory, invalid, and unused answers are rejected. The same rule
applies to destructive changes and type conversions: an agent can propose a
plan, but it cannot smuggle in unproven intent.

## Intended developer workflow

```sh
# Produce schema.sql from your ORM/declarative schema tool, then:
onwardpg plan \
  --from "$DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --concurrent-indexes \
  > plan.json

# Exit 2 means: commit an explicit answer file, then rerun.
onwardpg plan \
  --from "$DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --answers migration.answers.json \
  --concurrent-indexes \
  --sql > migrations/20260712_account_profile.sql
```

The review checklist is simple:

- Is the desired schema actually what the application needs?
- Are rename, drop, cast, and backfill decisions explicit and correct?
- Does `EXPAND` work with the old application version?
- Is `MIGRATE` safe for real data and acceptable under expected load?
- Is `CONTRACT` deferred until the compatibility window is over?
- Has the reviewed plan converged to an empty diff on a representative clone?

CI may detect drift and verify a reviewed plan. It must not auto-apply a new
schema diff to production.

## Developer preview: what works now

onwardpg is PostgreSQL-only and supports PostgreSQL 14–18. This is a developer
preview, not a production-release claim. It is useful today when its safety
boundary matches your schema, and it is intentionally loud when it does not.

| Area | Preview behavior |
| --- | --- |
| Inputs | Live PostgreSQL URLs and `file://` declarative DDL materialized in disposable PostgreSQL databases. |
| Core relations | Schemas, tables, columns, defaults, identity/generated columns, collations, comments, indexes, sequences, enums, and common constraints. |
| Dependencies | Typed ordering for tables, constraints, indexes, enums, views, materialized views, routines, triggers, and partition children. |
| Intent | Fingerprint-bound questions for credible renames, destructive drops, type conversions, and manual operational work. |
| Output | Deterministic JSON plus readable, forward-only SQL divided into `EXPAND`, `MIGRATE`, `CONTRACT`, and `MANUAL` batches. |
| Safety | No automatic apply, no generated down migration, no guessed cast/backfill, and no silent omission of unknown catalog state. |

Ordinary views support create, replace, drop, and answer-bound rename.
Materialized views support create/drop and an answer-bound native rename that
retains unchanged indexes; a rebuild is deliberately a reviewed destructive
operation. Functions, procedures, and triggers are catalog-modeled, including
their important ordering relationships. Partition-child attach/detach is
planned with hazards; a bound, parent, or default-partition change requires an
operator-authored manual-work contract rather than inferred data movement.

More concretely, the preview plans structural index create/drop/rename/rebuild
(including concurrent execution when requested), standalone sequence
create/drop/parameter changes, and extension create/drop/version/schema
changes. It plans ordinary-view replacement and typed view/routine/trigger
ordering, as well as partitioned-table and partition-child creation. These are
supported capabilities—not generic “unsupported resources.” The conservative
boundary is at ambiguous or operational transitions: for example an arbitrary
materialized-view rebuild, an unprovable dependent-view rewrite, or partition
reconfiguration that could move or validate data.

### Compared with Migra

[Migra](https://github.com/djrobstep/migra) established the developer-friendly
idea: compare two PostgreSQL schemas, print the migration SQL, and use that
diff to keep application models and a real database aligned. Its repository is
now deprecated; its final release was in 2022, with its functionality moved to
the author's `results` project.

onwardpg keeps Migra's valuable workflow—no migration history is needed to
*discover* a change; compare the real current schema with the desired
schema—but takes a different operational stance:

| | Migra | onwardpg developer preview |
| --- | --- | --- |
| Primary output | Ordered SQL diff | JSON plan and annotated, forward-only SQL |
| Schema inputs | PostgreSQL schemas | Live PostgreSQL or PostgreSQL-materialized declarative DDL |
| Ambiguous rename/drop/cast | Generates the schema diff | Stops for a typed, fingerprint-bound answer |
| Data migrations | Outside the schema diff | Explicit `MIGRATE` / `MANUAL` hand-off; never invented |
| Rollout | One generated sequence | Expand/contract phases, hazards, and valid transaction batches |
| Unknown objects | Inspector-dependent | Explicit unsupported result unless consciously ignored |

This is not a claim that onwardpg replaces Migra for every schema today. Migra
has mature coverage of several object families that onwardpg still blocks, and
onwardpg's preview contract is intentionally stricter around destructive and
operational changes.

### Known preview gaps

Do not use onwardpg as the sole production migration authority yet. In
particular, it currently blocks or requires manual work for:

- domains, composite/range types, ownership and grants, RLS/policies, rules,
  text-search objects, foreign tables, and extension-owned state outside the
  modeled boundary;
- arbitrary view/routine-body dependency rewrites, materialized-view rebuild
  transitions, and most routine-body semantic analysis;
- partition-bound/parent/default reconfiguration, data backfills, and casts
  whose correct behavior depends on live data;
- broad multi-schema moves and complex cross-schema dependency transitions;
- production release concerns: published artifacts, checksums, signing,
  benchmarked performance bounds, and a completed real-database compatibility
  corpus.

The machine-readable [feature map](parity/pgmig-roadmap.json) records the
broader PostgreSQL roadmap. The [reference behavior study](parity/atlas-postgres.json)
is retained as test research, not a product-completeness promise. See
[supported features](docs/supported-features.md) for the detailed boundary.

## Roadmap to the complete DX

- Produce a single agent hand-off bundle: plan JSON, commented SQL,
  answer-file template, hazard summary, and review checklist.
- Emit phase-specific migration artifacts so `expand`, observed data work, and
  later `contract` can be reviewed and committed independently.
- Make clone application and residual-diff verification first-class.
- Build a real-PostgreSQL convergence corpus for the preview's documented
  supported behavior across PostgreSQL 14–18.
- Extend the graph and planner through the broader pgmig roadmap without
  weakening the rule that unknown state is explicit.
- Publish reproducible binaries, checksums, release automation, performance
  bounds, and the supported-version policy required for production use.

## Documentation

- [CLI reference](docs/cli.md)
- [JSON protocol and answer files](docs/protocol.md)
- [Adapter API](docs/adapter.md)
- [Forward-only migration workflow](docs/migration-workflow.md)
- [Safety model](docs/safety-model.md)
- [Architecture](docs/architecture.md)
- [Reference behavior study](docs/atlas-postgres-parity.md)
- [Supported-feature ledger](docs/supported-features.md)
- [Changelog](CHANGELOG.md)
- [Installation and release status](docs/installation.md)
