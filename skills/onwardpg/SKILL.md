---
name: onwardpg
description: Plan, revise, restack, verify, and assess contract readiness for PostgreSQL schema migrations with onwardpg. Use when a repository has .onwardpg.toml or the user asks to create or update an onwardpg plan, resolve planner decisions, author a backfill or compatibility transition, handle schema drift across branches, verify a migration bundle, or check post-expand readiness.
license: MIT
compatibility: Requires the onwardpg CLI, the repository's configured schema exporter, and access to its configured scratch PostgreSQL administrator. Development and production database access are optional and must remain separately scoped.
metadata:
  author: onwardpg
  version: "1.0"
---

# Plan PostgreSQL migrations with onwardpg

Use onwardpg as the catalog-aware planner and verifier. Bring application intent from the repository and the user; do not invent product semantics from a schema diff.

## Hard boundaries

- onwardpg writes reviewable migration bundles. It does not deploy them to production.
- Never run generated phase SQL against development, staging, or production unless the user explicitly asks for that separate operation.
- Never treat development reconciliation (`D -> W`) as the durable feature migration (`H -> W`).
- Never copy production rows, credentials, secrets, or sensitive values into prompts, bundles, logs, or verification fixtures.
- Supply a semantic hint only when application code, product requirements, or bounded data evidence proves it.
- Edit only files and `ONWARDPG TODO` pockets named by planner output. Preserve markers and generated ownership boundaries.
- Treat successful disposable verification as structural and semantic evidence, not proof of live data distribution, lock duration, traffic drain, replica health, or deployment completion.
- Treat `onwardpg contract check` as read-only, expiring readiness evidence. It
  does not execute contract or replace the assertion repeated in contract SQL.

Read [the schema-state model](references/schema-states.md) before interpreting a diff. Read [production evidence](references/production-evidence.md) before querying any live or replicated database.

## Inspect before planning

1. Find `.onwardpg.toml` and read the target, PostgreSQL major, bundle root, schema exporter, and optional development database configuration.
2. Read the declarative schema source and the application readers and writers affected by the requested change.
3. Inspect existing bundles and repository delivery conventions. Do not create a second speculative migration for the same feature.
4. Run `onwardpg config check`. Fix configuration or export failures before interpreting planner output.
5. Run `onwardpg status` and `onwardpg history status`. If history is uninitialized, explain what `onwardpg init` will establish and obtain the user's intent before creating a baseline in an established project.

## Keep one feature plan alive

Start a new feature once:

```sh
onwardpg plan descriptive-feature-name
```

After every schema edit, answered decision, SQL edit, branch return, or rebase, revise the same active PlanID:

```sh
onwardpg plan
```

Do not stack local fixup migrations. Git moves the feature; rerunning `plan` restacks the same feature bundle on the currently accepted history and carries forward only decisions whose scoped meaning still holds.

`plan`, `verify`, and `init` run the schema exporter two times: at the start, and again immediately before the result. Run only one of them at a time in a checkout. Other work can run in the same checkout at the same time: onwardpg does not stop when a file changes. Two things still stop a command, because they change the exporter output between the two runs: an edit to the schema sources, and a new build of a package that the exporter imports. The command then ends with an error (`the configured schema export changed while the command worked` or `DDL export is nondeterministic`); rerun it after the edit or the build is complete. A result can have a `warnings` entry with the code `export_side_effects` and a list of paths: the git status of those paths changed while the command ran. It does not change the status or the exit code. If the exporter wrote those files, change the exporter so that it writes only to standard output; if another process wrote them, ignore the warning. If a command is slow, run it with `ONWARDPG_TIMINGS=1` and read the per-stage JSON line on standard error before you change anything.

