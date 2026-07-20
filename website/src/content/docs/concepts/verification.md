---
title: What verification proves
description: Understand clone convergence evidence and the operational facts it cannot establish.
---

`onwardpg verify` executes only in restricted disposable PostgreSQL databases. It checks that accepted history plus the selected bundle reaches the declared desired catalog.

## It proves

- the exact receipted phase SQL executes under its transactional batch rules;
- the selected prefix and full continuation both replay cleanly;
- every `verify.sql` assertion returns one boolean `true` row;
- dependency-safe operations reach the desired typed catalog fingerprint; and
- the final semantic diff is empty.

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

