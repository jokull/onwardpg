---
title: CLI reference
description: The preferred onwardpg authoring, verification, development, and drift commands.
---

All commands emit JSON by default. Use `--target NAME` when a repository configures more than one target.

## `config check`

```sh
onwardpg config check
```

Validate config and database boundaries, deterministically compile desired DDL, materialize it in disposable PostgreSQL, and verify accepted history.

## `init`

```sh
onwardpg init
```

Create the first clone-verified history entry. It refuses a non-empty target history.

## `plan`

```sh
onwardpg plan [NAME] \
  [--hint JSON] [--hints-file FILE] \
  [--dev-hint JSON] [--dev-hints-file FILE] \
  [--output json|text|sql]
```

Start or revise one worktree-local active migration. The durable comparison is accepted history to working DDL. When a development database is configured, `--output sql` prints a separate direct development reconciliation; onwardpg does not apply it.

Exit status `2` means the plan needs a semantic decision or editable SQL, not that onwardpg guessed a result.

## `status`

```sh
onwardpg status
```

Report the local active PlanID, selected bundle, and whether its parent is current. This is Git-independent and does not contact PostgreSQL.

## `verify`

```sh
onwardpg verify [--bundle NAME]
onwardpg verify --check
onwardpg verify --through expand
```

With no `--bundle`, verify selects the worktree's active plan. It replays exact history and bundle bytes in disposable databases, runs assertions, and proves catalog convergence. `--check` is the read-only CI gate; partial verification proves both the prefix and its continuation without claiming either ran in a real environment. Positional bundle names are rejected.

## `contract check`

```sh
onwardpg contract check \
  [--target NAME] \
  [--bundle NAME] \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  [--evidence deploy-readiness.json] \
  [--statement-timeout 30s] [--config .onwardpg.toml]
```

Require the selected bundle to be the history head, compare the caller database
with its receipted post-expand graph, evaluate exact data gates, and validate
expiring writer evidence. The connection runs one repeatable-read, read-only
snapshot; this command has no production migration execution path. Its
`onwardpg.contract-readiness/v1` report carries bundle/PlanID identity,
expected and observed fingerprints, gate results, findings, and a digest.
`--statement-timeout` defaults to 30 seconds. See
[contract readiness](/concepts/contract-readiness/).

## `drift check`

```sh
onwardpg drift check --database "$PRODUCTION_DATABASE_URL"
```

Inspect the supplied database read-only and compare it with accepted desired state. A finding is evidence to investigate, never permission for onwardpg to change production.

## `dev plan`

```sh
onwardpg dev plan --output text
```

Lower-level development reconciliation from the read-only dev catalog to working DDL. Workspace mode preserves absence-only local objects to make branch switching less destructive.

For the full preview protocol and lower-level `draft`, `history status`, selectors, and planner flags, see the repository’s canonical `docs/cli.md`.
