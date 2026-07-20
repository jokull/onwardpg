-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
-- The one newly deployed application version must work before and after every batch below.
-- Catch-up, validation, enforcement, and compatibility cleanup belong here.
-- ============================================================================
-- onwardpg:batch transactional
-- Batch batch-contract-001: transactional.
-- Review: safety=manual; hazards=manual_sql,data_movement,post_drain_backfill_required.
-- onwardpg:edit begin stmt-sha256-8efef1a10c145b891fc9259a7434ccab32bd75200e48130b10f6fbc37776f899
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
-- onwardpg:edit end stmt-sha256-8efef1a10c145b891fc9259a7434ccab32bd75200e48130b10f6fbc37776f899
-- Review: safety=review; hazards=table_scan,access_exclusive_lock,compatibility_removal.
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
