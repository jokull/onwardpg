-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
-- The one newly deployed application version must work before and after every batch below.
-- Catch-up, validation, enforcement, and compatibility cleanup belong here.
-- ============================================================================
-- onwardpg:batch transactional
-- Batch batch-contract-001: transactional.
-- Review: safety=review; hazards=final_overlap_catchup,data_movement,table_scan,unbounded_update,long_transaction,wal_volume,replica_lag; requires_gates=writers:legacy.
UPDATE "app"."accounts" SET "full_name" = "display_name" WHERE "full_name" IS DISTINCT FROM "display_name";
-- Review: safety=review; hazards=rename_equality_assertion,table_scan; requires_gates=data:53fe35210481b9df,writers:legacy.
DO $onwardpg$ BEGIN IF EXISTS (SELECT 1 FROM "app"."accounts" WHERE "full_name" IS DISTINCT FROM "display_name") THEN RAISE EXCEPTION 'onwardpg column rename equality assertion failed for column:app:accounts:display_name'; END IF; END $onwardpg$;
-- Review: safety=review; hazards=compatibility_removal,table_lock; requires_gates=data:53fe35210481b9df,writers:legacy.
DROP TRIGGER "onwardpg_sync_column_4cff936be08db67c" ON "app"."accounts";
-- Review: safety=review; hazards=compatibility_removal; requires_gates=data:53fe35210481b9df,writers:legacy.
DROP FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();
-- Review: safety=dangerous; hazards=compatibility_removal,data_loss,access_exclusive_lock; requires_gates=data:53fe35210481b9df,writers:legacy.
ALTER TABLE "app"."accounts" DROP COLUMN "full_name";
-- Review: safety=review; hazards=compatibility_removal,access_exclusive_lock; requires_gates=data:53fe35210481b9df,writers:legacy.
ALTER TABLE "app"."accounts" RENAME COLUMN "display_name" TO "full_name";
