# CLI reference

The preferred developer-preview surface is:

~~~text
onwardpg config check
onwardpg init --target TARGET
onwardpg dev plan --target TARGET
onwardpg draft --target TARGET --bundle ID
onwardpg verify --target TARGET --bundle ID
onwardpg drift check --target TARGET --database URL
onwardpg plan --from SOURCE --to SOURCE
~~~

All commands print JSON by default. onwardpg never applies migration SQL to a
caller-owned database. init, draft, and verify use scratch_database_env when
configured and otherwise fall back to dev_database_env for compatibility.

## config check

~~~sh
onwardpg config check [--config .onwardpg.toml]
~~~

Validates the versioned repository configuration, deterministically exports
every target's DDL, materializes it in disposable PostgreSQL, inspects the typed
catalog, and prints each target's fingerprint and detected PostgreSQL major.

## init

~~~sh
onwardpg init \
  --target primary \
  [--bundle baseline] \
  [--concurrent-indexes] \
  [--ignore SELECTOR]
~~~

Creates the first content-addressed history entry from empty PostgreSQL to the
configured desired DDL. It clone-verifies the bundle before installation and
refuses a non-empty target history. It creates databases only through the
configured administrative role.


## dev plan

~~~sh
onwardpg dev plan \
  --target primary \
  [--hint '{"kind":"..."}'] \
  [--hints-file hints.json] \
  [--output text|json]
~~~

Reads the current catalog from dev_database_env and compares it with the
configured working DDL. The current database is inspected read-only. Desired
DDL is materialized in a disposable database.

This D → W loop is intentionally independent from durable history. The command
never runs its emitted SQL.

Planner options include:

| Flag | Meaning |
| --- | --- |
| --hint JSON | Semantic decision; repeatable |
| --hints-file FILE | Array of semantic decisions |
| --concurrent-indexes | Build eligible standalone indexes concurrently |
| --if-not-exists | Use supported IF NOT EXISTS forms |
| --if-exists | Use supported IF EXISTS forms |
| --cascade-drops | Permit supported CASCADE rendering after destructive approval |
| --schema-qualifier VALUE | Scope and render through one schema qualifier |
| --ignore SELECTOR | Narrow validated catalog exclusion; repeatable |
| --output text\|json | Render copyable decisions/phased SQL or the JSON protocol |

## draft

~~~sh
onwardpg draft \
  --target primary \
  --bundle payment-settlement \
  [--hint '{"kind":"..."}'] \
  [--hints-file hints.json] \
  [--output text|json] \
  [--purpose feature|repair|contract]
~~~

draft is the durable H → W loop:

1. the named bundle is the only excluded/mutable history entry;
2. every other bundle must form one valid content-addressed chain;
3. the chain is replayed in disposable PostgreSQL;
4. working DDL is compiled twice and materialized in disposable PostgreSQL;
5. the typed graph planner emits semantic choices or a complete plan;
6. a complete generated plan is clone-verified before the bundle is written.

The command is deliberately Git-free. A coding agent is responsible for
pulling, rebasing, and selecting the bundle ID. If new history makes the
selected bundle's parent stale, the same command replans it on the new head.
Only exact participating-object scope matches are carried across that change.

`--hint` is repeatable and accepts one strict semantic JSON object.
`--hints-file` accepts an array of the same objects. Current kinds are rename,
drop, type_change, rollout, confirm, and manual_sql. Hints may be supplied on
the first invocation; every hint must consume an actual graph decision.

`manual_sql` never contains SQL. It writes a `needs_sql_edits` bundle with a
phase-local TODO. Replace the TODO directly in the phase file and run verify.
The incomplete bundle exits 2 and cannot become immutable base history.

A valid selected bundle remains replaceable because the bundle ID is explicit,
including after local application or verification. Receipted agent edits are
carried when their phase did not also change in the new generated plan;
same-phase conflicts preserve the current SQL, install the new plan as its
unreceipted basis, and return all three SQL versions plus the `verify` next
step.

draft supports the same planner flags as dev plan.
Its decision output protocol is `onwardpg/draft/2`: `needs_decisions` contains
only semantic choice sets, while `needs_sql_edits` names the bundle path and
files the agent must edit. Fingerprint-bound answers are generated receipts,
not an agent-facing authoring format.

## verify

~~~sh
onwardpg verify \
  --target primary \
  --bundle payment-settlement \
  [--through expand|migrate|contract]
~~~

Creates disposable databases, executes validated history through the selected
phase, catalog-inspects the result, and compares it with a separate full-chain
execution. Full verification exits zero only for an empty residual. Edited
phase files and verify.sql assertions are receipted only after this succeeds.

Use --check for read-only CI-style verification. It rejects unreceipted edits
instead of refreshing their receipts, requires the selected bundle to be the
history head, recompiles the configured desired DDL, and rejects a stale
desired fingerprint before clone execution.


An edited phase without directives is transactional. Exact
-- onwardpg:batch transactional and -- onwardpg:batch nontransactional comments
split additional execution batches. Exact -- onwardpg:assert NAME comments
split boolean queries in verify.sql.

## drift check

~~~sh
onwardpg drift check \
  --target primary \
  --database "$PRODUCTION_DATABASE_URL" \
  [--ignore SELECTOR]
~~~

Replays the complete receipted history head in disposable PostgreSQL, inspects the
explicitly supplied live catalog read-only, and reports typed missing,
unexpected, and changed objects. Exit zero means drift_free; drift exits 4.

The audit never generates repair SQL, changes history, or participates in
ordinary draft generation.

## plan

~~~sh
onwardpg plan --from SOURCE --to SOURCE [options]
~~~

SOURCE is either a PostgreSQL URL or file:///absolute/path/schema.sql. Live URLs
are inspected read-only. DDL files are executed in disposable PostgreSQL
through --dev-url and then catalog-inspected; onwardpg does not partially parse
DDL.

| Flag | Meaning |
| --- | --- |
| --from SOURCE | Required current schema |
| --to SOURCE | Required desired schema |
| --dev-url URL | Administrative URL required for DDL sources |
| --hint JSON | Semantic decision; repeatable |
| --hints-file FILE | Array of semantic decisions |
| --output text\|json | JSON by default; text renders decisions or SQL |

The remaining planner and ignore flags match dev plan. `plan` never writes a
bundle. Use `draft` when the result belongs to the configured append-only
history.

## Exit codes

| Exit | Meaning |
| --- | --- |
| 0 | Complete plan or successful full verification |
| 2 | A semantic decision or SQL edit remains |
| 3 | Unsupported schema state |
| 4 | History, convergence, residual, or policy blocker |
| 1 | Invocation, configuration, source, or internal error |

There is no apply command, production destination, ORM journal handoff, hidden
Git mutation, or generated down migration.
