-- onwardpg: forward-only PostgreSQL migration plan.
-- Review every batch, safety classification, and hazard in the JSON plan before execution.
-- ============================================================================
-- EXPAND — run before the one application deployment anchored to this plan.
-- Old code must remain usable while new code begins using the expanded shape.
-- Transactional and non-transactional batches are marked below; this phase is not split by transaction.
-- ============================================================================
-- onwardpg:batch nontransactional
-- Batch batch-expand-001: non-transactional; execute outside BEGIN/COMMIT.
-- Review: safety=review; hazards=baseline_replay.
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint PRIMARY KEY,
  email text NOT NULL,
  org bigint,
  name text
);
CREATE INDEX accounts_org_idx ON app.accounts (org);
CREATE INDEX accounts_name_idx ON app.accounts (name);
CREATE UNIQUE INDEX accounts_id_copy_idx ON app.accounts (id);
CREATE UNIQUE INDEX accounts_email_idx ON app.accounts (email);
