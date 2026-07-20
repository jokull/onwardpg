# Restricted production evidence

Production evidence can inform a migration without granting migration authority.

For a `text -> integer` transition, a catalog cannot reveal whether live values contain blanks, whitespace, signs, decimals, or malformed strings. If the user has explicitly provided read-only MCP access to a production replica or a tightly restricted role, prefer aggregate classifications:

```sql
SELECT
  count(*) AS rows,
  count(*) FILTER (WHERE age IS NULL) AS nulls,
  count(*) FILTER (WHERE age IS NOT NULL AND trim(age) = '') AS blanks,
  count(*) FILTER (
    WHERE age IS NOT NULL
      AND trim(age) <> ''
      AND trim(age) !~ '^[0-9]+$'
  ) AS non_numeric,
  min(length(age)) AS shortest,
  max(length(age)) AS longest
FROM app.accounts;
```

Do not select or copy representative raw rows when shape counts answer the question. Record a live precondition such as `non_numeric = 0` for a deployment preflight because disposable verification cannot prove that it remains true.

Translate observed classes into synthetic semantics:

```sql
-- onwardpg:assert age_conversion_examples
WITH examples(raw, expected) AS (
  VALUES
    ('42'::text, 42::integer),
    (' 7 '::text, 7::integer),
    (''::text, NULL::integer),
    ('   '::text, NULL::integer),
    (NULL::text, NULL::integer)
)
SELECT bool_and(
  NULLIF(trim(raw), '')::integer IS NOT DISTINCT FROM expected
)
FROM examples;
```

The production query discovers categories and live preconditions. The synthetic
assertion tests the product rule repeatably. Edited phase SQL performs the
migration work. After expand, `onwardpg contract check` compares the live
catalog with the receipted checkpoint and evaluates exact contract data gates.
The release system still owns volume, lock budgets, replica health, execution,
and drain evidence.

Drain evidence must cover potential writers, not only active sessions: web,
workers, schedules, queues and retries, connection pools, previews, and ad-hoc
writers. A scaled-to-zero preview with production write credentials can return
and therefore remains active potential unless upgraded, drained, isolated, or
made read-only. Evidence is expiring and bound to the exact plan, bundle entry,
environment, and release.

## Access boundary

Use a dedicated role with only `CONNECT` and `SELECT` on approved schemas, views, or columns. Enforce read-only transactions, statement timeouts, row limits, network restrictions, and audited access.

Never give a coding agent:

- onwardpg's scratch administrator;
- production or staging write credentials;
- secret-management access unrelated to the task;
- unrestricted access to sensitive columns; or
- permission to copy live rows into the repository or conversation.
