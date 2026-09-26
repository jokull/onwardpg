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
-- onwardpg:edit begin stmt-sha256-9b26c5e7e779fe588376a246169e4c21c30449293242eea637249e7bc2e512a2
-- PRODUCT-SPECIFIC SQL: Provide reviewed reconcile_contract_sql SQL for app.bookings.status
-- Verify: SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL);
-- ONWARDPG TODO: replace this comment with reviewed SQL for reconcile_contract_sql on app.bookings.status.
-- Planner analysis: Supply reviewed post-drain cleanup/backfill SQL for column:app:bookings:status. A generated read-only Boolean contract gate verifies the result; add product-specific assertions if needed.
-- Expected effect: complete the named operation and converge to the desired catalog state.
-- onwardpg:edit end stmt-sha256-9b26c5e7e779fe588376a246169e4c21c30449293242eea637249e7bc2e512a2

-- onwardpg:batch transactional
-- Batch batch-contract-002: transactional.
-- Review: safety=review; hazards=contract_data_assertion,table_scan_possible; requires_gates=writers:legacy.
-- Suggested session timeouts: statement_timeout=20m, lock_timeout=3s.
DO $onwardpg$ BEGIN IF NOT COALESCE((SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL)), false) THEN RAISE EXCEPTION 'onwardpg contract gate failed: data:c6703912502bd497'; END IF; END $onwardpg$;
-- Review: safety=review; hazards=table_scan,access_exclusive_lock,compatibility_removal; requires_gates=data:c6703912502bd497,writers:legacy.
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
