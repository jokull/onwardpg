-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
-- The one newly deployed application version must work before and after every batch below.
-- Catch-up, validation, enforcement, and compatibility cleanup belong here.
-- ============================================================================
-- onwardpg:batch nontransactional
-- Batch batch-contract-001: non-transactional; execute outside BEGIN/COMMIT.
-- Review: safety=dangerous; hazards=data_loss,concurrent_index_drop; requires_gates=writers:legacy.
DROP INDEX CONCURRENTLY "app"."accounts_org_idx";
-- Review: safety=dangerous; hazards=data_loss,concurrent_index_drop; requires_gates=writers:legacy.
DROP INDEX CONCURRENTLY "app"."accounts_name_idx";
