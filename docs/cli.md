# CLI reference

The preferred developer-preview surface is:

~~~text
onwardpg config check
onwardpg init --target TARGET
onwardpg plan NAME --target TARGET
onwardpg plan --target TARGET --output sql
onwardpg status --target TARGET
onwardpg verify --target TARGET
onwardpg drift check --target TARGET --database URL
onwardpg diff --from SOURCE --to SOURCE
~~~

All commands print JSON by default. onwardpg never applies migration SQL to a
caller-owned database. `plan` keeps one worktree-local active migration anchor;
it derives the durable bundle from accepted history to working DDL and, when a
development database is configured, separately prints a safe D → W
reconciliation with `--output sql`. init, plan, and verify use scratch_database_env when
configured and otherwise fall back to dev_database_env for compatibility.

`draft`, `dev plan`, `history status`, and source-to-source `plan --from --to`
remain lower-level compatibility interfaces. New integrations should use
`plan`, `status`, and `diff` above. They are documented below because their
receipts and protocols remain supported during the developer preview.

## plan

~~~sh
onwardpg plan [NAME] --target primary \
  [--bundle ID] [--hint JSON] [--hints-file FILE] \
  [--dev-hint JSON] [--dev-hints-file FILE] \
  [--output json|text|sql]
~~~

`plan NAME` starts one evolving logical migration in the current worktree.
Later `plan --target primary` calls resume it; there is no lock or finalize
command. The bundle is always recalculated from replayed accepted history (H)
to exported working DDL (W), so an incoming accepted migration becomes part of
the ground beneath the same feature bundle rather than a second feature
migration. `--bundle` selects the same anchor in a clean CI checkout.

When development configuration is present, `plan --output sql` writes only the
direct development reconciliation (D → W) to stdout. It is deliberately not
the cumulative PR bundle and it preserves surplus dev objects in workspace
mode, preventing branch switches from suggesting destructive cleanup. JSON and
text output contain the reviewable H → W bundle report plus that D → W report.
No output is applied automatically.

`plan` exits `2` when either comparison needs a decision or editable SQL. A
durable question is answered with the same strict `--hint` form as `draft`.
Development questions are reported independently and are answered on the same
command with `--dev-hint` (or `--dev-hints-file`). A valid history-to-working
answer is never guessed to mean the same thing for an arbitrary long-lived
development database. Workspace mode preserves an absence-only object from D
rather than proposing a local rename or drop; scoped development hints are for
strict/disposable databases or an actual incompatible D → W transition.

## status

~~~sh
onwardpg status --target primary
~~~

Reads the worktree-local active-plan anchor and repository history only. It
reports the PlanID, selected bundle, and whether its parent is current, stale,
or invalid. It does not read Git or contact PostgreSQL. `verify --target` uses
this same anchor by default.

## config check

~~~sh
onwardpg config check [--config .onwardpg.toml]
~~~

Validates the versioned repository configuration, connects to the development
and scratch URLs, deterministically exports every target's DDL, materializes it
in disposable PostgreSQL, validates existing history, and requires all three
PostgreSQL-major receipts to agree.

When a target has an `ignore` list, `config check` also inspects the development
catalog read-only. Every selector must match the exported DDL or development
catalog, and the JSON receipt lists the exact excluded objects. This lets a
target acknowledge provider-owned state that may be absent from replay history.

## history status

~~~sh
onwardpg history status --target primary [--bundle payment-settlement]
~~~

Inspects only repository receipts. Without `--bundle`, it returns the ordered
chain, `head_bundle`, digest, and exact `head_ref`. With `--bundle`, the selected
head entry is excluded and the command reports whether its parent is current,
stale, or missing. Selecting a valid historical non-head is blocked explicitly.
Invalid forks, missing parents, and altered history exit 4. The command does
not inspect Git or connect to PostgreSQL.

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

Repeatable `--ignore` flags are strict for that invocation: an unused selector
is an error. Long-lived provider exclusions belong in the target-level `ignore`
list in `.onwardpg.toml`; those may be dormant in one H → W comparison because
the object exists only in the development catalog.

## draft

~~~sh
onwardpg draft \
  --target primary \
  --bundle payment-settlement \
  --after "$BASE_HEAD" \
  [--create] \
  [--hint '{"kind":"..."}'] \
  [--hints-file hints.json] \
  [--output text|json] \
  [--purpose feature|repair|contract]
~~~

draft is the durable H → W loop. `$BASE_HEAD` denotes the exact `head_ref`
copied from `history status`; it is not a bare bundle name.

1. the named bundle is the only excluded/mutable history entry;
2. every other bundle must form one valid content-addressed chain;
3. that chain must end at the exact accepted name-and-digest `head_ref` named
   by `--after`;
4. the chain is replayed in disposable PostgreSQL;
5. working DDL is compiled twice and materialized in disposable PostgreSQL;
6. the typed graph planner emits semantic choices or a complete plan;
7. a complete generated plan is clone-verified before the bundle is written.

The command is deliberately Git-free. A coding agent is responsible for
pulling, rebasing, selecting the bundle ID, and copying the accepted base
`head_ref` into `--after`. `--create` is required once when the bundle is absent;
later refreshes reject a missing folder so a rebase cannot silently lose agent
SQL. A mismatch blocks accidental stacking on another unpublished or
same-named rewritten bundle. If new accepted history makes the selected parent
stale, rerun the same command with the new exact head reference. Only exact
participating-object scope matches are carried across that change.

`--hint` is repeatable and accepts one strict semantic JSON object.
`--hints-file` accepts an array of the same objects. Current kinds are identity,
rename, drop, type_change, rollout, confirm, and manual_sql. `identity` is a
table-only upstream assertion: it lets an agent state that two observed table
names are the same relation before normal rename candidacy. It never guesses a
transition; an unautomatable asserted identity becomes an editable compatibility
bridge. Hints may be supplied on
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
Its decision output protocol is `onwardpg.draft/v4`: `needs_decisions` contains
semantic choice sets plus the path and receipts it wrote, while
`needs_sql_edits` names the bundle path and files the agent must edit.
Fingerprint-bound answers are generated receipts, not an agent-facing
authoring format.

If the asserted base already produces the desired schema, a generated-only
selected bundle is removed with `status: "absorbed"`. An edited bundle is
preserved for explicit reconciliation because onwardpg cannot infer whether
its data work remains necessary.

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
Partial verification exits zero with `partial_verified` only after the exact
prefix and its full continuation both succeed on independent disposable
clones. Output includes `simulated_bundle_phases`, `remaining_bundle_phases`,
and the expected residual; these never claim a real environment applied them.
Assertions that run only after the continuation are named separately as
`full_continuation_assertions`, not `verified_assertions`.

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

## diff (and compatibility `plan --from --to`)

~~~sh
onwardpg diff --from SOURCE --to SOURCE [options]
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

The remaining planner and ignore flags match dev plan. `diff` never writes a
bundle. `plan --from --to` is retained as a compatibility spelling. Use the
high-level `plan NAME --target TARGET` when the result belongs to configured
onwardpg history.

## Exit codes

| Exit | Meaning |
| --- | --- |
| 0 | Complete plan, no changes, absorbed generated draft, or successful full verification |
| 2 | A semantic decision, SQL edit, or explicitly checked development postcondition needs review |
| 3 | Unsupported schema state |
| 4 | History, convergence, residual, or policy blocker |
| 1 | Invocation, configuration, source, or internal error |

There is no apply command, production destination, ORM journal handoff, hidden
Git mutation, or generated down migration.
