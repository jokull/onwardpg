---
title: Safety boundary
description: What onwardpg is allowed to touch, where it fails closed, and what reviewers still own.
---

onwardpg is a planner with one execution surface: random disposable PostgreSQL databases created through the configured scratch administrator. It does not apply migrations to caller-owned targets.

## Scratch isolation

Each materialization uses a random, short-lived login that owns only its random database and lacks `SUPERUSER`, `CREATEDB`, `CREATEROLE`, `REPLICATION`, and `BYPASSRLS`. The administrative identity only provisions and cleans up.

The scratch cluster must still provide required roles, languages, and extension packages. Extensions needing superuser installation or externally owned objects may require provisioning outside onwardpg’s database-local boundary.

## Fail-closed planning

Unsupported or ambiguous catalog shapes block instead of comparing equal. Rename, destructive, cast, authorization, and backfill decisions are bound to exact graph fingerprints. Product-specific SQL remains an editable TODO until it executes and converges in verification.

Every contract statement that restores row enforcement has a typed disposition:
an exact data/writer gate or a narrow catalog/atomic proof. A hazard label alone
cannot authorize tightening. `contract check` evaluates live catalog, data, and
writer evidence read-only, while contract SQL repeats data assertions at the
actual enforcement boundary.

Narrow ignore selectors acknowledge a blind spot; they do not declare ignored objects equivalent. Every selector is validated and exact exclusions are reported.

## Reviewer ownership

You remain responsible for:

- product semantics and application query compatibility;
- production data validity and backfill progress;
- lock duration, table size, WAL, replicas, and availability;
- rollout, drain, and rollback evidence;
- secrets, authentication, and production application of SQL; and
- dynamic SQL dependencies PostgreSQL itself does not catalog.

Review generated files as deployment code. A green verification result is strong structural evidence, not a production safety oracle.

Editable SQL is scoped, not free-form. A same-type rename with an unproved
dependent view gets three ordered pockets around the generated bridge. A
confirmed cross-name/type transition gets exactly two pockets that own both
columns and the full current/desired dependency closure. Unresolved TODOs,
escaped endpoint DDL, or a non-converged final catalog block verification.

`CREATE OR REPLACE VIEW` is emitted only when existing output names, order,
and PostgreSQL type identities remain stable; only compatible appended outputs
may be added. Existing output renames and type changes must use the owning
transition handoff. Materialized-view refresh, rebuild availability, and
freshness remain explicit reviewer decisions.
