-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
-- The one newly deployed application version must work before and after every batch below.
-- Catch-up, validation, enforcement, and compatibility cleanup belong here.
-- ============================================================================
-- onwardpg:batch transactional
-- Batch batch-contract-001: transactional.
-- Review: safety=manual; hazards=contract_reconciliation,data_movement,post_drain_writers_required; requires_gates=writers:legacy.
-- onwardpg:edit begin stmt-sha256-7d55b725e4e5aeac8a2691e13518e9723c9a61598d40e49ed5890dea254005d4
UPDATE "app"."bookings"
SET "status" = 'pending'
WHERE "status" IS NULL;
-- onwardpg:edit end stmt-sha256-7d55b725e4e5aeac8a2691e13518e9723c9a61598d40e49ed5890dea254005d4

-- onwardpg:batch transactional
-- Batch batch-contract-002: transactional.
-- Review: safety=review; hazards=contract_data_assertion,table_scan_possible; requires_gates=writers:legacy.
-- Suggested session timeouts: statement_timeout=20m, lock_timeout=3s.
DO $onwardpg$ BEGIN IF NOT COALESCE((SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL)), false) THEN RAISE EXCEPTION 'onwardpg contract gate failed: data:1c16b884027de910'; END IF; END $onwardpg$;
-- Review: safety=review; hazards=table_scan,access_exclusive_lock,compatibility_removal; requires_gates=data:1c16b884027de910,writers:legacy.
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
