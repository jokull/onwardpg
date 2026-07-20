# Developer-preview security review

Reviewed 2026-07-15 against the developer-preview CLI boundary. No unresolved
critical security finding was identified. This is a scoped engineering review,
not a third-party audit.

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
- DDL export output, configuration files, and captured stderr are bounded;
  JSON input rejects unknown and duplicate keys.
- Transactional verification rolls back failed batches. Non-transactional
  failures are reported as partial application inside a disposable database,
  which is then destroyed.
- Unknown catalog families block planning unless a validated narrow ignore
  selector matches them; ignored state is reported explicitly.
- The dependency scan is clean with Go 1.26.5 and pgx 5.9.2; CI and the release
  workflow rerun the pinned `govulncheck` command.

## Residual risks and release gates

- A malicious `schema_command` has the authority of the onwardpg process. Run
  only repository-controlled export commands, ideally in an isolated CI job.
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