Use JSON output, which is the default. Follow the high-level `status`, ordered
`next_actions`, nested decision choices, named edit requirements, and exit
codes rather than guessing. A `workspace_fast_forward` action contains direct
D -> W SQL plus exact argv; it is never the durable H -> W bundle. Use
`durable.status` as the effective bundle state. `durable.generated_plan` is
the raw generator result before edit reconciliation and may retain
`needs_sql_edits` as provenance after the edited artifact verifies.
Inspect `next_actions` even when the top-level status is `ready`: that state
means the durable artifact is ready, while caller-owned development SQL remains
an explicit optional action.
Lower-level `draft` reports additionally provide `next_action`:

- `0`: review the report and SQL; continue or verify.
- `2`: supply justified intent or edit only the named SQL pocket, then rerun.
- `3`: stop on unsupported catalog state; change the design, narrow an explicit ignore, or report the gap.
- `4`: repair stale history, receipts, residual differences, or clone evidence before continuing.
- `1`: fix invocation, configuration, export, connection, or environment errors.

See [the decision protocol](references/decision-protocol.md) for hints, SQL handoffs, branch switching, and rebases.

## Answer only what the repository proves

Hints contain semantic intent, never SQL or opaque planner fingerprints. An agent that already knows a rename or manual conversion strategy may provide it on the first invocation with `--hint` or `--hints-file`. onwardpg consumes only hints that match real dependency-ordered decisions and rejects stale, contradictory, impossible, or unused guesses.

Keep durable `--hint` decisions separate from local-only `--dev-hint` decisions. A messy development database is convenience evidence, not migration history.

For product-specific transformations:

1. Read all application paths that read or write the affected values.
2. Use bounded production aggregates or value-shape classifications only when the user has provided restricted read-only access.
3. Record any live precondition that must be rechecked during deployment.
4. Edit the named `expand.sql` or `contract.sql` pocket with reviewed SQL.
5. Resolve any planner-named Boolean contract-gate pocket in `contract.sql`;
   that assertion is repeated at production enforcement time. Separately add
   optional read-only assertions to `verify.sql` for synthetic `WITH ... VALUES
   (...)` conversion examples or clone-only postconditions. Do not insert
   fixtures into deployment phases or treat `verify.sql` as production
   readiness evidence.
6. Rerun `onwardpg plan` when the declarative schema changes, then run `onwardpg verify` against the exact edited bundle.

## Development databases and branches

`onwardpg plan --output sql` emits only direct development reconciliation (`D -> W`). It is never the PR bundle. In workspace mode, preserve development-only objects that may belong to another branch.

When ordinary plan output contains a `workspace_fast_forward` action, inspect
its `preserved` objects and reason. `accepted_history_changed` means a rebase
left D behind the new accepted head. The included SQL is available immediately;
the argv renders the same stream for optional execution and repeats any
consumed ephemeral dev hints. Never copy D-only
objects into the durable plan, and stop if the development report asks a
decision or reports an incompatibility.

If a branch switch removes the active bundle from the checkout, name the returning or other plan explicitly. Let onwardpg park or restore worktree-local PlanIDs. If it reports an active-plan conflict, stop and resolve which feature is present instead of deleting anchors or fabricating another plan.

## Finish with an evidence handoff

Run:

```sh
onwardpg verify
onwardpg status
```

Report:

1. the PlanID and bundle path;
2. the H -> W compatibility strategy and application deployment assumption;
3. every consumed semantic decision and its code or product evidence;
4. edited phase SQL and synthetic verification assertions;
5. verification outcome and residual diff;
6. hazards, unsupported objects, or unanswered decisions;
7. live preconditions and operational gates still owned by the deployment system.

If the user is operating a post-expand deployment, validate provider-neutral,
expiring evidence for every potential writer cohort and run the read-only gate:

```sh
onwardpg contract check \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json
```

Report `ready`, `needs_evidence`, `blocked`, `stale`, or `unsupported`; never apply phase SQL.

## Check a live database

Use these commands only with a restricted read-only URL that the user provides
in an environment variable. Never put a database URL in command arguments, a
file, or a report.

```sh
onwardpg drift check --database-env PROD_READONLY_DATABASE_URL
onwardpg diff --from-env PROD_READONLY_DATABASE_URL --to-env DESIRED_DATABASE_URL --target NAME
```

