# PostgreSQL version policy

onwardpg supports PostgreSQL **15, 16, 17, and 18**. This is deliberately
independent of the historical versions accepted by the pinned Atlas reference:
onwardpg is a new library and does not claim compatibility with PostgreSQL 14
or older.

## Admission and retirement

- A server outside 15--18 is rejected before catalog inspection. In particular,
  a new major is not implicitly accepted merely because the catalog connection
  succeeds.
- Each supported major is covered by the `postgres` GitHub Actions matrix. The
  job builds the exact pinned Atlas commit and runs the full integration and
  differential suite against a real server.
- Adding a major requires catalog-query review, version-specific fixtures where
  needed, and green differential/integration evidence on that major before the
  runtime range may be widened.
- Retiring a major is a deliberate compatibility-policy change: update the
  runtime range, matrix, this document, README, and version tests together.

## Feature gating

Tests must gate only an unavailable feature, never an entire integration suite.
`NULLS NOT DISTINCT` is available throughout the supported range. Catalog
selectors preserve relevant historical PostgreSQL-version boundaries for
features that predate the supported range (such as generated columns and index
`INCLUDE`) so a future policy change cannot accidentally erase that knowledge.

Catalog inspection treats state it cannot model faithfully as an
explicit unsupported result. Version support is therefore not a promise that
every PostgreSQL extension or every Atlas product feature is supported; the
authoritative scope is the machine-readable
[`parity/atlas-postgres.json`](../parity/atlas-postgres.json) ledger.
