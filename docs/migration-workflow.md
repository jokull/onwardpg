# Forward-only migration workflow

onwardpg produces one evolving, reviewable forward migration per feature. It
plans and clone-verifies; it never applies to a caller-owned database and never
generates down migrations.

## The normal path

```sh
onwardpg init --target primary
onwardpg plan add-booking-dates --target primary

# Answer only an ambiguity that schema state cannot prove.
onwardpg plan --target primary \
  --hint '{"kind":"rename","object":"column","from":["app","booking","when"],"to":["app","booking","starts_at"]}'

# Repeat as the feature or accepted base changes.
onwardpg plan --target primary

# Review exact bytes in disposable PostgreSQL clones.
onwardpg verify --target primary
```

The first command establishes the ground floor. `plan NAME` starts a
worktree-local PlanID and a matching durable bundle. Every later `plan` refresh
replaces the same unexecuted logical bundle. It is therefore natural to run
after a schema edit, after resolving an ambiguity, and after rebasing onto new
accepted history.

The durable migration starts at replayed accepted history (H), not at a
developer database. Its destination is current exported CREATE-statement DDL
(W). This provides the PR-level guarantee: accepted history plus the reviewed
bundle converges to W in a clean clone.

## Deployment phases

Generated operations are classified and rendered into readable phase files:

| Phase | Typical work | Deployment meaning |
| --- | --- | --- |
| Expand | additive columns, compatible tables/indexes, `NOT VALID` constraints | run before code that needs the new shape |
| Migrate | data movement, backfills, validation, agent-edited product SQL | run while old and new application paths coexist |
| Contract | destructive cleanup and final restrictions | delay until compatibility window is complete |

Comments identify transactional boundaries, required non-transactional batches,
locking/rewrite hazards, and sequencing. Review the files as deployment SQL;
their application remains the responsibility of the person or system with
deployment visibility.

## Development without pretending it is history

When a target has `dev_database_env`, each `plan` also compares developer DB D
with W. Use this only to bring local development forward:

```sh
onwardpg plan --target primary --output sql | psql "$DEV_DATABASE_URL"
```

The command itself does not pipe or apply the SQL. In the default workspace
mode, objects that exist in D but not W are preserved rather than proposed for
drop. That makes branch switching and long-lived test data safe enough to be
useful. The SQL is D → W, not the PR bundle; it does not prove that local state
followed accepted migration order.

If a deliberately disposable `strict` development database has its own
rename, cast, or destructive ambiguity, rerun the same command with
`--dev-hint`. Those answers are scoped to D → W and never authorize the
durable H → W bundle. Workspace mode intentionally preserves absence-only
objects instead.

If incoming history puts a new declarative column before columns already
appended in D, PostgreSQL cannot reproduce that physical order with `ALTER
TABLE`. Workspace output adds the required column at the reachable end and
reports `workspace_compatibility`; clone verification remains exact.

## Restacking without Git coupling

Git tells the developer that `main` moved. onwardpg validates the actual
accepted migration history and selected PlanID. After rebase, repeat
`onwardpg plan --target primary`. The tool replays the newly accepted chain and
regenerates the feature bundle on top of it. It does not inspect branch names,
merge bases, or commit SHAs as correctness facts.

Agent-authored phase edits are retained only as explicit receipts. On a base
change, onwardpg inventories generated structural phases separately from
`migrate`/`manual`, agent-authored, and assertion files. Files whose effects
cannot be established from catalog state are marked for review rather than
silently replayed into development.

For a narrow extra signal, an accepted Boolean assertion may include
`-- onwardpg:dev-postcondition`. `plan` evaluates only that opt-in assertion
against D under a read-only PostgreSQL transaction and reports its evidence.
It never runs upstream phase SQL on D.

In a single checkout, switching branches may make the current bundle disappear.
Use `plan NAME` for the branch you entered. The local anchor parks the absent
plan and restores it by bundle name when you return. It refuses to create a
second mutable plan while the previous bundle remains present.

## Drift is a separate health check

Run this when it is operationally useful, not as a requirement of every PR:

```sh
onwardpg drift check --target primary --database "$PROD_DATABASE_URL"
```

It compares observed P with W and reports drift. A finding is evidence to
investigate, not permission for onwardpg to make production changes. Resolve it
through a reviewed forward bundle or a deliberate declared-schema correction.

## Lower-level compatibility commands

`onwardpg draft`, `onwardpg dev plan`, and `onwardpg history status` retain the
earlier explicit history-head interface for automation and debugging. They are
not the recommended authoring loop: use `plan`, `status`, and `verify` unless
you specifically need that lower-level control.
