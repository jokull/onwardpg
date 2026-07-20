---
title: Contract readiness
description: Prove the post-expand catalog, overlap data, and every potential writer before restoring enforcement.
---

Expand may deliberately accept more states than either endpoint schema. That is
what lets legacy and new code overlap. Contract may tighten that envelope only
after two different facts are true:

1. overlap rows satisfy the desired invariant; and
2. every potential legacy writer is upgraded, drained, isolated, or read-only.

`onwardpg verify` cannot establish either production fact. It executes the exact
bundle on disposable PostgreSQL, proves convergence, and receipts the catalog
it observed immediately after expand. `onwardpg contract check` uses that
checkpoint later without applying SQL to the caller database.

## The planner captures the repair decision

Suppose the old CHECK accepts `shared | legacy` and the new CHECK accepts
`shared | new`. Neither constraint is a safe overlap. Expand removes the old
one so both application releases can write. The planner then asks how contract
will reconcile rows admitted during that window:

- `assert_only` runs the exact generated assertion when no repair is expected;
- `manual_sql` captures reviewed cleanup plus the required Boolean contract gate;
  or
- `split_plan` retains the loose schema and restores enforcement after another
  deployment.

Where PostgreSQL supports `NOT VALID`, contract fences new writes before repair:

```sql
ALTER TABLE "app"."delivery"
  ADD CONSTRAINT "delivery_tier_check"
  CHECK (tier IN ('shared', 'new')) NOT VALID;

UPDATE "app"."delivery"
SET tier = 'new'
WHERE tier = 'legacy';

DO $onwardpg$
BEGIN
  IF NOT COALESCE((
    SELECT NOT EXISTS (
      SELECT 1 FROM "app"."delivery"
      WHERE (tier IN ('shared', 'new')) IS FALSE
    )
  ), false) THEN
    RAISE EXCEPTION 'onwardpg contract gate failed: data:...';
  END IF;
END
$onwardpg$;

ALTER TABLE "app"."delivery"
  VALIDATE CONSTRAINT "delivery_tier_check";
```

`IS FALSE` preserves PostgreSQL CHECK semantics: NULL is accepted. Foreign-key
gates use typed ordered columns and MATCH SIMPLE/FULL behavior. Supported
btree uniqueness preserves key expressions, predicates, collations, and NULL
semantics. Shapes onwardpg cannot prove exactly require reviewed Boolean SQL or
a split plan.

For those manual shapes, the generated assertion is a bounded edit pocket in
`contract.sql`. Edit that SQL, not the gate metadata. Successful verification
writes the resolved query to `contract-gate-overrides.json`, bound to the
generated gate ID and history entry. Optional `verify.sql` assertions remain
clone-only semantic examples or postconditions; they do not authorize live
contract.

Unique and exclusion builds have no general `NOT VALID` fence. Those
transitions also require a write-fence attestation so a concurrent writer cannot
reopen a conflict between the exact probe and enforcement build.

## Check production read-only

```sh
export PROD_READONLY_DATABASE_URL='postgres://...'

onwardpg contract check \
  --target app \
  --bundle delivery-tier \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json \
  --statement-timeout 30s
```

The result is `ready`, `needs_evidence`, `blocked`, or `stale`. In one
`REPEATABLE READ, READ ONLY` transaction it:

1. validates the bundle and requires it to be the history head;
2. compares production with the receipted post-expand graph;
3. evaluates exact read-only data gates; and
4. validates expiring writer evidence bound to the environment, PlanID,
   generation, history entry, and release.

Writer evidence covers web deployments, workers, schedules, queues and retries,
connection pools, previews, and ad-hoc writers. A scaled-to-zero preview with
production write credentials is still a potential writer: it can reconnect.
Zero current sessions is therefore not drain evidence.

`ready` is operational feedback, not permission that survives forever.
Contract SQL repeats its data assertion after cleanup, immediately before
enforcement. Your release system still owns batch execution, lock budgets,
replica health, approval, and the final cutover.