`diff` reads each side from a database. `DESIRED_DATABASE_URL` names a
disposable local database that holds the desired schema. A SQL file is also a
valid side, as `file://PATH` together with `--dev-url`.

`drift check` compares the live catalog with the replayed accepted history.
The replay is the one that `verify` uses, so history that holds
`CREATE INDEX CONCURRENTLY` or `DROP INDEX CONCURRENTLY` batches from
`plan --concurrent-indexes` replays without a change to the bundles. Do not
plan without `--concurrent-indexes` to make a check pass.

The role in the URL can be the database owner, a login role that only inherits
`pg_read_all_data` (on PlanetScale: `pscale role create DATABASE BRANCH NAME
--inherited-roles pg_read_all_data --ttl 24h`), or a login role with no
membership and no grants: `drift check` reads system catalogs only. The result
names the kind in `observer.mode`. The role can have `BYPASSRLS`
(`observer.bypass_rls`), which a team needs when row-level security hides rows
from a reader. A role that can write, create, or administer is refused before
the history replay with `drift_observer_role_elevated` or
`drift_observer_access_policy_unsafe`; the message names each privilege, and a
grant to `PUBLIC` counts. Give the user the SQL in `next_actions`.
`contract check` also reads rows, so its role needs `pg_read_all_data` or
`USAGE` and `SELECT` grants, and `BYPASSRLS` when policies filter its rows.

Read `status`, the exit code, and every list in the result:

- `drift_free` (exit 0): no modeled difference and nothing the planner cannot
  handle.
- `drifted` (exit 4): `differences` names each object. Between a bundle's
  expand and its contract, the live catalog differs from the replayed history
  on purpose. Expect only what the pending contract changes: the objects it
  drops, and the final state it sets, such as `NOT NULL` or a validated
  constraint.
- `unsupported` (exit 3): `unsupported` lists catalog state that the planner
  does not model, from the live database, the replayed history, or both.
  `differences` is still reported. Do not treat this result as a drift verdict.

A managed provider owns some live state: an extension or a schema that its
admin role owns, and parameter grants to its roles. That state is listed in the
target's `live_ignore` in `.onwardpg.toml`, as exact selectors copied from an
`unsupported` report (`ownership:extension:NAME=ROLE`,
`ownership:schema:NAME=ROLE`, `parameter_acl:NAME`). It applies only to
`drift check`, `contract check`, and `diff --target`; it never changes `plan`,
`verify`, or `init`. It filters the live side only, so it cannot clear a
selector that comes from the replayed history; fix that one in the declared
schema. Report what was ignored: `drift check` and `contract check` give
`observer.live_ignored` and `observer.live_ignore_unmatched`; `diff --target`
gives `live_ignored:SELECTOR` and `live_ignore_unmatched:SELECTOR` entries in
`workspace_compatibility`, and gives neither when it stops for a decision. An
unmatched entry acknowledges nothing: it is a typo or a name that needs quotes.
Add or change a `live_ignore` entry only with the user's approval, because a
parameter entry also hides a later grant of that parameter to another role.

Provider objects that the planner does model, such as an extra extension, still
show as differences. Pass `--ignore SELECTOR` for those, and report the list.

A result never authorizes a repair. Resolve drift through a reviewed bundle or
a deliberate change to the declared schema.

## Renames

A constraint or index that differs from the desired schema only in its name is
offered as a rename decision. A foreign key whose referenced primary-key or
unique constraint is renamed in the same plan is offered with it; answer the
key and the foreign key together. Confirm a rename only when the definitions
are the same object.

- Constraint: a confirmed rename emits `ALTER TABLE ... RENAME CONSTRAINT`, a
  metadata change. A declined one becomes a drop and a new constraint, with a
  new validation.
- Standalone index: a confirmed rename emits `ALTER INDEX ... RENAME TO`, a
  metadata change. A declined one builds the index again.

Do not describe the migration as safe merely because clone verification passed.
