# Blind framework adoption study

Started from `v0.1.0-preview.3` on 2026-09-26. Fresh agents adopted onwardpg in
existing Drizzle, Django, Prisma, and SQL-first projects. These are agent
simulations, not observations of human users.

## Method

Each user received a separate project, PostgreSQL container, binary, and snapshot
of public documentation. Agents had no inherited conversation and were told not
to inspect onwardpg source, earlier findings, or other sandboxes. A coordinator
collected evidence and assigned fixes separately. This is instruction-level
blinding; agents still had host tool access.

Projects started with 100 customers and, for frameworks, a real applied baseline
migration and journal. The change was a rolling `display_name` → `full_name`
rename plus required `account_status`, with `active` for existing customers.
Users configured export, initialized history, answered decisions, edited cleanup,
revised the plan, verified, and rehearsed overlap and contract on populated data.
Later cohorts also exercised actual ORM reads/writes and live readiness.

The planned stopping rule was two consecutive fresh cohorts without new substantive
onboarding blockers, misleading diagnostics, or unsafe workflow ambiguity.
Minor preferences and deliberate safety questions are recorded separately.

## Findings addressed

| Finding | Change |
| --- | --- |
| Ignoring Django's journal made catalog inspection fail on replica identity; related owned metadata had the same problem | Skip metadata only for tables actually excluded by a matched table selector; retain missing-table failures elsewhere |
| Live-only journal exclusions disappeared from saved policy, so contract readiness still reported catalog drift | Bind exact observed development-only exclusions separately from clone-schema exclusions; contract uses saved policy |
| Drift, development planning, and raw diff treated asymmetric ignore receipts as schema differences | Align receipt evidence at comparison entry points; keep durable fingerprints and real graph differences unchanged |
| Revoking observer grants left an explicit default ACL on a custom schema, causing a false unsupported finding | Recognize exact default-equivalent custom-schema ACLs; retain real grants, revocations, and ownership changes |
| The initial Drizzle RC recipe excluded its table-owned serial sequence separately, which failed unused-selector validation | Keep table/schema selectors; add sequence selectors only for separately reported sequences |
| Django exporter setup required undocumented settings and a separate admin environment variable | Complete settings and client recipe; reuse the scratch URL by default; configurable matching `pg_dump` |
| Edited Django models could silently export old migration state | Exporter checks for missing migration state before producing DDL |
| Framework migration ownership was underspecified | Concrete Drizzle, Django, and Prisma handoff and fresh-database guidance |
| Export examples assumed pnpm and an `app` schema | Local executable recipes for npm/pnpm; public-only Drizzle export has no extra schema |
| Edited-bundle follow-up commands lost newly confirmed decisions | Carry current valid hints and the selected target in complete suggested commands; preserve edited SQL while waiting |
| Replanning repeated a rename question without explaining the invalidation | CLI identifies a changed question scope, only while that decision remains unanswered |
| Manual cleanup prompt demanded another assertion although a generated gate existed | Distinguish generated enforcement checks from required custom checks |
| Journal selectors were confused with colon-separated plan object IDs | Document `table:public.django_migrations` spelling and when the live boundary is validated |
| A statically required Prisma field returned a runtime null during overlap | Framework-specific read/write guidance, backed by a Prisma 7.10 client probe |
| Readiness status list omitted `reconciliation_required` | Document the cleanup-before-enforcement sequence |
| Initial users approximated batch boundaries with whole-file execution | Explain that psql does not interpret batch comments; later rehearsals execute batches separately |

## Cohort evidence

Round 1: SQL-first, Drizzle, and Django all verified and preserved their original
records. They reported exporter and workflow gaps. Their whole-file SQL
rehearsals did not faithfully reproduce every transaction boundary.

Round 2: fresh Drizzle and Prisma users completed per-batch rehearsals; the
Prisma user also probed generated-client null behavior. The fresh Django user
preserved all records and verified the bundle, but live contract readiness was
blocked by the journal inspection/policy defects above. That is a failed
readiness outcome, despite the successful SQL rehearsal.

