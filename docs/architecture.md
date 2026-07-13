# Architecture

`onwardpg` treats PostgreSQL catalog state as a typed dependency graph. SQL is
an input and an output, not the internal schema model.

```text
live URL ───────────────┐
                       ├─ catalog snapshot ─ typed graph ─ semantic changes
DDL / ORM adapter ─ temp PostgreSQL ┘                         │
                                                             ▼
                                            questions + hazard policy
                                                             │
                                                             ▼
                                              dependency-ordered batches
                                                             │
                                                             ▼
                                                   reviewable forward SQL
```

## Invariants

- Catalog inspection occurs in one `REPEATABLE READ, READ ONLY` transaction.
- Every modeled object has a typed node and every known dependency has an edge.
- Graph fingerprints use deterministic canonical JSON and SHA-256.
- Unknown catalog state blocks planning unless a validated narrow ignore
  selector accounts for it.
- Creation follows desired-state dependencies. Destruction reverses
  current-state dependencies. Strongly connected components remain explicit.
- Rename, cast, backfill and destructive intent come from fingerprinted answers,
  never heuristics presented as facts.
- Plans are forward-only artifacts. Applying them is outside the planner.

## Public boundaries

- [`pgschema`](../pgschema) is the public typed graph model.
- [`adapter`](../adapter) lets ORM and code-schema tooling provide declarative
  PostgreSQL DDL or a complete typed snapshot.
- [`parity/atlas-postgres.json`](../parity/atlas-postgres.json) is a
  machine-readable pinned-reference behavior study.

## Current state

The CLI invokes the graph planner directly; the legacy flat planner has been
removed. The preview's core object families are populated directly from PostgreSQL
catalogs, while non-core catalog objects become explicit graph blockers. The
graph differ and dependency scheduler are the only active planning path.

This architectural milestone is not a parity or release claim. Differential
coverage, PostgreSQL-version coverage, and the remaining production gates are
tracked by the feature map and release documentation.
