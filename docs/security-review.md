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
- `schema_command` is trusted project code, invoked directly without a shell.
  It is checked for deterministic output. It is not an operating-system
  sandbox, and onwardpg does not check what the command writes; in a git work
  tree it reports files whose status changed as a non-blocking warning. See
  [the schema export and the checkout](safety-model.md#the-schema-export-and-the-checkout).
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
  The monitor is not a strict disk quota;
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
- Superuser-only extensions and ownership transfer to external roles cannot be
  materialized by the default restricted owner. Supporting those declarative
  inputs requires an isolated privileged-cluster execution boundary, not a
  quiet privilege escalation inside the shared scratch cluster.
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
