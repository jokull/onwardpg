# Developer-preview security review

Updated 2026-09-26. The [ownership review](ownership-review.md) reproduced and
fixed assertion execution, row-security visibility, and transaction-boundary
failures missed by the previous review. This is a scoped engineering review,
not a third-party audit or a production-safety certification.

## Trust boundaries

- Caller-owned PostgreSQL URLs are catalog-inspected in a repeatable-read,
  read-only transaction. onwardpg never applies migration SQL to them.
- The configured scratch URL is an administrator for a dedicated local or CI
  cluster. It creates a one-hour login and disposable database, but DDL and
  migration SQL execute only as that database owner. The execution login has
  no superuser, role, database, replication, or row-security-bypass authority.
  The scratch URL must never point at production or a shared application
  cluster. Disposable databases use `template0`, not a locally customized
  default template. Creation explicitly copies the connected control
  database's encoding, locale provider, locale, collation, and ctype, and
  rejects a collation-version mismatch.
- A target may list `scratch_admin_extensions`. For each listed name the
  scratch administrator, not the restricted login, runs that extension's
  install script (and those of its declared dependencies) as a superuser inside
  the disposable database, after PostgreSQL's own `CREATE EXTENSION` refused the
  restricted login. To keep the extension owned by the restricted login the
  administrator creates a transient `NOLOGIN SUPERUSER` role, installs through
  it, runs `REASSIGN OWNED`, and drops the role, all in one transaction; that
  role is never granted to, or loginable by, the restricted login, and it is a
  cluster-global role only for the length of that transaction. This widens
  trust in the script and the library code that the extension loads on the
  scratch server: an extension such as `dblink` or `file_fdw` gives later
  project DDL and migration SQL in that database whatever the extension exposes,
  for example outbound connections or server-file access. Keep the list to
  extensions that the scratch cluster's operators already trust, and keep that
  cluster dedicated. The trigger is PostgreSQL's own refusal, recognized by its
  non-localized source location (SQLSTATE `42501`, `extension.c`,
  `execute_extension_script`), so project SQL cannot raise a lookalike error to
  make the administrator run an install script. Nothing is inferred from SQL
  text; an entry that nothing asks for has no effect; the login itself stays
  non-superuser; the database and login are still dropped afterwards. The list
  is receipted in each bundle, each accepted bundle replays under its own
  receipt, and `verify --check` blocks when a bundle's receipt differs from the
  configuration. Caller-owned development and production databases are
  inspected read-only and no live-database code path creates an extension.
  `live_ignore` is unrelated: it acknowledges provider-owned state in live
  clusters and never applies to a disposable database.
- `schema_command` is trusted project code, invoked directly without a shell.
  It is checked for deterministic output and observable checkout mutations,
  but it is not an operating-system sandbox.
- Generated SQL and edited phase SQL are untrusted until clone verification
  succeeds. Verification proves declared catalog convergence and assertions;
  it does not prove production traffic safety.
- Semantic hints cannot add arbitrary planner answers. They must match a
  currently reachable choice, and onwardpg generates the fingerprint-bound
  receipt itself.

## Controls reviewed

- Bundle paths reject traversal and dot-only identities; reads reject symlinks,
  unexpected files, missing receipts, and digest drift.
- Bundle replacement uses a per-bundle lock, re-reads the destination before
  replacement, checks identity and generation, preserves a recoverable backup,
  flushes files and directories before atomic rename, and refuses executed or
  unreceipted state.
- Digest inputs use length framing. Identifier rendering uses PostgreSQL-safe
  quoting and structured identifier arrays avoid delimiter ambiguity.
- Source descriptions reject URLs and common libpq secret-bearing forms.
  Connection strings are used at runtime and are not written to bundles.
- Configuration files and exporter reads are bounded. Both schema input paths
  accept at most 64 MiB; command output is monitored while the process runs,
  and each export has a five-minute deadline. Stdout remains a regular file.
  Checkout hashing streams input. The monitor is not a strict disk quota;
  see [exporter limits](exporter-limits.md) for process and platform boundaries.
- User-authored Boolean checks use one shared executor: a read-only transaction
  or savepoint, pre-execution Parse/Describe, forced extended protocol, exactly
  one non-null Boolean result, and unconditional rollback. Transaction commands
  are rejected before they can end their surrounding transaction.
- Assertions and readiness reject any database containing a table whose RLS
  applies to the current role, including unmanaged tables and unrelated queries.
  This prevents function-local `row_security=on` from concealing rows. Ordinary
  non-forced RLS owned by the observer remains supported. `row_security=off` is
  an additional guard; it does not bypass policies.
- Transactional verification checks that the PostgreSQL transaction ID stays
  unchanged. Explicit commits or rollbacks cannot silently satisfy a
  transactional batch. Detection cannot undo an earlier explicit commit; the
  verification database is disposable. Non-transactional batches must leave
  the connection idle, and failures may have partially applied there.
- Unknown catalog families block planning unless a validated narrow ignore
  selector matches them; ignored state is reported explicitly.
- The 2026-09-26 dependency scan is clean with Go 1.26.8, pgx 5.9.2, and
  `golang.org/x/text` 0.42.0. CI and release jobs rerun the pinned `govulncheck`
  command; this result applies to these versions and that scan date.

## Residual risks and release gates

- A malicious `schema_command` has the authority of the onwardpg process. Run
  only repository-controlled export commands, ideally in an isolated CI job.
- Read-only SQL is not a sandbox for arbitrary database functions. Review gate
  SQL and installed routines, and use a dedicated observer where supported.
- Scratch cleanup uses an uncancelled context without a deadline. A stalled
  administrator connection can delay cleanup indefinitely; a disconnected or
  killed process can leave disposable databases and roles behind.
- Clone verification cannot model table size, lock queues, concurrent traffic,
  role membership outside the clone, or application rollout correctness.
- Superuser-only extensions are materialized only through the explicit,
  receipted `scratch_admin_extensions` allowlist described above; an unlisted
  one still fails with a hint. Ownership transfer to external roles cannot be
  materialized by the default restricted owner. Supporting that requires an
  isolated privileged-cluster execution boundary, not a quiet privilege
  escalation inside the shared scratch cluster.
- Release archives have SHA-256 checksums and GitHub build-provenance
  attestations. Homebrew verifies the selected archive checksum; consumers who
  require provenance verification must additionally use `gh attestation
  verify`.
- PostgreSQL's catalog surface is larger than the modeled preview boundary.
  The catalog-family and column-level ledgers classify the PostgreSQL 15–18
  surface, and live tests reject newly unclassified columns. Derived,
  environmental, runtime, and secret classifications remain explicit review
  boundaries rather than supported migration semantics.

Before a preview tag, rerun race tests, vet, static analysis, formatting,
PostgreSQL 15–18 integration tests, deterministic release builds, and the Go
vulnerability scan. Release archives must retain the repository's MIT
License.
