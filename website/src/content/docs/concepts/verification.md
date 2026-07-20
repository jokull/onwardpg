---
title: What verification proves
description: Understand clone convergence evidence and the operational facts it cannot establish.
---

`onwardpg verify` executes only in restricted disposable PostgreSQL databases. It checks that accepted history plus the selected bundle reaches the declared desired catalog.

## It proves

- the exact receipted phase SQL executes under its transactional batch rules;
- the selected prefix and full continuation both replay cleanly;
- every `verify.sql` assertion returns one boolean `true` row;
- dependency-safe operations reach the desired typed catalog fingerprint;
- the final semantic diff is empty; and
- the exact catalog graph observed after expand is content-addressed in the
  bundle for a later read-only production comparison.

For CI, `onwardpg verify --check` also rejects unreceipted edits, requires the selected bundle to be the history head, recompiles configured DDL, and rejects a stale desired fingerprint.

## It does not prove

- existing production rows satisfy a conversion or backfill assumption;
- locks fit your latency budget;
- table scans, WAL, and replica lag are acceptable;
- old application writers have drained;
- hidden dynamic SQL has no dependency;
- a rollback is ready; or
- the release system applied either phase.

Think of clone verification as a strong database-structure proof inside a deliberately limited boundary. Pair it with production preconditions, rollout telemetry, lock and statement timeouts, replica checks, and human approval for dangerous changes.

For gated plans, the next boundary is
[`onwardpg contract check`](/concepts/contract-readiness/). It checks the live
post-expand catalog, exact data assertions, and expiring potential-writer
evidence in a read-only snapshot. It still does not execute contract, and its
earlier result does not replace the assertion repeated inside contract SQL.
