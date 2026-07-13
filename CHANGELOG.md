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
- Isolated synthetic PR trees, deterministic DDL export commands,
  hash-chained base-history replay, `BC == BM` enforcement, and Git-aware
  `pr regenerate` bundle generation.
- Git-free PR-analysis core fed by prepared directories and
  explicit receipts, with Git retained as optional CLI orchestration.
- Read-only bundle freshness classification with typed remediation.
- Idempotent draft generations/decision attempts and stronger bundle
  replacement, phase-integrity, digest-framing, path, and secret guards.
- Narrow DDL-export boundary: `schema_file` or deterministic `schema_command`
  feeds the CLI; framework adapters, ORM journal integration, and runner
  handoffs are intentionally out of scope. Phase bundles are the forward
  migration history.
- Per-target content-addressed bundle history with canonical parent/entry
  digests, fork and missing-parent rejection, filename-independent replay, and
  complete `BC == BM` / `HC == HM` schema-square verification during PR
  regeneration.
- Question-scoped answer fingerprints and staged answer rebinding across base
  erosion, with explicit carried, invalidated, unanswered, and deferred
  reports. Equivalent Git provenance refreshes no longer create a new bundle
  generation when the schema/planner/history contract is unchanged.
- Config-driven `dev plan` for the deliberate local database loop, including
  deterministic `schema_file` / `schema_command` export and the same typed
  questions, answer files, phase SQL, and residual diff as the low-level CLI.
- Canonical staged-question receipts in ready bundles, allowing rename and
  manual-work answers to survive repeated feature edits and multiple base
  history changes instead of disappearing at generation boundaries.
- A `staged_with_backfill` NOT NULL strategy that records application-owned
  manual SQL and boolean verification without inventing data logic.
- `bundle verify`, which executes selected phases only in self-created
  disposable databases, checks manual postconditions, and reports residual or
  full convergence; failed transactional postconditions roll back.
- Strict read-only `ci check` composition for committed one-bundle ownership,
  freshness, hash-chain integrity, schema-square fidelity, and clone
  convergence.

### Known limitations

- No published binary artifacts, checksums, signing, package distribution, or
  release automation yet.
- Several PostgreSQL object families are explicitly unsupported or require
  manual work. See [README.md](README.md#known-preview-gaps) and
  [docs/supported-features.md](docs/supported-features.md).
- The real-PostgreSQL convergence corpus is growing and is not yet a complete
  production compatibility certification.
- Execution/amendment receipts and the post-merge delayed-contract lifecycle
  remain future work; the developer preview does not include a production
  migration apply command.
