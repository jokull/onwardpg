---
title: Your first plan
description: Establish history, evolve desired DDL, answer ambiguity, and verify a bundle.
---

:::tip[Pairing with a coding agent?]
Copy this into the task before editing models or DDL:

```text
Read and follow https://onwardpg.solberg.is/skill.md. Create or revise one
onwardpg plan for this feature, follow its status, decisions, and edit_files,
edit only named SQL pockets, verify the exact bundle, and summarize unresolved hazards
and live deployment gates for human review.
```
:::

Assume your desired schema begins with:

```sql
CREATE SCHEMA app;

CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  display_name text NOT NULL
);
```

## Establish replayable history

```sh
onwardpg config check
onwardpg init
```

`init` creates and clone-verifies the first content-addressed history entry. It does not apply anything to your development or production database.

## Evolve desired DDL

Add a nullable status to the authoritative schema:

```sql
ALTER TABLE app.accounts
  ADD COLUMN status text;
```

In a declarative schema file, write the resulting complete `CREATE TABLE` shape instead of the `ALTER` above. Then plan one feature:

```sh
onwardpg plan add-account-status
```

This creates one worktree-local `PlanID`. The durable bundle compares accepted history replayed in PostgreSQL (H) with the current exported schema materialized in PostgreSQL (W). If a development database is configured, the same command also reports a separate D → W reconciliation; it never uses D as migration history.

For this nullable additive change, the generated bundle appears under the
configured `bundle_root` as:

```text
migrations/onward/app/add-account-status/
├── manifest.json
├── plan.json
└── phases/
    └── expand.sql
```

Only non-empty phases exist. Decision receipts appear after hints are consumed;
`verify.sql` appears when you add assertions. A required column, rename bridge,
or other compatibility change may also generate `contract.sql` and editable
pockets.

Review every statement and hazard. Continue editing your models or DDL and rerun `onwardpg plan`; the same active feature bundle is recalculated rather than stacked into a trail of local fixups. After rebasing on newly accepted migrations, run it again: the same `PlanID` is restacked on the new replayed head.

## Prove the bundle

```sh
onwardpg verify
```

The command selects the active plan. In a clean checkout without that local
anchor, use `onwardpg verify --bundle add-account-status`.

Verification independently replays the chosen checkpoint and its continuation, runs boolean assertions, compares the final graph fingerprint, and requires an empty residual diff.

Use the read-only CI form after the bundle is reviewed:

```sh
onwardpg verify --check
```

If the high-level plan reports `needs_input` or `needs_sql_edits`, the bundle is intentionally incomplete. Supply one of the emitted semantic hints or replace the TODOs in the reported `edit_files`, then rerun `plan` and verify. The nested lower-level `draft` report uses `needs_decisions` for its compact decision envelope.
