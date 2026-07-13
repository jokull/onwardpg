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
- One-time `history init` onboarding that plans empty PostgreSQL to the current
  declarative schema, clone-verifies the `baseline` root bundle before writing
  it, and refuses to modify an existing target chain or application database.
- Evidence-linked ecosystem comparison covering Migra, pgmig, Stripe
  pg-schema-diff, Alembic, Django migrations, and Drizzle Kit, with explicit
  onwardpg gaps and a refreshed machine-readable pgmig roadmap map.
- A pinned, MIT-attributed Stripe pg-schema-diff v1.0.7 executable reference,
  deterministic 415-case acceptance inventory, and test-only differential
  convergence harness that cannot enter the production planning path.
- Continuous same-name standalone index replacement on ordinary tables,
  materialized views, and independent local partitions, with deterministic
  temporary identifiers, concurrent build/cleanup, phase boundaries, hazards,
  statement/lock-timeout guidance, and Stripe differential evidence. Direct
  leaf partition-parent hierarchies use an `ON ONLY` shell plus concurrent
  leaf builds/attachments; nested trees recursively create `ON ONLY` shells
  and attach concurrently built leaves bottom-up before retiring the old tree.
  Existing structurally matching local indexes can be attached to incomplete
  partitioned parents without rebuild or drop, with pinned Stripe differential
  convergence evidence. New local primary/unique constraints can claim
  same-named matching unique indexes before their constraint-owned attachment.
- Ordinary primary-key and unique-constraint definition changes without
  external dependents build replacement indexes concurrently and perform a
  short transactional constraint swap; foreign-key dependents reject.
- Typed standalone sequence `OWNED BY` edges and complete identity
  add/options/confirmed-drop planning with real-PostgreSQL and differential
  convergence evidence.
- Typed RLS enable/force state, policies, policy dependency ordering, and
  ordinary/partitioned-table privileges, including quoted roles, explicit
  authorization decisions, timeout/hazard metadata, and Stripe differential
  convergence evidence.
- Explicit blockers and exact ignore receipts for ownership/ACL/default
  privileges, rules, text search, event triggers, publications, extended
  statistics, FDW/server/user-mapping state, replica identity,
  clustered/invalid indexes, relation/column physical attributes, relation
  tablespaces, traditional inheritance, subscriptions, custom access methods,
  operators, casts, conversions and languages, security labels, unmodeled
  comments, and PostgreSQL 18-only constraint/generated-column state. Every
  PostgreSQL 14–18 catalog table is machine-classified, and the blocker suite
  runs on all five majors; the attribute audit remains explicitly incomplete.

### Known limitations

- No published binary artifacts, checksums, signing, package distribution, or
  release automation yet.
- Several PostgreSQL object families are explicitly unsupported or require
  manual work. See [README.md](README.md#what-the-developer-preview-supports) and
  [docs/supported-features.md](docs/supported-features.md).
- The real-PostgreSQL convergence corpus is growing and is not yet a complete
  production compatibility certification.
- Real migration execution remains deliberately outside onwardpg; the CLI has
  no production, staging, or development apply command.
