-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
-- The one newly deployed application version must work before and after every batch below.
-- Catch-up, validation, enforcement, and compatibility cleanup belong here.
-- ============================================================================
-- onwardpg:batch transactional
-- Batch batch-contract-001: transactional.
-- Review: safety=review; hazards=blocking_lock,data_loss,derived_object_rebuild,materialized_view_rebuild,stored_data_recomputed; requires_gates=writers:legacy.
DROP MATERIALIZED VIEW "app"."fact_cache";
-- Review: safety=review; hazards=blocking_lock,data_loss,derived_object_rebuild; requires_gates=writers:legacy.
DROP VIEW "app"."fact_view";
-- Review: safety=manual; hazards=manual_sql,table_rewrite_possible,access_exclusive_lock,single_deployment_bridge_required; requires_gates=writers:legacy.
-- onwardpg:edit begin stmt-sha256-edd1fc33ddcb4025e793468d85aa41aebe92ce7fc66e5c674c50f7bbed0b6619
-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for column:app:facts:val (integer -> bigint).
-- After pre-deployment writers drain, perform final catch-up/assertions, remove compatibility objects, and converge to PostgreSQL type bigint.
-- Required mutation shape: convert "app"."facts"."val" to bigint with a reviewed expression; do not rely on an inferred cast.
-- Add boolean assertions to verify.sql for every data-dependent conversion assumption.
-- onwardpg:edit end stmt-sha256-edd1fc33ddcb4025e793468d85aa41aebe92ce7fc66e5c674c50f7bbed0b6619
-- Review: safety=review; hazards=type_change_statistics_refresh,database_performance_impact; requires_gates=writers:legacy.
ANALYZE "app"."facts" ("val");
-- Review: safety=review; hazards=blocking_lock,derived_object_rebuild; requires_gates=writers:legacy.
CREATE VIEW "app"."fact_view" AS SELECT val
   FROM app.facts;
-- Review: safety=review; hazards=blocking_lock,derived_object_rebuild,materialized_view_rebuild,stored_data_recomputed; requires_gates=writers:legacy.
CREATE MATERIALIZED VIEW "app"."fact_cache" AS SELECT val
   FROM app.fact_view WITH DATA;

-- onwardpg:batch nontransactional
-- Batch batch-contract-002: non-transactional; execute outside BEGIN/COMMIT.
-- Review: safety=review; hazards=compatible_writers_required,derived_object_rebuild,index_build,materialized_view_data_derived,materialized_view_index_rebuild,table_lock_possible; requires_gates=writers:legacy.
CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx" ON "app"."fact_cache" USING "btree" ("val" NULLS LAST);
