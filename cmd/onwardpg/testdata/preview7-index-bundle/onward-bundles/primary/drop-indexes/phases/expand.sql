-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- EXPAND — run before the one application deployment anchored to this plan.
-- Old code must remain usable while new code begins using the expanded shape.
-- Transactional and non-transactional batches are marked below; this phase is not split by transaction.
-- ============================================================================
-- onwardpg:batch nontransactional
-- Batch batch-expand-001: non-transactional; execute outside BEGIN/COMMIT.
-- Review: safety=review; hazards=index_build,table_lock_possible.
CREATE INDEX CONCURRENTLY "accounts_email_org_idx" ON "app"."accounts" USING "btree" ("email" NULLS LAST, "org" NULLS LAST);
-- Review: safety=review; hazards=unique_index_enforcement_removed,duplicate_rows_possible,concurrent_index_drop.
DROP INDEX CONCURRENTLY "app"."accounts_id_copy_idx";
-- Review: safety=review; hazards=unique_index_enforcement_removed,duplicate_rows_possible,concurrent_index_drop.
DROP INDEX CONCURRENTLY "app"."accounts_email_idx";
