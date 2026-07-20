---
title: Decisions and manual work
description: How onwardpg handles intent and product semantics that catalogs cannot prove.
---

Two schema snapshots show facts, not intent. If `display_name` disappears while `full_name` appears, that could be a rename, a replacement with different meaning, or two unrelated changes. onwardpg stops and prints a semantic hint.

## Confirm a rename

```sh
onwardpg plan rename-display-name \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}'
```

For an eligible same-type rename, the planner can create a temporary second column and deterministic dual-write bridge. Existing rows still need an explicit strategy: reviewed manual SQL, an acknowledged single transaction for a known-small table, or a split plan.

## Change `text` to `integer`

The catalog proves the types differ. It cannot decide whether an empty string becomes `NULL`, fails, or maps to a product value.

After the `type_change` hint selects `manual_sql`, actual generated output
contains two blocking pockets:

```sql
-- expand.sql
-- ONWARDPG TODO: replace this comment with reviewed EXPAND SQL for column:app:accounts:age (text -> integer).
-- Establish both old and new interfaces, synchronization/conflict behavior, and any initial backfill while old code is live.
-- Do not use a direct ALTER TYPE here: this plan surrounds one rolling application deployment.

-- contract.sql
-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for column:app:accounts:age (text -> integer).
-- After pre-deployment writers drain, perform final catch-up/assertions, remove compatibility objects, and converge to PostgreSQL type integer.
```

The developer supplies a real compatibility bridge in those pockets. The
conversion rule inside that bridge might be
`NULLIF(trim(age), '')::integer`; a bare in-place `ALTER TYPE` would not keep
both application contracts available during a rolling deployment. Add
assertions for every data-dependent assumption:

```sql
-- onwardpg:assert account_age_is_convertible
SELECT NOT EXISTS (
  SELECT 1
  FROM app.accounts
  WHERE age IS NOT NULL
    AND trim(age) !~ '^[0-9]+$'
);
```

`manual_sql` hints never carry SQL. The editable file is the reviewed artifact, and its exact bytes become receipted only after clone verification succeeds.
The complete generated phase files are black-box CLI receipts under
[`docs/receipts/type-change`](https://github.com/jokull/onwardpg/tree/main/docs/receipts/type-change).

## Why decisions expire

Consumed hints are captured automatically in `decisions.json`; developers and agents do not copy internal fingerprints or resubmit accepted answers. Answers are bound to the exact participating graph scope. Unrelated schema erosion preserves them, while a later edit that changes their meaning makes them stale. This prevents a previous approval for one destructive shape from silently authorizing another.

An agent that already understands the code change may provide several hints on the first `plan` call. onwardpg consumes only those that match real decisions and rejects an unused guess. See [agent-assisted planning](/agents/agent-assisted-planning/).
