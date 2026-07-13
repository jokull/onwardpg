# Changelog

All notable changes to onwardpg are documented here. This project follows
[Semantic Versioning](https://semver.org/) once published releases begin.

## Unreleased — developer preview

This is the first developer-preview cut of onwardpg. It is intended for local,
reviewed use by developers and coding agents; it is not a production-release
claim and does not apply migrations automatically.

### Added

- PostgreSQL 14–18 catalog-backed schema comparison for live databases and
  declarative `CREATE`-statement SQL materialized in disposable PostgreSQL.
- Typed dependency-aware planning for core relations, constraints, indexes,
  enums, sequences, views, materialized views, routines, triggers, and
  partition children.
- Forward-only annotated SQL and a versioned JSON planning protocol with
  fingerprint-bound answers for ambiguous or destructive intent.
- Expand, migrate, contract, and manual-work phases; typed hazards; and valid
  transactional/non-transactional execution batches.
- Conservative rename handling and explicit unsupported results instead of
  guessed casts, backfills, destructive operations, or unknown catalog state.
- Deterministic statement IDs, `onwardpg.bundle/v1` receipt directories,
  phase-specific SQL artifacts, preserved decision history, strict repository
  configuration, and versioned diagnostic errors.
- Read-only `pr status` Git provenance with base-erosion, protected migration
  history, concurrent path-collision, and dirty-revision classification.
- Isolated synthetic PR trees, deterministic declarative compilers, Drizzle or
  ordered-SQL base-history replay, `BC == BM` enforcement, and Git-verified
  `pr regenerate` bundle generation.

### Known limitations

- No published binary artifacts, checksums, signing, package distribution, or
  release automation yet.
- Several PostgreSQL object families are explicitly unsupported or require
  manual work. See [README.md](README.md#known-preview-gaps) and
  [docs/supported-features.md](docs/supported-features.md).
- The real-PostgreSQL convergence corpus is growing and is not yet a complete
  production compatibility certification.
