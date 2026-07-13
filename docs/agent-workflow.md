# Agent-assisted migration workflow

onwardpg is designed for an agent that has the feature's developer intent in
its context but must still leave an auditable trail. The agent does not turn a
schema diff into an automatic deployment. It repeatedly asks onwardpg to state
what is knowable from catalog state, records the decisions that are knowable
from the feature context, and stops when an operator-owned data or availability
decision is required.

The receipts for a migration should live beside the reviewed SQL:

```text
migrations/20260713_profile_name/
├── intent.md              # developer/agent explanation of the feature intent
├── decision-1.json        # exact needs_input result from onwardpg
├── answers.json           # reviewed, fingerprint-bound decisions
├── plan.json              # exact ready plan used to render SQL
├── migration.sql          # reviewed forward-only SQL rendering
└── clone-verification.md  # clone URL/identity, command, result, residual diff
```

Do not edit the fingerprints or question keys. An answer file is deliberately
invalid once either input schema changes.

## The decision loop

An agent should follow this loop for every feature:

1. Write or obtain the desired DDL from the application schema tool. This can
   come from Drizzle, Django, Prisma, SQLAlchemy, or handwritten PostgreSQL
   `CREATE` statements.
2. Run `onwardpg plan` against a production-like clone or development database.
   Save the raw JSON result, including a `needs_input` result.
3. Branch only on the documented exit status:

   | Status / exit | Agent action |
   | --- | --- |
   | `planned` / `0` | Save the JSON plan, render SQL, review hazards/batches, and test it on a clone. |
   | `needs_input` / `2` | Read every question. Answer only intent the developer context explicitly establishes; otherwise ask the developer. Rerun with a complete answer file. |
   | `unsupported` / `3` | Do not try to paper over it with invented SQL. Narrowly ignore only an object the developer intentionally accepts as out of scope, or change the migration design / implement support. |
   | `error` / `1` | Fix invocation, credentials, DDL, or environment. Do not treat it as a schema decision. |

4. Repeat until `planned`. A planned result is a proposal, not authority to
   apply it.
5. Render SQL only from the saved ready plan inputs. Test it on a clone using
   the deployment mechanism that will execute it, then rerun onwardpg against
   that clone to require an empty residual diff.

The safe decision rule is simple: a coding agent may select a listed rename or
drop choice only when the developer's feature intent says so. It may never
invent a cast, a backfill, a partition move, a `REFRESH MATERIALIZED VIEW`
mode, or a destructive acknowledgement merely to make the CLI return `0`.

## Example 1: a simple column rename

The feature request says: “Rename `app.users.name` to `full_name`; retain all
existing values. Old code will be gone before the contract migration runs.”
The agent produces the desired DDL:

```sql
CREATE SCHEMA app;

CREATE TABLE app.users (
  id bigint PRIMARY KEY,
  full_name text NOT NULL
);
```

The current clone already has `app.users(id, name)`. The agent's first call is
deliberately answer-free and saves the raw receipt:

```sh
mkdir -p migrations/20260713_profile_name

onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  > migrations/20260713_profile_name/decision-1.json
status=$?
test "$status" -eq 2
```

The important receipt is the `needs_input` response, not an agent's guess.
Its actual fingerprints and object keys are copied verbatim; abbreviated
fingerprints below are illustrative:

```json
{
  "protocol_version": "onwardpg.plan/v1",
  "status": "needs_input",
  "current_fingerprint": "sha256:current…",
  "desired_fingerprint": "sha256:desired…",
  "questions": [
    {
      "kind": "rename_column",
      "key": "column:app:users:name",
      "choices": ["column:app:users:full_name"],
      "message": "Did column app.users.name move to app.users.full_name?"
    }
  ]
}
```

Because the developer explicitly said this is a rename, the agent can prepare
the following answer for review. It does not need to—and must not—derive the
choice from a name similarity heuristic:

```json
{
  "protocol_version": "onwardpg.plan/v1",
  "current_fingerprint": "sha256:current…",
  "desired_fingerprint": "sha256:desired…",
  "answers": [
    {
      "kind": "rename_column",
      "key": "column:app:users:name",
      "value": "column:app:users:full_name"
    }
  ]
}
```

The answer file must use the *full*, unmodified fingerprints from
`decision-1.json`. The agent then obtains the ready plan and human-readable
SQL as two separate receipts:

```sh
onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --answers migrations/20260713_profile_name/answers.json \
  > migrations/20260713_profile_name/plan.json

onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --answers migrations/20260713_profile_name/answers.json \
  --sql > migrations/20260713_profile_name/migration.sql
```

The rendered file has phase and transaction comments; the important operation
is reviewable rather than hidden in an agent transcript:

```sql
-- ============================================================================
-- CONTRACT — run only after old application code no longer uses the prior shape.
-- ============================================================================
-- Batch batch-001: transactional.
ALTER TABLE "app"."users" RENAME COLUMN "name" TO "full_name";
```

The agent records why it is contract work in `intent.md`, applies the reviewed
file only to a clone using the team's normal executor, then reruns the same
comparison against that clone. An empty planned result is the convergence
receipt. If the feature changes before merge, the old fingerprints make the
answer file fail safely and the agent starts the loop again.

## Example 2: advanced reporting schema and rollout plan

This example deliberately combines objects that a plain table diff often makes
hard to review:

- an `app.orders` range-partitioned table and monthly partition children;
- a new standalone reporting index that must be created concurrently;
- a function and ordinary view used by a materialized dashboard view; and
- a changed partition bound that must not be guessed because it can validate,
  lock, or move data.

