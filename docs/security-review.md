# Developer-preview security review

Reviewed 2026-07-13 against the developer-preview CLI boundary. No unresolved
critical security finding was identified. This is a scoped engineering review,
not a third-party audit.

## Trust boundaries

- Caller-owned PostgreSQL URLs are catalog-inspected in a repeatable-read,
  read-only transaction. onwardpg never applies migration SQL to them.
- DDL and migration SQL execute only in onwardpg-created disposable databases.
  The configured scratch role is therefore trusted with `CREATE DATABASE` and
  must not point at an application database.
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
- Release archives currently have checksums but no signatures or provenance
  attestations.
- PostgreSQL's catalog surface is larger than the modeled preview boundary.
  The catalog blocker inventory reduces silent equivalence risk, but the
  attribute-level audit remains ongoing and is documented as a preview limit.

Before a preview tag, rerun race tests, vet, static analysis, formatting,
PostgreSQL 14–18 integration tests, deterministic release builds, and the Go
vulnerability scan. Release archives must retain the repository's MIT
License.
