# Safety model

onwardpg is a planner, not a deployment agent. It never executes a plan on a
caller-supplied target. Clone verification is the only execution surface, and
it creates and destroys its own randomly named disposable databases.

The planner's core safety rules are:

- inspect live catalog state in a single `REPEATABLE READ, READ ONLY`
  transaction;
- materialize DDL in a disposable PostgreSQL database rather than parse a
  subset of SQL;
- block catalog state in the preview's explicit unsupported-family inventory
  unless a validated narrow ignore selector accounts for it;
- accept semantic hints for only the intent graph state cannot prove, then bind
  the generated internal receipt to both source and desired graph fingerprints;
- reject stale, impossible, contradictory, duplicate, and unused hints or
  internal answers;
- hand product-specific casts, backfills, refreshes, and niche operations to an
  explicit phase-local SQL TODO rather than accepting SQL inside decision JSON;
- never report an incomplete plan as converged: `needs_decisions`,
  `needs_sql_edits`, and `unsupported` are blocking states;
- require every mutable PR plan to have one explicit local PlanID; the
  preferred `plan` command excludes it and requires the remaining accepted
  history to form one valid chain before planning begins. The lower-level
  `draft --after` interface retains an exact predecessor assertion;
- retain declarative physical column-order differences as stable compatibility
  evidence without forcing dangerous replacement-table migrations;
- preserve execution constraints through explicit transactional batches; and
- surface destructive, lock, rewrite, validation, and availability concerns as
  statement safety/hazard metadata for review.

Every supported PostgreSQL `pg_catalog` table is classified in the developer
preview inventory. The attribute-level audit remains in progress. The current
blockers include domains, composites, aggregates, standalone collations,
range/multirange types, foreign tables, explicit ownership deviations,
non-table and column ACL/default-privilege state, non-owner grant chains,
replica identity, clustered or invalid indexes, relation and column physical
options, explicit relation tablespaces, traditional inheritance, rules,
text-search objects, event triggers, publications and subscriptions, extended
statistics, FDWs/servers/user mappings, custom access methods/operators/casts/
conversions/languages/transforms, security labels, and comments whose typed
object does not yet retain them. PostgreSQL 18's canonical generated `NOT
NULL` identities normalize to the existing column flag; custom/noncanonical
or commented `NOT NULL`, unenforced and period constraints, and virtual
generated columns are version-gated blockers.
Subscription connection strings and security-label values are never included
in diagnostics.
Extension-owned members are represented atomically by the typed extension
name/version/schema boundary and are not independently planned. The
machine-readable [catalog-family inventory](../parity/postgres-catalog-families.json)
records the per-major catalog-table evidence separately from its still-open
attribute audit. “No unsupported result” is not a catalog-completeness
certification until that second milestone closes.

RLS enable/force state, policies, and table privileges are modeled rather than
ignored. Graph edges place policies before RLS enable and RLS disable before
policy removal. Policy replacement, policy alteration, RLS relaxation,
privilege revocation, and removal of grant options remain reviewable and, when
destructive or authorization-relaxing, require an explicit semantic decision.
The generated internal receipt remains fingerprint-bound. Every emitted
authorization statement carries lock/statement timeout guidance; onwardpg
does not set those values on a caller session.

Product-specific SQL is developer/agent-owned and is never invented from
catalog state. Choosing `manual_sql` writes an explicit `ONWARDPG TODO` into the
relevant phase; the semantic hint itself cannot carry SQL. Every TODO must be
replaced before verification. Optional `verify.sql` postconditions must each
return one boolean `true` row during clone verification. Edited SQL and its
batch directives are receipted only after execution and convergence succeed.
Only an assertion explicitly marked `-- onwardpg:dev-postcondition` is ever
queried against a caller-owned development database, and it runs inside a
PostgreSQL read-only transaction. Its result is narrow evidence about a
historical data effect, never authorization to replay phase SQL or infer a
repair.
The read-only `verify --check` gate additionally recompiles current configured
DDL, requires the selected bundle to be the chain head, and compares its desired
fingerprint before clone execution. Self-consistency with a stale recorded
target is not sufficient.

The agent, not onwardpg, manages Git. The preferred `plan` command derives its
base by excluding the active local PlanID and validating the remaining
content-addressed chain; a fork, missing parent, descendant, or altered history
blocks. A missing active bundle is parked locally during a normal checkout
switch rather than overwritten. `history status` and `draft --after` retain an
exact `head_ref` boundary for expert diagnostics and compatibility. Target
lifecycle locks and final artifact comparison reject concurrent onwardpg
history forks or ordinary path-based edits during long clone verification. The
final commit point also rematerializes configured DDL, reloads
`.onwardpg.toml`, and rejects configuration or schema export state that changed
during planning or verification.
`history status` exposes the repository chain and selected relationship without
reading Git. If accepted history fully absorbs generated feature work, the
selected bundle is removed as `absorbed`; developer-owned SQL is never removed
by that inference.

The repository lifecycle lock is an operating-system advisory lock on the
existing `.onwardpg.toml` file itself. Physical-path aliases and cache settings
therefore resolve to the same lock inode without creating an untracked lock
artifact. It is released automatically when its process exits, so there is no
stale lock directory to delete and an old owner cannot unlock a replacement
inode. Atomic replacement of the config file is detected before commit. The
lock coordinates onwardpg processes, not editors. Do not save a
selected bundle while `draft`, `verify`, or `init` is running. A process that
already holds an open file descriptor to a file which onwardpg atomically
replaces can write to the detached inode after verification; no portable
filesystem protocol can attribute that late write to the new path. The command
post-validates the installed artifact, but concurrent external editing remains
an explicit unsupported operating condition rather than a claim of magic
locking.

Transactional batches are intended to be atomic execution boundaries. The real
PostgreSQL integration suite includes a failure case that proves an earlier
statement is rolled back when a later statement in the same batch fails.

An ignore selector is acceptance of a blind spot, not a declaration that the
ignored object is equivalent. A command-line `--ignore` must match at least one
of the two compared snapshots and the exact excluded objects are returned in
the result's `ignored` field. A target-level `.onwardpg.toml` `ignore` list is
for reviewed, persistent provider-owned state. `config check` validates those
selectors across authoritative DDL and the development catalog; a selector may
then be dormant in a history-to-working comparison when the object exists only
in development. Durable bundles receipt only configured selectors that
actually affected that bundle's graphs. Ignoring a schema does not recursively
ignore its contents.

The planner cannot prove data validity, safe casts, backfills, lock duration,
or application compatibility. Reviewers own those operational decisions. Test
the generated plan on a clone and require an empty residual diff after it is
applied.
