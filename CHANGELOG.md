# Changelog

All notable changes to onwardpg are documented here. Published versions follow
Semantic Versioning; preview tags use the form `vX.Y.Z-preview.N`.

## Unreleased

### Fixed

- `drift check` no longer hides live catalog state that makes `init`, `plan`, and
  `diff` stop as unsupported. The state (for example
  `ownership:extension:earthdistance=pscale_admin` or
  `parameter_acl:session_replication_role`) was in the inspected snapshot, but
  the report dropped it and showed only `drifted`. The report now has an
  `unsupported` list; when it is not empty the status is `unsupported` and the
  exit code is 3, and the `differences` are still listed. A result without
  `unsupported` entries now means there is no modeled drift and nothing the
  planner would refuse. The list includes unsupported state in the replayed
  history as well as in the live catalog.
- `contract check` reported unsupported production state as `catalog_drift`
  (status `blocked`, exit 4), because that state is part of the observed
  fingerprint. It now reports status `unsupported` (exit 3), the selectors in
  `unsupported`, and the finding `unsupported_catalog_state`, and runs no data
  gate. Catalog drift is classified on the modeled graph alone, so a catalog
  that also differs from the receipted checkpoint still gets its
  `catalog_drift`, `expand_not_applied`, or `contract_already_applied` finding.

- A foreign key whose only differences from the desired schema are its own
  name and the name of the index behind the referenced primary-key or unique
  constraint is now offered as a `rename_constraint` decision, in the same round as the key's own
  rename. Before, such a foreign key was never a rename candidate, because the
  index name that PostgreSQL records for the referenced key (`UsingIndex`) was
  compared literally: after an ORM or `pg_dump` round trip left old key names
  (`..._pk`, `idx_..._sqlite_autoindex_...`), every foreign key referencing one
  was planned as drop plus re-add, with a full re-validation on a production
  table. The foreign key is a candidate only while its referenced key is itself
  a rename candidate that has not been declined; every other catalog field is
  still compared. A confirmed pair emits only `ALTER TABLE ... RENAME
  CONSTRAINT`, which PostgreSQL carries out as a metadata change. A foreign
  key that references a unique index without a constraint is not covered.

### Added

- A target can list `live_ignore` selectors in `.onwardpg.toml` for state a
  managed PostgreSQL provider owns in live clusters: an extension or a schema
  owned by another role (`ownership:extension:NAME=ROLE`,
  `ownership:schema:NAME=ROLE`) and a `pg_parameter_acl` grant
  (`parameter_acl:NAME`). Those selectors can exist only in production, so they
  could not be target `ignore` entries and every live command needed one
  `--ignore` flag per selector. `drift check`, `contract check`, and `diff`
  (with the new `--target NAME [--config FILE]`) remove exactly the named
  blocker markers from the live snapshot and list them as
  `observer.live_ignored` (`workspace_compatibility` for a `diff` plan). Selectors are
  exact, written as the report prints them with `quote_ident`-style
  identifiers, validated for form and kind, and need not match in every
  cluster. They never remove a typed object or add an ignore receipt.
  They are removed before the comparison fingerprint is computed, so a catalog
  that differs from the expected graph only by acknowledged state compares
  equal (`drift check` is `drift_free`; `contract check` matches its
  checkpoint) and the reported fingerprint depends on the list;
  `observer.observed_fingerprint` reports the fingerprint before removal, in
  `drift check` and `contract check` results when something was removed.
  A configured selector that matched nothing is listed as
  `observer.live_ignore_unmatched` (`live_ignore_unmatched:SELECTOR` in a `diff`
  plan's `workspace_compatibility`); that is information only and never changes
  a status, exit code, or fingerprint, so a typo, or a keyword that PostgreSQL
  quotes, does not pass unnoticed. `--target` and `--config` belong to `diff`
  only: the legacy `plan --from --to` spelling does not accept them and never
  applies the list.
  Planning, verification, replayed history, and DDL sources ignore the list. The attribute ledger keeps these catalogs classified as blocked:
  a parameter ACL or a foreign-owned schema can be a real privilege difference
  between environments, so nothing is acknowledged without an exact entry, and a
  changed owner or a new grant blocks again.
- `diff --from-env ENV` and `--to-env ENV` read a side's PostgreSQL URL from an
  environment variable, like `drift check --database-env`, so a live URL stays
  out of process arguments. Each is mutually exclusive with `--from` or `--to`
  for the same side.

- Targets can list `scratch_admin_extensions` in `.onwardpg.toml`, for example
  `[{ name = "earthdistance", schema = "extensions" }]`, with an optional
  `version`. When project DDL asks for an extension that PostgreSQL does not
  trust, the restricted scratch login is refused with
  `permission denied to create extension`. The scratch administrator then
  installs the listed extension and its dependencies into the named schema
  inside that disposable database (the entry's version, or the server default
  when none is given; a `VERSION` clause in the DDL is not honored for a listed
  extension), and the unchanged DDL is run again. The refusal is recognized by
  PostgreSQL's non-localized error fields (SQLSTATE 42501, `extension.c`,
  `execute_extension_script`), not by message text, so project SQL cannot
  trigger an install. Nothing is inferred from SQL text, an extension that
  nothing asks for is never installed, and no live-database path creates
  extensions. Without the entry the same error carries a hint naming the
  setting. A transient superuser role installs the extension and `REASSIGN
  OWNED` hands it to the restricted login, so the catalog and fingerprints equal
  those built where the owner created a trusted extension, and a later bundle
  can drop it. The list is receipted in `planner.scratch_admin_extensions`.
  Each accepted bundle replays under the list it receipted (a bundle without
  the field replays with none) in `init`, `plan`, `draft`, `verify`, and
  `drift check`; only the desired DDL and the bundle being planned use the
  current configuration. `verify --check` blocks with
  `scratch_admin_extensions_changed` when the checked bundle's receipt differs
  from the configuration. `config check` reports `scratch_admin_extensions`,
  `scratch_admin_installed`, and `notes`; an entry or version the server lacks is
  an error and an entry for a trusted extension is a note. `diff` and
  `plan --from --to` take `--scratch-admin-extension NAME=SCHEMA[@VERSION]`.