The desired declarative DDL might include:

```sql
CREATE SCHEMA app;

CREATE TABLE app.orders (
  id bigint PRIMARY KEY,
  customer_id bigint NOT NULL,
  created_at timestamptz NOT NULL,
  total_cents integer NOT NULL
) PARTITION BY RANGE (created_at);

CREATE TABLE app.orders_2026_07
  PARTITION OF app.orders
  FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');

CREATE FUNCTION app.day_start(timestamptz) RETURNS date
LANGUAGE sql IMMUTABLE AS $$ SELECT t::date $$;

CREATE VIEW app.daily_order_totals AS
  SELECT app.day_start(created_at) AS day, sum(total_cents) AS total_cents
  FROM app.orders GROUP BY 1;

CREATE MATERIALIZED VIEW app.dashboard_daily_order_totals AS
  SELECT * FROM app.daily_order_totals;

CREATE UNIQUE INDEX dashboard_daily_order_totals_day_key
  ON app.dashboard_daily_order_totals (day);
```

Assume the current clone already has the same reporting objects with an older
function/view definition and an existing `orders_2026_07` child whose bound is
being corrected. The example therefore demonstrates a replacement/refresh and
a partition reconfiguration, rather than implying that a brand-new child needs
to be detached.

The agent requests an indexed, online-aware plan:

```sh
onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/reporting-schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --concurrent-indexes \
  > migrations/20260713_reporting/decision-1.json
```

There are three distinct outcomes worth noticing:

1. **Typed dependency order is automatic.** The graph schedules schema/type/
   table prerequisites before the function and ordinary view, and handles
   dependent view/materialized-view ordering on drop or recreate. The agent
   does not topologically sort SQL text itself.
2. **The standalone index is a separate non-transactional batch.** The final
   SQL says it must run outside `BEGIN`/`COMMIT`; an executor can therefore
   preserve PostgreSQL's `CREATE INDEX CONCURRENTLY` requirement.
3. **Operational intent is not automatic.** If the desired schema changes an
   existing partition bound or parent/default relationship, onwardpg returns a
   `partition_reconfiguration` question. If a changed view or routine can make
   a dependent materialized view's stored rows stale, it returns a
   `refresh_materialized_view` manual-work question.

The agent can carry developer intent into an operator-reviewed answer file;
the SQL itself must be supplied rather than inferred. The exact keys and
fingerprints are copied from `decision-1.json`:

```json
{
  "protocol_version": "onwardpg.plan/v1",
  "current_fingerprint": "sha256:current…",
  "desired_fingerprint": "sha256:desired…",
  "answers": [
    {
      "kind": "partition_reconfiguration",
      "key": "table:app:orders_2026_07",
      "value": "provided",
      "manual": {
        "summary": "Move the reviewed, empty July partition to the approved bound.",
        "execution_mode": "transactional",
        "statements": [
          "ALTER TABLE \"app\".\"orders\" DETACH PARTITION \"app\".\"orders_2026_07\";",
          "ALTER TABLE \"app\".\"orders\" ATTACH PARTITION \"app\".\"orders_2026_07\" FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');"
        ],
        "verification_sql": [
          "SELECT count(*) = 0 FROM \"app\".\"orders_2026_07\";"
        ]
      }
    },
    {
      "kind": "refresh_materialized_view",
      "key": "materialized_view:app:dashboard_daily_order_totals",
      "value": "provided",
      "manual": {
        "summary": "Refresh dashboard totals after the reporting-view change.",
        "execution_mode": "non_transactional",
        "statements": [
          "REFRESH MATERIALIZED VIEW CONCURRENTLY \"app\".\"dashboard_daily_order_totals\";"
        ],
        "verification_sql": [
          "SELECT count(*) >= 0 FROM \"app\".\"dashboard_daily_order_totals\";"
        ]
      }
    }
  ]
}
```

This is intentionally a showcase of the boundaries as well as the supported
graph. The agent must verify that the partition is genuinely empty before using
the shown detach/attach contract, and `REFRESH ... CONCURRENTLY` requires a
suitable unique index on the materialized view. The developer/operator chooses
those details; onwardpg makes the decision, execution mode, and verification
visible in the plan.

After the answer file is accepted, the rendered plan will be structured like:

```sql
-- EXPAND — compatible schema additions.
CREATE UNIQUE INDEX CONCURRENTLY "dashboard_daily_order_totals_day_key"
  ON "app"."dashboard_daily_order_totals" ("day");

-- MIGRATE — reviewed schema changes and hazards.
-- (typed table/function/view dependency-ordered DDL appears here)

-- MANUAL — execute only with the checked-in operator contract.
-- Partition reconfiguration: Move the reviewed, empty July partition ...
ALTER TABLE "app"."orders" DETACH PARTITION "app"."orders_2026_07";
ALTER TABLE "app"."orders" ATTACH PARTITION "app"."orders_2026_07"
  FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');

-- Batch manual-non-transactional: non-transactional; execute outside BEGIN/COMMIT.
REFRESH MATERIALIZED VIEW CONCURRENTLY "app"."dashboard_daily_order_totals";
```

The exact operation placement depends on the current and desired catalog
graphs; the shown SQL is a shape guide, not a substitute for the saved
`plan.json`. The receipts that make it safe are the initial questions, the
fingerprint-bound manual contracts, the final plan/SQL, clone execution log,
and the empty residual diff.
