---
title: Expand and contract
description: One migration bundle surrounds exactly one application rollout.
---

The compatibility window is the product:

```text
old app + old schema
        |
        v
      EXPAND  <- safe while old writers and readers exist
        |
        v
old app + expanded schema
        |
        v
      DEPLOY  <- new app understands both schema contracts
        |
        v
new app + expanded schema
        |
        v
       DRAIN  <- prove old traffic is gone
        |
        v
     CONTRACT <- validate, tighten, remove compatibility
```

## Example: add a required column

The end state may be simple:

```sql
CREATE TABLE app.bookings (
  id bigint PRIMARY KEY,
  status text NOT NULL
);
```

Adding it as required in one operation can break old inserts. A compatible
route starts with the loose shape:

```sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;
```

That is all the generated expand phase contains for this change. Legacy code
can still insert a booking without `status`, including after expand has run.
A one-time backfill in expand would therefore race those writers and could not
establish the `NOT NULL` precondition.

The first plan asks whether contract should merely assert, run reviewed cleanup,
or stay loose for another deployment. Choosing `manual_sql` creates one
application-owned cleanup pocket and an exact generated assertion:

```sql
-- contract.sql
-- onwardpg:edit begin stmt-sha256-1a5377b536479569445c7585eb95560c9e977f5d696557db5f28031f9789eec6
-- PRODUCT-SPECIFIC SQL: Provide reviewed reconcile_contract_sql SQL for app.bookings.status
-- Verify: SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL);
-- ONWARDPG TODO: replace this comment with reviewed SQL for reconcile_contract_sql on app.bookings.status.
-- Planner analysis: Supply reviewed post-drain cleanup/backfill SQL and at least one read-only Boolean verification query for column:app:bookings:status.
-- Expected effect: complete the named operation and converge to the desired catalog state.
-- onwardpg:edit end stmt-sha256-1a5377b536479569445c7585eb95560c9e977f5d696557db5f28031f9789eec6

-- onwardpg:batch transactional
-- Batch batch-contract-002: transactional.
-- Review: safety=review; hazards=contract_data_assertion,table_scan_possible; requires_gates=writers:legacy.
-- Suggested session timeouts: statement_timeout=20m, lock_timeout=3s.
DO $onwardpg$ BEGIN IF NOT COALESCE((SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL)), false) THEN RAISE EXCEPTION 'onwardpg contract gate failed: data:c6703912502bd497'; END IF; END $onwardpg$;
-- Review: safety=review; hazards=table_scan,access_exclusive_lock,compatibility_removal; requires_gates=data:c6703912502bd497,writers:legacy.
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
```

For a product whose correct historical value is `pending`, the developer can
replace only that pocket:

```sql
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
```

The generated contract assertion remains between that cleanup and enforcement.
The same postcondition can also live in `verify.sql`, so disposable verification
names the product assumption independently:

```sql
-- onwardpg:assert booking_status_present
SELECT NOT EXISTS (
  SELECT 1
  FROM "app"."bookings"
  WHERE "status" IS NULL
);
```

Deploy code that writes and reads `status`, then drain every legacy writer.
`onwardpg verify` executes those exact edited bytes on disposable PostgreSQL,
accepts them only after they reach the requested schema, and `verify --check`
proves the verified files have not drifted. It also records the graph observed
after expand. After the application deploy, `onwardpg contract check` can
compare production with that checkpoint and require live data plus writer
evidence. The deployment system still owns execution and approval. See
[contract readiness](/concepts/contract-readiness/).

## The one-deployment rule

The new application version must work immediately after expand and still work after contract. If removing the old shape requires another code release, keep the compatibility object and choose a split plan. The later feature owns its removal.

Backfills are not a third deployment phase. They are explicit work scheduled where old and new contracts remain safe. Contract should never be used as a vague container for “do risky things later.”

The same envelope can span dependencies. An eligible same-type column rename
keeps its generated two-way bridge even when a dependent view needs human
semantics. In that case onwardpg adds bounded pockets for an expand view that
keeps the legacy output prefix, removal before the contract cutover, and exact
desired recreation afterward. If name and type change together, the confirmed
transition uses two broader pockets—one per phase—that own both columns and
the full view, materialized-view, and index closure. The planner never invents
the cast, conflict policy, or freshness rule, and it never emits an illegal
`CREATE OR REPLACE VIEW` output rename alongside the handoff.