### Changed

- `--if-not-exists` now also renders `CREATE EXTENSION IF NOT EXISTS`, which
  generated bundles need when the scratch administrator installs an extension
  before they run.

## v0.1.0-preview.5 — 2026-10-06

### Fixed

- On PostgreSQL 18, a NOT NULL constraint no longer blocks as
  `not_null_constraint:` when its name is the one PostgreSQL itself generated
  for its table and column. The check now reproduces PostgreSQL's
  `makeObjectName` shortening to 63 bytes (including multibyte clipping) and the
  `not_null1`, `not_null2`, ... collision suffix, instead of comparing against
  `<table>_<column>_not_null`. Custom names, and default names left on a renamed
  table or column, still block. Catalogs that differ only in which generated name
  a constraint received now compare equal in `plan`, `diff`, `dev plan`, and
  `drift check`. `not_null_constraint:` ignore selectors that existed only for
  generated names can be removed; left in a target's `ignore` list they are
  dormant, but an unused `--ignore` flag on `diff` or `drift check` is still
  an error.

### Added

- `drift check --database-env ENV` reads the live URL from an environment
  variable, like `contract check --database-env`. `--database` still works; the
  two flags are mutually exclusive.

## v0.1.0-preview.4 — 2026-09-26

### Fixed

- Table exclusions now cover their owned catalog metadata without suppressing
  missing-table errors elsewhere. Application foreign keys that cross an ignored
  table boundary require explicit review.
- Bundles retain exact, observed development-only journal exclusions for live
  contract readiness. The saved policy is integrity-bound and cannot be widened
  by changing current configuration. Clone verification retains its own boundary.
- Development planning, raw diff, and drift checks no longer report differences
  caused only by a journal ignore receipt existing on one side of the comparison.
  Successful development output retains the observed exclusion evidence.
- Revoking temporary schema grants no longer leaves a false unsupported-ACL
  finding when the resulting custom-schema ACL exactly matches PostgreSQL's
  default. Actual privilege and ownership changes remain visible.
- Suggested commands carry current confirmed hints through edited-bundle
  revisions that are waiting for more decisions, without replacing existing SQL
  or reusing invalidated answers. Follow-up commands retain the selected target.
- Replanning explains invalidated answers while they remain unanswered, and
  cleanup prompts distinguish generated data gates from required custom queries.
- The Django exporter rejects model edits without migration state and supports
  explicit `pg_dump` selection and scratch-admin URL reuse.

### Documentation and adoption

- Add existing-project recipes for Drizzle 1.0 RC, Django, and Prisma, including
  framework migration handoff, empty-database bootstrap, and nullable overlap.
- Add disposable populated adoption sandboxes and record fresh-agent feedback
  with the limits of these small rehearsals.

## v0.1.0-preview.3 — 2026-09-26

