# Forward-only migration workflow

onwardpg produces one evolving, reviewable forward migration per feature. It
plans and clone-verifies; it never applies to a caller-owned database and never
generates down migrations.

## The normal path

```sh
onwardpg init
onwardpg plan add-booking-dates

# Answer only an ambiguity that schema state cannot prove.
onwardpg plan \
  --hint '{"kind":"rename","object":"column","from":["app","booking","when"],"to":["app","booking","starts_at"]}'

# Repeat as the feature or accepted base changes.
onwardpg plan

# Review exact bytes in disposable PostgreSQL clones.
onwardpg verify
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
| Expand | additive shape, compatibility views/triggers, constraint relaxation, explicitly chosen backfill work | run while pre-deployment code is still live, before one application rollout |
| Contract | assertions, validation, enforcement, compatibility cleanup | run after pre-deployment instances and writers have drained |

Comments identify transactional boundaries, required non-transactional batches,
locking/rewrite hazards, and sequencing. Review the files as deployment SQL;
their application remains the responsibility of the person or system with
deployment visibility.

Backfill is work, not a third deployment phase. Synchronization belongs in
expand when it is safe with old code live. Expand may intentionally enforce
less than either endpoint schema when that is necessary for both writers to be
accepted. Desired tightening, assertions, and restoration of temporarily
removed enforcement belong in contract after old writers drain. Either file
may contain multiple explicit transactional and non-transactional batches.

One bundle surrounds exactly one application deployment. The new application
must work before and after contract. If it cannot, keep an intermediate shape
in this feature and remove the old contract in a later feature plan.

## Development without pretending it is history

When a target has `dev_database_env`, each `plan` also compares developer DB D
with W. Use this only to bring local development forward:

```sh
onwardpg plan --output sql | psql "$DEV_DATABASE_URL"
```

The command itself does not pipe or apply the SQL. In the default workspace
mode, objects that exist in D but not W are preserved rather than proposed for
drop. That makes branch switching and long-lived test data safe enough to be
useful. The SQL is D → W, not the PR bundle; it does not prove that local state
followed accepted migration order.

A common feature-iteration case is different from a production rename. Suppose
an unmerged plan added `quote_mode` and the developer applied it to D, then the
code schema changed the name to `pricing_mode`. H never contained either name,
so regenerating the durable bundle collapses directly to `ADD pricing_mode`.
D does contain `quote_mode`, so development reconciliation asks a dev-scoped
rename question. Confirming it emits one direct local `ALTER TABLE ... RENAME
COLUMN`; choosing `preserve` adds the new column and keeps the old local object.
The durable bundle still uses the rolling-safe strategy whenever an accepted
production column is genuinely being renamed.

When development reconciliation has a rename, cast, or destructive ambiguity,
rerun the same command with `--dev-hint`. Those answers are scoped to D → W and
never authorize the durable H → W bundle. Workspace mode intentionally
preserves absence-only objects instead; a deliberately disposable `strict`
development database may plan their confirmed removal.

If incoming history puts a new declarative column before columns already
appended in D, PostgreSQL cannot reproduce that physical order with `ALTER
TABLE`. onwardpg adds the required column at the reachable end and reports the
physical-order difference as compatibility evidence. Column order remains in
the typed catalog inventory, but does not block semantic convergence.

## Restacking without Git coupling

Git tells the developer that `main` moved. onwardpg validates the actual
accepted migration history and selected PlanID. After rebase, repeat
`onwardpg plan`. The tool replays the newly accepted chain and
regenerates the feature bundle on top of it. It does not inspect branch names,
merge bases, or commit SHAs as correctness facts.

Agent-authored phase edits are retained only as explicit receipts. Generated
TODOs have stable edit-pocket markers; bytes inside those markers can be
transplanted while surrounding generated SQL refreshes. An edit outside a
pocket conflicts when the generator also changes that phase, and onwardpg
returns all three versions for review.

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
onwardpg drift check --database "$PROD_DATABASE_URL"
```

It compares observed P with W and reports drift. A finding is evidence to
investigate, not permission for onwardpg to make production changes. Resolve it
through a reviewed forward bundle or a deliberate declared-schema correction.

`init` establishes a logical replay baseline from exported DDL; it does not
adopt physical object-name aliases from a caller-owned environment. A legacy
foreign-key name that differs only because an exporter changed its deterministic
naming algorithm is therefore visible to `drift check`, never silently
normalized. If a future feature must change that legacy constraint, make the
physical-to-declarative transition explicit in reviewed bundle SQL, including
the names necessary for clone verification and the real environment.

## Lower-level compatibility commands

`onwardpg draft`, `onwardpg dev plan`, and `onwardpg history status` retain the
earlier explicit history-head interface for automation and debugging. They are
not the recommended authoring loop: use `plan`, `status`, and `verify` unless
you specifically need that lower-level control.
