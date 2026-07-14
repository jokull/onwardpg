# Architecture

`onwardpg` treats PostgreSQL catalog state as a typed dependency graph. SQL is
an input and an output, not the internal schema model.

```text
accepted chain ── disposable replay ── H ─┐
                                          ├─ typed graph diff ─ questions ─ phased SQL
working DDL ─── disposable PostgreSQL ─ W ┘

developer DB ───────────────────────── D ── diff to W only for local reconciliation
production ─────────────────────────── P ── compared to H only by explicit drift audit
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
- Git state is supplied by the coding agent through the files in the checkout;
  the preferred `plan` path sees only hash-chained history, one explicitly
  selected local PlanID, and desired DDL. It excludes that mutable plan and
  derives the unique remaining accepted head, preventing accidental stacking
  without teaching onwardpg about Git. The lower-level `draft --after` keeps an
  explicit name-and-digest assertion for diagnostics and compatibility. A
  repository-scoped OS advisory lock serializes lifecycle commands; their
  commit points reload configuration and revalidate DDL, history, and exact
  artifacts before writing.
- PostgreSQL physical column positions are preserved catalog state. A desired
  order that `ALTER TABLE` cannot reach is an explicit unsupported result, not
  silent equivalence or a late fingerprint-only failure.

## Product boundaries

- [`pgschema`](../pgschema) is the public typed graph model.
- [`schema-inputs.md`](schema-inputs.md) defines the CLI's deliberately narrow
  PostgreSQL DDL input contract.
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