This release hardens verification and the scratch execution boundary. Rebuild
or upgrade the CLI and rerun `onwardpg verify --check` on existing bundles before
relying on earlier verification receipts.

### Fixed

- Assertions now run through one read-only executor that rejects multiple SQL
  statements, writes, NULL or non-Boolean values, and extra rows or columns.
  A connection configured for simple protocol cannot escape a read-only check
  with `COMMIT` followed by a write. Statements are described before execution
  so a standalone transaction command cannot commit the surrounding work.
- Contract checks and assertions reject incomplete row visibility under RLS,
  including forced policies affecting a database owner and function-local RLS
  settings. The visibility check conservatively covers the whole database.
- Verification rejects transactional batches that commit, roll back, or replace
  their transaction, and non-transactional batches that leave a transaction open.
- Schema input reads are capped at 64 MiB; running exporters have output-size
  monitoring, a five-minute deadline, and process-group/job containment while
  retaining regular-file stdout. Checkout hashing streams file contents.
- Scratch cleanup has deadlines, reconnects after connection loss, preserves
  original and cleanup errors together, and identifies exact resources for retry.
- Go 1.26.8 and `golang.org/x/text` 0.42.0 replace versions with four reachable
  vulnerability advisories in the ownership review.

### Changed

- Release publication now depends on the full PostgreSQL 15–18 integration,
  differential, compiled-CLI acceptance, PGlite, documentation, and quality gates.
  Preview tags publish as prereleases with reviewed changelog notes.
- Public documentation is one narrative quick start with concrete migration
  scenarios. Blume is updated to 2.0.3.

### Verification and limits

- Adversarial assertion and transaction regressions cover the reproduced
  failures. The ownership review records their prerequisites and remaining
  production rollout responsibilities.
- Compiled-CLI release rehearsals cover populated renames with prepared old/new
  clients contending on the same rows, exact counter preservation, and blocked
  transactional DDL cancellation followed by retry. Both tests are mandatory on
  PostgreSQL 15–18; exporter process containment is tested on native Windows.
- The pinned Stripe comparison harness isolates temporary resources per run,
  cleans them by exact owner, and surfaces incomplete cleanup.
- Clone verification establishes catalog convergence and declared assertions.
  Production data, load, writer drainage, and deployment timing still need
  application-specific review.

## v0.1.0-preview.2 — 2026-07-21

### Added

- Stripe pg-schema-diff drift-close recipes for reviewed column-type handoffs,
  dependency-aware primary/unique replacement across inbound foreign keys,
  typed ordinary/materialized-view recreation closures, and retained-data
  partition-topology runbooks. Partition conversions enumerate deterministic
  shadow trees, copy/catch-up and verification gates, brief rename cutover,
  and separately authorized old-hierarchy cleanup instead of copying Stripe's
  data-deleting replacement strategy.

### Changed

- Decision output now carries non-executable SQL guidance in both text and JSON,
  so a fingerprint-bound partition decision arrives with a bounded scaffold
  rather than an open-ended request to invent the migration.
- View and materialized-view ownership, privileges, triggers, indexes,
  population state, and transitive dependencies survive typed rebuilds;
  extension-owned view ACLs remain represented by their extension rather than
  leaking into caller-schema diffs.

### Fixed

- Generated Homebrew Formulae install from Homebrew's stripped archive root
  rather than addressing the tarball's enclosing directory a second time.

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

- Ground-floor `init` preserves and clone-verifies the complete authoritative
  exported DDL after typed catalog inventory, including session scaffolding
  such as `SET search_path` that cannot be reconstructed from graph nodes.
- Declarative physical column position remains typed compatibility evidence but
  no longer blocks semantic planning or convergence.
- Replanning after an unaccepted column was applied locally now collapses the
  durable bundle to the final name while offering an explicit dev-scoped
  rename. A confirmed development rename emits a direct column rename and, on
  PostgreSQL 18, the companion generated `NOT NULL` constraint rename. The
  rolling-safe trigger bridge remains the only durable rename strategy.
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
- All command envelopes use status-oriented JSON without speculative protocol
  versions; decision handoffs include a stable `next_action`, written paths and
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

### Fixed

- Capture `schema_command` output through a regular temporary file so exporters
  such as `drizzle-kit export` cannot truncate large DDL streams at pipe-buffer
  boundaries.
- Exclude VCS internals and installed `node_modules` trees from schema-command
  mutation checks, avoiding minute-long monorepo scans without ignoring
  generated project files.
- Preserve PostgreSQL 18's generated `NOT NULL` constraint identity when a
  confirmed direct or rolling-safe column rename changes its canonical name.

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
