# Ownership change report — 2026-09-26

This collects the documentation work, ownership audit, release hardening, and
framework adoption study. Detailed reproductions are linked below. The study
concluded after five rounds with 15 fresh agents at the user’s request; the
planned two clean confirmation rounds were not reached.

## Verdict and scope

onwardpg is useful when old and new application versions share PostgreSQL during
a rollout. Its value is the combination of phased SQL, explicit decisions,
editable bundles, and verification against replayed history. It remains a
developer preview requiring reviewed application deployment.

This work did not turn onwardpg into a production SQL executor or a framework
migration-journal converter. The deployment runner still owns application order,
writer drainage, batch execution, and operational limits.

## Documentation and website

- Replaced the documentation site with one narrative quick start and compact
  sections for required columns, everyday use, deployment, and reference links.
- Added “Why do I need this?” with rolling renames, required fields, changing
  CHECK values, and type changes beneath views and indexes.
- Updated Blume to 2.0.3 and retained generated CLI help and agent discovery/docs.
- Kept the 51-line README unchanged during adoption work. The website remains
  one page, with the same `init` → `plan` → edit SQL → `verify` quick start.
- Explained baseline adoption before model edits, separate development
  decisions, and why a verified durable bundle can coexist with exit code 2.
- Added complete Drizzle, Django, and Prisma adoption recipes, including
  framework runner handoff and fresh-database bootstrap.
- Documented exact journal exclusions, saved observer policy, ORM null handling
  during overlap, observer access, and named cleanup before contract enforcement.
- Clarified that batch comments do not create transactions in psql, and that a
  named reconciliation batch can precede an earlier cutover batch in execution.
- Expanded the documentation checker to cover framework recipe Markdown.

## Security and execution safety — preview.3

- Replaced assertion paths with one read-only executor. It requires exactly one
  non-null Boolean row and column, rejects statement lists, and describes the
  result shape before execution so transaction commands cannot escape the check.
- Added savepoint handling for assertions over uncommitted migration work,
  restoring the caller's settings and transaction after checks.
- Added database-wide RLS visibility preflight, covering forced owner RLS and
  functions that change row-security settings. Unrelated hidden-row tables block
  checks too.
- Enforced declared batch boundaries: transactional work cannot commit, replace,
  or roll back its transaction; nontransactional work must leave the connection
  idle.
- Limited schema input to 64 MiB; added exporter deadlines, live output-size
  checks, process-group containment, and native Windows job containment. Kept
  regular-file exporter output and changed checkout hashing to stream files.
- Bounded scratch cleanup, reconnecting after connection loss, retaining cleanup
  errors alongside the original failure, and identifying exact resources for retry.
- Isolated each pinned Stripe reference run's temporary resources by exact
  owning role and made incomplete reference cleanup visible.
- Updated Go to 1.26.8 and `golang.org/x/text` to 0.42.0 after four reachable
  vulnerability advisories were reported. The subsequent Go scan was clear.

See [the ownership audit](ownership-review.md), [security review](security-review.md),
[exporter limits](exporter-limits.md), and [scratch recovery](scratch-recovery.md).

## Catalog and CLI fixes from adoption

- Ignored tables now exclude their owned replica identity, triggers, policies,
  RLS metadata, and grants. Unmodeled missing-table errors remain errors.
- A retained application foreign key into an ignored table requires an explicit
  decision to exclude its dependent constraint.
- Added digest-bound `planner.observer_ignore_selectors` for exact live-only
  exclusions observed during planning. Clone-schema exclusions stay separate.
- Observer-only receipts reject wildcards. Contract checks derive their policy
  from the validated bundle, so later configuration changes cannot widen it.
  Existing bundles preserve their previous behavior.
- Development planning, raw diff, and drift checks align asymmetric ignore
  receipt metadata. Real objects, dependency edges, unsupported markers, and
  stored durable fingerprints retain their meaning.
- Successful development output retains the observed exclusion evidence.
- Explicit ordinary-schema ACLs equal to PostgreSQL's defaults no longer cause
  false unsupported findings after grants are revoked. Public-schema semantics,
  extension-recorded defaults, grant options, revocations, and ownership changes
  remain checked.
- CLI output explains invalidated answers while they remain unanswered.
- Manual cleanup questions distinguish existing generated data gates from
  situations requiring a custom Boolean assertion.
- Suggested commands carry valid confirmed decisions and the selected target
  through edited-bundle revisions. A native PostgreSQL regression follows the
  emitted commands sequentially and verifies that waiting leaves edited SQL intact.

## Framework exporters and exercises

- Drizzle and Prisma examples use installed local executables with npm or pnpm;
  the Drizzle exporter no longer creates an unrelated `app` schema.
- Django's exporter rejects model changes absent from migration state, documents
  its export-only database alias, reuses the scratch admin URL by default, and
  supports selecting a matching `pg_dump` with `PG_DUMP`.
- Django's recipe covers state-only migrations, historical data/SQL operations,
  migration signals, and test-database bootstrap limitations.
- Prisma's recipe separates client generation from DDL export and documents
  the reproduced required-field/runtime-null behavior during overlap.
- Added a reproducible sandbox helper with separate populated PostgreSQL
  containers, random local credentials/ports, copied binaries and public docs,
  binary hashes, command evidence, and ownership-checked teardown.
- Switched Drizzle cohorts to matching ORM/Kit 1.0.0-rc.4 at the user's request.
  Other fixtures use Django 5.2.17, psycopg 3.3.6, Prisma 7.10.0, and PostgreSQL 18.
- Fresh agents receive public docs and a sandbox task, with instructions to avoid
  implementation code and prior findings. They exercise baseline adoption, plan revision, actual old/new ORM clients,
  per-batch execution, live readiness, data preservation, drift, and development
  planning. This is instruction-level blinding, not an OS sandbox.

See [the cohort-by-cohort study](adoption-study.md) and
[sandbox instructions](../scripts/adoption-sandbox.md).

## Validation and release process

- Added adversarial assertion, transaction, RLS, process, cleanup, catalog,
  observer-policy, comparison, and CLI regression tests for reproduced failures.
- Added mandatory compiled-CLI rehearsals for populated rename overlap and
  blocked transactional DDL cancellation/retry, with exact counter preservation.
- Release publication now requires PostgreSQL 15–18 native/differential and
  compiled-CLI acceptance gates, native Windows exporter tests, PGlite preflight,
  race tests, vet, staticcheck, vulnerability scanning, and documentation checks.
- Release archives remain deterministic across six OS/architecture targets,
  with embedded versions, checksums, provenance attestations, and a generated
  Homebrew formula. Preview releases use reviewed changelog notes.
- Preview.3 was published from `601cd78`; its Homebrew formula and documentation
  site were updated. Preview.4 collects the adoption fixes above; publication
  remains gated by the full release workflow.

## Remaining limits

Passing clone verification and these small fixtures does not establish
production-scale lock duration, backfill cost, replication health, or real writer
drainage. Assertions and exporter code remain trusted project inputs; resource
limits and read-only SQL are not OS isolation. Process death can still require
explicit scratch recovery.

The study uses simulated users and small schemas. Broader Django applications,
third-party migrations, application caches, prepared `SELECT *`, positional
inserts, and production deployment integrations need their own rehearsals.
Prisma fixture installs reported external npm dependency advisories; those
packages are outside the released Go CLI and are recorded in the study.

Repeated remaining feedback concerns the separate durable/development workflows
and the length of explicit JSON hints. Deliberate decision gates and documented
framework null behavior are distinguished from implementation defects.
