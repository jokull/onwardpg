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

Use JSON output, which is the default. Follow the high-level `status`, nested
decision choices, named `edit_files`, and exit codes rather than guessing.
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

Report `ready`, `needs_evidence`, `blocked`, or `stale`; never apply phase SQL.

Do not describe the migration as safe merely because clone verification passed.
