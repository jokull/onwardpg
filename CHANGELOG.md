# Changelog

All notable changes to onwardpg are documented here. Published versions follow
Semantic Versioning; preview tags use the form `vX.Y.Z-preview.N`.

## Unreleased

## v0.1.0-preview.1 — 2026-07-15

This is the first developer-preview line. onwardpg generates forward-only,
reviewable PostgreSQL migration bundles. It never applies SQL to a caller-owned
development, staging, or production database.

### Added

- A typed PostgreSQL dependency graph populated from consistent read-only
  catalog snapshots on PostgreSQL 15–18.
- Live PostgreSQL and deterministic `schema_file` / `schema_command` inputs;
  CREATE-statement DDL is materialized in disposable PostgreSQL rather than
  partially parsed.
- Git-free `init`, `history status`, `dev plan`, `draft`, `verify`, and `drift check`
  workflows plus a low-level explicit-source `plan` command.
- Content-addressed, per-target history with parent digests, fork detection,
  deterministic replay, and one explicitly selected mutable feature bundle.
- Agent-facing semantic hints for renames, destructive changes, type changes,
  NOT NULL rollout choices, confirmations, and product-specific SQL handoff.
  Hints can be supplied ahead of time and are bound to narrow graph scopes in
  generated receipts.
- Readable `expand.sql` and `contract.sql` files around exactly one application
  deployment, with phase
  timing, batch boundaries, hazards, lock/rewrite guidance, and optional
  `verify.sql` boolean assertions.
- Stable edit pockets that transplant agent-owned SQL through regenerated
  surroundings, plus conservative three-way conflicts for edits outside those
  pockets.
- Trigger-backed same-type column rename overlap: old and new names remain
  writable during rollout, divergent dual writes fail, and contract preserves
  original catalog identity.
- Disposable clone verification of generated and edited SQL, expected partial
  residuals, exact edit receipts, typed failure diagnostics, cancellation
  cleanup, and the read-only `verify --check` CI gate.
- Explicit read-only drift auditing against replayed history.
- Broad PostgreSQL planning for tables, columns, constraints, indexes,
  sequences and identity, enums, extensions, routines, triggers, views,
  materialized views, row-level security, privileges, and common partition
  relationships. Unmodeled catalog state blocks or requires a validated narrow
  ignore selector.
- Continuous concurrent index replacement, staged NOT NULL enforcement,
  foreign-key cycle handling, and explicit transactional/non-transactional
  batches.
- Pinned, test-only Atlas and Stripe pg-schema-diff references with
  machine-readable capability matrices and MIT attribution where applicable.
- Tag-driven deterministic archives on Darwin, Linux, and Windows on amd64 and
  arm64, with embedded version metadata, SHA-256 checksums, GitHub provenance
  attestations, and a generated Formula for `jokull/homebrew-tap`.
- A large-schema planner benchmark and documented preview performance envelope;
  typed-ID ordering avoids allocation-heavy string formatting in graph sorts.

### Changed

- The ordinary developer-preview loop is now `init`, one evolving `plan`,
  `status`, and `verify`. It keeps durable H → W planning separate from
  workspace-safe D → W SQL; strict local decisions use scoped `--dev-hint`
  rather than being reused as durable migration intent.
- The supported PostgreSQL range is 15–18. PostgreSQL 14 is rejected by both
  live inspection and recorded source receipts.
- Product-specific backfills and orchestration are edited directly in phase SQL
  rather than authored through a JSON operation language.
- The normal lifecycle has exactly two phases around one application deployment:
  expand and contract. Backfills are work inside a phase, not another deploy.
- `--target` defaults to the sole configured database and is required only when
  multiple targets make selection ambiguous.
- `dev plan`, `draft`, and low-level `plan` share `--output text|json`; JSON is the stable
  non-interactive default.
- Empty DDL is accepted as a valid empty desired schema, while destructive
  changes still require explicit intent.
- The PostgreSQL major is discovered from the scratch server, recorded in
  bundle receipts, and enforced during replay rather than duplicated in config.
- Frameworks participate only by exporting PostgreSQL DDL; onwardpg has no
  framework adapter API.
- Rename decisions enumerate every credible target, and confirmed table
  renames compose with same-column structural changes instead of degrading to
  destructive replacement.
- Product-authored SQL that resolves a generated TODO is preserved and
  re-verified when the same logical bundle is restacked over a new history
  parent.
- `history status` now emits a content-bound `head_ref`; `draft --after`
  requires that exact name-and-digest predecessor from the coding agent. A
  one-shot `--create` distinguishes first creation from refresh, preventing an
  accidentally missing bundle from silently losing agent-authored SQL.
- A repository-config OS advisory lock, final history/configuration/DDL
  revalidation, post-install receipt validation, and complete backup comparison
  reject concurrent onwardpg forks and ordinary path-based editor races. Valid
  unreceipted SQL can be restacked over incoming history before its old parent
  can be verified again; concurrent external saves remain outside the supported
  operating model.
- Generated-only bundles fully absorbed by incoming accepted history are
  removed with an explicit `absorbed` result instead of leaving empty entries.
- All command envelopes use consistently named `protocol_version` and `status`
  values; decision handoffs include a stable `next_action`, written paths and
  shell-safe grouped choices. Help exits successfully, invocation errors are
  machine-clean, and history-chain blockers consistently exit 4.
- `dev plan` reports an explicit `no_changes` result. Partial clone verification
  returns `partial_verified` only after the prefix and full continuation both
  succeed, and names simulated and remaining bundle phases. `config check`
  validates both database URLs and existing history majors.
- Verification distinguishes total and selected-bundle batch counts, and lists
  successful assertion IDs while documenting the empty-clone data boundary.
  Partial reports attribute continuation-only assertions separately instead of
  implying the selected prefix ran them.
- Hint-resolved restacks clear historical pending answer-rebind fields, and a
  nullable `ADD COLUMN` without a default no longer claims a possible table
  rewrite.

### Removed

- Git, branch, pull-request, merge-base, and dirty-working-tree awareness.
- `pr`, `ci`, `history init`, and `bundle verify` command aliases.
- The public adapter package and legacy Git-derived analysis packages.
- Fingerprint-bound `--answers` authoring. Internal answer evidence
  remains generated and state-bound.
- Free-form `intent.md` authoring and low-level bundle-writing flags; bounded
  hints carry decisions, phase SQL carries richer intent, and only `draft`
  writes durable history.
- The abandoned `execution.json` receipt/finalization lifecycle. onwardpg does
  not observe application, and explicitly selecting a feature bundle keeps it
  mutable until it becomes unselected base history.
- Caller-database apply, deployment orchestration, down migrations, ORM journal
  integration, embedded agents, and plugin APIs.

### Verification

- Full unit, race, vet, staticcheck, formatting, and parity-matrix gates pass.
- The Git-free lifecycle, edited SQL handoff, partial/full convergence,
  transactional rollback, non-transactional failure, false assertions,
  cancellation, cleanup, and major-version receipts have been exercised on
  real PostgreSQL 15, 16, 17, and 18.
- CI builds release archives twice and compares their checksums and generated
  Homebrew Formula before a preview tag is published.

### Known limitations

- PostgreSQL families marked unsupported in
  [docs/supported-features.md](docs/supported-features.md) remain explicit
  blockers unless narrowly ignored.
- Declarative physical column reordering and middle insertion are explicitly
  unsupported because ordinary `ALTER TABLE` cannot reach that catalog shape.
- Clone convergence proves schema effects and declared assertions, not
  production traffic safety, application compatibility, or rollout timing.
- Migration application remains deliberately outside onwardpg.