Round 3: fresh Drizzle 1.0 RC, Django, and Prisma users all verified, exercised
actual old/new ORM reads and writes, reached `reconciliation_required` then
`ready` with restricted observers, and completed contract per batch. Each kept
all 100 original customers and ended with 103 rows including probe inserts.
They found two recipe defects: the redundant Drizzle sequence exclusion and
missing Prisma journal configuration. Both are corrected. The contract reference
also clarifies named cleanup ordering and observer access to ignored journals.
This round does not count toward the plateau.

Round 4: fresh Drizzle, Prisma, and Django users completed ORM overlap,
per-batch application, verification, and live readiness with all original
customers preserved. Django then found a development-planning error after the database
was drift-free: asymmetric journal ignore receipts appeared as an unsupported
dependency-only difference. That finding resets the plateau count. Its first
report of a missing Django recipe was retracted after the agent found the
provided files; it was a navigation error. Drizzle also exposed an explicit
default schema ACL left after revoking observer grants; that representation was
incorrectly treated as unsupported. Genuine grants to other roles remain visible
when inspection uses a database-owner connection.

Round 5: Drizzle and Prisma completed without new substantive findings. Django
completed the migration but exposed a command-handoff gap when revising an
already edited bundle: newly confirmed decisions were not yet saved, and the
next command did not carry them forward. Supplying every hint together worked;
following individual suggested choices could repeat earlier questions. That
finding resets the plateau count.

The study stopped at the user’s request after five rounds (15 fresh agents).
The final decision-handoff defect is fixed and covered by a native PostgreSQL
regression: sequential suggested commands retain confirmed hints and the selected
target while preserving edited SQL. Two additional cohorts had been provisioned
but were not run. The planned plateau was not reached.

## Validation

The affected native PostgreSQL 17 packages pass, including the full CLI package
and init → plan → verify → real expand → live readiness regression. Tests cover
journal/data preservation, ignored-table metadata, explicit incoming foreign-key
boundaries, strict unused-selector rejection, unavailable development databases,
exact wildcard receipt narrowing, bundle integrity, and unchanged RLS safeguards.
The full Go race suite and a final affected-package race run pass, as do vet,
staticcheck, govulncheck, documentation validation, and the website type check.
Release gates caught two older native tests expecting follow-up commands without
the newly retained target, then a stored SQL receipt containing the old cleanup
prompt. Their expectations and receipts were corrected. The full native
PostgreSQL 18 CLI package passed; documentation workflows were rerun separately.

## Reproduce and inspect

Use [the sandbox helper](../scripts/adoption-sandbox.md) to create a new project
and give it to an agent with no inherited context. Fixture versions are
drizzle-kit 0.31.11 and drizzle-orm 0.45.3 in rounds 1–2, then matching
1.0.0-rc.4 packages from round 3 at the user’s request; Django 5.2.17,
psycopg 3.3.6, Prisma 7.10.0, and PostgreSQL 18. The catalog-ignore regression also runs on PostgreSQL
17 during development; the normal CI matrix covers 15–18.

Original commands, feedback, generated bundles, and available database dumps are
retained locally under `/tmp/onwardpg-adoption-20260926`. Each sandbox's
`evidence/` directory separates user mistakes, expected failures, and product
findings. Provisioning logs record framework versions and setup failures. The round-2
Django final dump failed due to a host client version mismatch before cleanup;
its data-preservation evidence is in the command transcript. Later cleanup uses
the container’s matching client.

## Limits

These are small single-table adoption fixtures. They do not establish behavior
for a complete production application, third-party Django data migrations,
framework test runners, large backfills, replication lag, or real deployment
provider evidence. A plateau here means the tested onboarding paths stopped
yielding substantive new findings; it does not mean the library has no bugs.

Prisma's fixture dependency install reported four high-severity npm audit entries
in its config/MySQL dependency tree. The raw audit is retained as
`prisma-fixture-audit.json`; this study does not certify framework dependency
security. Those Node packages are not part of the released Go CLI.
