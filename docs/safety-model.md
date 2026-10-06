# Safety model

The CLI never applies migration phase SQL to a caller-supplied target. Clone
verification executes phases in randomly named disposable databases. Live
contract gates and opt-in development postconditions run Boolean assertions
through a separate read-only boundary.

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
  explicit phase-local SQL TODO or a typed, fingerprint-bound operator work
  contract;
- never report an incomplete plan as converged: `needs_decisions`,
  `needs_sql_edits`, and `unsupported` are blocking states;
- require every mutable PR plan to have one explicit local PlanID; the
  preferred `plan` command excludes it and requires the remaining accepted
  history to form one valid chain before planning begins. The lower-level
  `draft --after` interface retains an exact predecessor assertion;
- retain declarative physical column-order differences as stable compatibility
  evidence without forcing dangerous replacement-table migrations;
- preserve execution constraints through explicit transactional batches; and
- require every contract enforcement statement to name exact data/writer gates
  or a narrow typed catalog proof; and
- surface destructive, lock, rewrite, validation, and availability concerns as
  statement safety/hazard metadata for review.

Every column of every PostgreSQL 15–18 `pg_catalog` table is classified in the
checked-in attribute ledger as modeled, blocked, derived, environmental,
runtime, or secret. Live-version tests reject catalog shape drift. Supported
families and their exceptions are recorded in the inventories linked below.
For example, aggregates, foreign tables, traditional inheritance, event
triggers, and logical replication configuration block planning. Domains,
composites, ranges, and PostgreSQL 18 virtual generated columns have modeled
paths; their support does not imply that every alteration is automatic.
Subscription connection strings and security-label values are never included
in diagnostics.
Provider-owned state in a live cluster has one narrow exception. Ownership of an
extension or a schema by a role other than the inspecting role, and
`pg_parameter_acl` grants, remain blockers for every command, and the attribute
ledger keeps them classified as blocked. A target's `live_ignore` list can
acknowledge an exact selector for commands that read a live catalog
(`drift check`, `contract check`, and `diff --target`). The list removes only
that blocker marker, never a typed object, and it is not applied to replayed
history, DDL sources, planning, or verification. It is not a default: a new
parameter grant or a changed owner is a different selector and blocks again, and
the acknowledged state is reported as `observer.live_ignored`. A parameter ACL
or a foreign-owned schema can be a real privilege difference between
environments, so onwardpg does not reclassify it as environmental.
Extension-owned members are represented atomically by the typed extension
name/version/schema boundary and are not independently planned. Physical
member addresses alias to that Extension node for dependency ordering.
Ordinary-view column defaults and comments on domain constraints, composite
attributes, and view columns remain explicit blockers. Pending concurrent
partition detaches and exceptional PostgreSQL 18 NOT NULL inheritance also
fail closed rather than masquerading as ordinary topology. A PostgreSQL 18
NOT NULL constraint with a name PostgreSQL itself generates for its table and
column, including the 63-byte shortening and the `not_null1`, `not_null2`, ...
collision suffix, is represented by the column alone and its name is never
compared. A custom name, or a generated name left on a renamed table or column,
blocks as `not_null_constraint:`; `ALTER TABLE ... RENAME CONSTRAINT` to the
current default clears the latter. Customized options,
comments, expressions, persistence, or names on implicit serial and identity
backing sequences also block when their state is not retained by the typed
column. The
machine-readable [catalog-family inventory](../parity/postgres-catalog-families.json)
and [attribute ledger](../parity/postgres-catalog-attributes.json) record the
per-major evidence. Classification proves that the surface was considered; it
does not turn derived or out-of-scope state into a supported migration target.

Dependency order is checked across phases as well as inside the graph schedule:
new work depending on a contract-phase provider is promoted to contract.
Retained expression/partial indexes, stored generated columns, and constraints
depending on a semantically changed routine block because PostgreSQL will not
rebuild, recompute, or revalidate their stored state automatically. Cross-kind
replacement inside PostgreSQL's shared relation/type namespaces also blocks
until it has explicit drop-before-create compatibility choreography.

RLS enable/force state, policies, and table privileges are modeled rather than
ignored. Graph edges place policies before RLS enable and RLS disable before
policy removal. Creation, alteration, or tightening on an existing table runs
in contract after old application traffic drains; a policy change stays before
dependent RLS enable/force work. Policy replacement, policy alteration, RLS
relaxation, privilege revocation, and removal of grant options remain reviewable
and, when destructive or authorization-relaxing, require an explicit semantic decision.
The generated internal receipt remains fingerprint-bound. Every emitted
authorization statement carries lock/statement timeout guidance; onwardpg
does not set those values on a caller session.

Product-specific SQL is developer/agent-owned and is never invented from
catalog state. Choosing `manual_sql` writes an explicit `ONWARDPG TODO` into the
relevant phase. Typed operator work can also carry reviewed statements and
verification queries. Every TODO must be replaced before verification. Optional
`verify.sql` postconditions and manual verification queries must each return
exactly one non-null PostgreSQL Boolean value, `true`, in a read-only transaction
or savepoint. Multiple statements and extra rows or columns are rejected even
when the connection defaults to simple protocol. Queries fail if row-level
security applies to the current role anywhere in the database, even when the
assertion does not use that table. Parse/Describe rejects transaction commands
before execution. The assertion transaction is always rolled back. Functions
that change execution identity or cause external effects still require review. Edited SQL and its
batch directives are receipted only after execution and convergence succeed.
Only an assertion explicitly marked `-- onwardpg:dev-postcondition` is ever
queried against a caller-owned development database, and it runs inside a
PostgreSQL read-only transaction. Its result is narrow evidence about a
historical data effect, never authorization to replay phase SQL or infer a
repair.

An editable transition is still dependency-bounded. A same-type rename with
an unproved dependent-view change keeps the generated column bridge and adds
three ordered pockets for the overlap view, pre-cutover removal, and exact
desired recreation. A confirmed cross-name/type transition instead owns both
endpoint columns and its whole current/desired dependency closure in exactly
two pockets, one per phase. In both cases the pocket text names that closure,
and verification rejects unresolved TODOs or a final catalog that does not
converge. This is not permission to place unrelated migration work inside the
pocket.

Ordinary-view replacement has a separate structural guard: onwardpg emits
`CREATE OR REPLACE VIEW` only when the existing output prefix retains the same
names, order, and PostgreSQL type identities. It may append compatible outputs;
it never uses replacement to rename an existing output column. Materialized
views are not passed through that shortcut: their rebuild and freshness
semantics remain explicit reviewed work.

The read-only `verify --check` gate additionally recompiles current configured
DDL, requires the selected bundle to be the chain head, and compares its desired
fingerprint before clone execution. Self-consistency with a stale recorded
target is not sufficient.

`onwardpg contract check` is a second, intentionally separate read-only
surface. It validates the bundle head, compares a caller database with the
receipted post-expand graph, and evaluates data gates plus expiring writer
attestations inside one repeatable-read snapshot. It has no code path that
loads or executes phase SQL. Its result cannot replace the inline assertion in
contract SQL: readiness feedback can become stale between observation and
execution.

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
