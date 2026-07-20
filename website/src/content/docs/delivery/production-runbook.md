---
title: Production runbook
description: Operational checks for safely applying an onwardpg bundle.
---

onwardpg gives you reviewed SQL and clone-convergence evidence. The production operator supplies environment evidence.

## Before expand

- Confirm the bundle is the reviewed immutable artifact and `onwardpg verify --check` passed on the same PostgreSQL major.
- Read every statement’s hazards, transaction boundary, and timeout guidance.
- Estimate table scans, rewrites, index builds, WAL, lock queues, and replica impact against real relation sizes.
- Confirm backups and the application rollback path. A down migration is not generated.
- Ensure the new application can tolerate both the expanded and contracted shapes.

## Apply expand

Run batches exactly as rendered. Nontransactional work such as concurrent index creation must remain outside a transaction. Observe lock waits, error rate, database saturation, WAL generation, and replica lag.

If expand partially fails, stop. Diagnose the exact batch and reconcile forward; do not assume rerunning arbitrary SQL is idempotent.

## Deploy and drain

Deploy the dual-compatible application. Prove old instances and writers are gone, including background workers, scheduled jobs, queues, stale connection pools, and rollback traffic. Run any separately operated backfill required by the reviewed plan. A backfill embedded in a contract edit pocket runs later, as part of the exact contract batch, before its dependent enforcement statement.

## Apply contract

Recheck production preconditions in `verify.sql` where applicable. Then apply contract with the same batch and timeout discipline. Contract may validate constraints, revoke behavior, remove bridges, or perform a final rewrite; “after deploy” does not mean “low risk.”

## After contract

Confirm application health, database errors, locks, replicas, and the expected schema. Record bundle digest, phases, timestamps, operator, database major, and deployment release. Run [drift check](/reference/cli/#drift-check) on your normal operational cadence.
