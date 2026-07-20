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

Adding it as required in one operation can break old inserts. A compatible route is:

The blocks below are excerpts from the checked-in black-box CLI receipt for
this exact schema. The full generated files are regenerated on disposable
PostgreSQL in CI.

<!-- onwardpg-receipt: required-column-expand-statement -->
```sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;
```

That is all the generated expand phase contains for this change. Legacy code
can still insert a booking without `status`, including after expand has run.
A one-time backfill in expand would therefore race those writers and could not
establish the `NOT NULL` precondition.

Deploy code that writes and reads `status`, then drain every legacy writer.
The generated contract deliberately stops at an application-owned edit pocket:

<!-- onwardpg-receipt: required-column-generated-contract-pocket -->
```sql
-- contract.sql
-- onwardpg:edit begin stmt-sha256-8efef1a10c145b891fc9259a7434ccab32bd75200e48130b10f6fbc37776f899
-- ONWARDPG TODO: deploy code that writes column:app:bookings:status, then replace this comment with a reviewed backfill for existing rows.
-- Expected effect: every row has a product-correct value before the NOT NULL contract runs.
-- Add a boolean assertion to verify.sql proving no NULL values remain.
-- onwardpg:edit end stmt-sha256-8efef1a10c145b891fc9259a7434ccab32bd75200e48130b10f6fbc37776f899
-- Review: safety=review; hazards=table_scan,access_exclusive_lock,compatibility_removal.
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
```

For a product whose correct historical value is `pending`, the developer can
replace only that pocket:

<!-- onwardpg-receipt: required-column-edited-contract-pocket -->
```sql
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
```

and add the receipt's Boolean verification query:

<!-- onwardpg-receipt: required-column-verification -->
```sql
-- onwardpg:assert booking_status_present
SELECT NOT EXISTS (
  SELECT 1
  FROM "app"."bookings"
  WHERE "status" IS NULL
);
```

`onwardpg verify` executes those exact edited bytes on disposable PostgreSQL,
receipts them only after convergence, and `verify --check` proves the receipt
has not drifted. The deployment system still owns the real drain gate before
contract. See the source fixtures in
[`docs/receipts/required-column`](https://github.com/jokull/onwardpg/tree/main/docs/receipts/required-column).

## The one-deployment rule

The new application version must work immediately after expand and still work after contract. If removing the old shape requires another code release, keep the compatibility object and choose a split plan. The later feature owns its removal.

Backfills are not a third deployment phase. They are explicit work scheduled where old and new contracts remain safe. Contract should never be used as a vague container for “do risky things later.”
