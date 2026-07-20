CREATE SCHEMA app;

CREATE TABLE app.facts (
  val bigint NOT NULL
);

CREATE VIEW app.fact_view AS
  SELECT val FROM app.facts;

CREATE MATERIALIZED VIEW app.fact_cache AS
  SELECT val FROM app.fact_view;

CREATE UNIQUE INDEX fact_cache_val_idx
  ON app.fact_cache (val);
