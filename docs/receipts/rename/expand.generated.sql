-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- EXPAND — run before the one application deployment anchored to this plan.
-- Old code must remain usable while new code begins using the expanded shape.
-- Transactional and non-transactional batches are marked below; this phase is not split by transaction.
-- ============================================================================
-- onwardpg:batch transactional
-- Batch batch-expand-001: transactional.
-- Review: safety=review; hazards=table_lock,column_overlap_bridge.
ALTER TABLE "app"."accounts" ADD COLUMN "full_name" text;
-- Review: safety=review; hazards=column_overlap_bridge,trigger_function.
CREATE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"() RETURNS trigger LANGUAGE plpgsql AS $onwardpg$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW."display_name" IS NULL THEN
      NEW."display_name" := NEW."full_name";
    ELSIF NEW."full_name" IS NULL THEN
      NEW."full_name" := NEW."display_name";
    ELSIF NEW."display_name" IS DISTINCT FROM NEW."full_name" THEN
      RAISE EXCEPTION 'onwardpg column bridge conflict on app.accounts: display_name and full_name differ' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW."display_name" IS DISTINCT FROM OLD."display_name" AND NEW."full_name" IS NOT DISTINCT FROM OLD."full_name" THEN
    NEW."full_name" := NEW."display_name";
  ELSIF NEW."full_name" IS DISTINCT FROM OLD."full_name" AND NEW."display_name" IS NOT DISTINCT FROM OLD."display_name" THEN
    NEW."display_name" := NEW."full_name";
  ELSIF NEW."display_name" IS DISTINCT FROM OLD."display_name" AND NEW."full_name" IS DISTINCT FROM OLD."full_name" AND NEW."display_name" IS DISTINCT FROM NEW."full_name" THEN
    RAISE EXCEPTION 'onwardpg column bridge conflict on app.accounts: display_name and full_name differ' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END
$onwardpg$;
-- Review: safety=review; hazards=column_overlap_bridge,trigger_behavior_change,table_lock.
CREATE TRIGGER "onwardpg_sync_column_4cff936be08db67c" BEFORE INSERT OR UPDATE OF "display_name", "full_name" ON "app"."accounts" FOR EACH ROW EXECUTE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();
-- Review: safety=review; hazards=column_overlap_bridge,data_movement,table_scan,unbounded_update,long_transaction,wal_volume,replica_lag.
UPDATE "app"."accounts" SET "full_name" = "display_name" WHERE "full_name" IS DISTINCT FROM "display_name";
