# Supported features

The developer-preview comparison ledger is
[`parity/pgmig-roadmap.json`](../parity/pgmig-roadmap.json). It tracks onwardpg
against pgmig's public roadmap and is intentionally marked `in_progress`; it
is not an exhaustive onwardpg catalog inventory. The
[`pgmig scenario-parity plan`](pgmig-parity-plan.md) pins pgmig's executable
API corpus and defines the work and evidence required for behavioral parity.
At commit `d2cccb6`, all 454 scenarios are evidence-linked: 405 verified, 25
served by clone-verified editable handoffs, and 24 intentional rejections, with
no implemented-but-unverified or unsupported-gap rows.
The separate
[`parity/atlas-postgres.json`](../parity/atlas-postgres.json) is a pinned
reference-behavior study used to find regressions and design cases; it is not a
promise of one-for-one compatibility. The
[`parity/stripe-pg-schema-diff-v1.0.7.json`](../parity/stripe-pg-schema-diff-v1.0.7.json)
matrix indexes Stripe's complete pinned acceptance corpus and records observed
or still-unverified differences. The supported PostgreSQL server range is 15–18.

`implemented` means code and at least one project test exist. It does **not**
mean a feature is ready for unattended production use. A preview feature earns
stronger confidence only with current real-PostgreSQL convergence evidence.

For explicitly inventoried unsupported families, onwardpg blocks rather than
silently ignoring the object. Every supported PostgreSQL catalog table is
classified, while the finer modeled-attribute audit remains open; see [the
safety model](safety-model.md) for that distinction and ignore-selector
semantics, and the [reference behavior
study](atlas-postgres-parity.md) for the separate Atlas research boundary.

## Dependency-aware work in progress

Enums support empty and labeled creation, comments, approved drops, inserted
values, pure positional label renames, and confirmed identity-changing
rewrites. A rewrite handles removal, reorder, and mixed rename-plus-insert by
retyping unchanged scalar/array columns through text, restoring defaults and
comments, and atomically dropping the old type. These paths converge on
PostgreSQL 15–18 and preserve retained values. Generated, indexed,
constrained, domain-mediated, view-read, or otherwise changed dependents remain
explicit refusals.
Extensions normalize their package-supplied default descriptions and model
only user-customized comments; custom comment changes converge on PostgreSQL
15–18 without creating default-comment drift. Version updates can be
selectively suppressed by repeating `--ignore-extension-version NAME`; an
unmatched name is intentionally harmless so one policy can span databases
with different extension sets. Identity-preserving schema moves use `ALTER
EXTENSION ... SET SCHEMA`. Both version-ignore branches and schema moves
converge on PostgreSQL 15–18.
Standalone sequences model logged/unlogged persistence, including unlogged
creation and both `SET LOGGED`/`SET UNLOGGED` transitions on PostgreSQL 15–18.
Temporary sequence persistence remains an explicit blocker.
Replica identity is a separate typed table child with `DEFAULT`, `FULL`,
`NOTHING`, and `USING INDEX` modes. The index form has an explicit dependency
on its unique index, so new-table plans create the table and index before
setting identity; resets to `DEFAULT` precede an old identity-index removal.
All pinned lifecycle shapes converge on PostgreSQL 15–18.
Domains model base type, non-default collation, default, nullability, named
CHECK constraints, validation state, comments, and typed dependencies. Create,
approved drop, default/nullability changes, check add/drop/rename, and comment
changes converge on PostgreSQL 15–18. Base-type or collation replacement is an
explicit `domain_base_type_change` boundary until dependent values can be
rewritten safely.
Composite types model ordered attributes, attribute types/collations,
comments, and nested or array-element dependencies. Empty and populated
creation, approved dependent-first drops, appended attributes, removals, type
changes with `CASCADE`, combined mutations, and comments converge on
PostgreSQL 15–18. Reordering retained attributes remains an explicit
`composite_attribute_reorder` rejection because PostgreSQL has no in-place
form that reproduces it.
Range types model their subtype, non-default collation and operator class,
subtype-difference function, canonical-function name, multirange name,
comments, and typed subtype/column dependencies. Creation, approved drops,
unchanged state, comment changes, and property-changing recreation converge on
PostgreSQL 15–18. A property rewrite is rejected with
`range_rewrite_dependents` while modeled dependents still consume the range;
custom canonical-function dependency choreography remains outside the verified
boundary.

Stored generated columns support expression replacement by dropping and
re-adding the generated column on PostgreSQL 15–18. PostgreSQL 18 virtual
generated columns support create, add, drop, and in-place expression changes.
Explicit same-type column collation changes and reset-to-default transitions
use reviewed `ALTER COLUMN ... TYPE ... USING` operations and converge across
the PostgreSQL 15–18 matrix.

PostgreSQL 18 `WITHOUT OVERLAPS` primary/unique constraints and `PERIOD`
foreign keys retain their catalog-rendered definitions and typed dependency
ordering. `NOT ENFORCED` check and foreign-key constraints are also modeled;
adds, unchanged state, both foreign-key enforcement transitions, check
rebuilds, temporal renames/rebuilds, and approved drops converge on PostgreSQL
18. These forms remain version-gated and are not emitted on older servers.

Standalone same-name index definition changes on ordinary tables,
materialized views, and independent local partition indexes support continuous
replacement when concurrent indexes are enabled: the typed old index receives a
deterministic temporary name, the desired stable name is built concurrently,
and the temporary index is dropped concurrently in `contract`. Every statement
has explicit lock/statement-timeout guidance, and each step is placed in a
valid transactional or non-transactional batch. Temporary-name conflicts are
checked across PostgreSQL's whole relation namespace. Partitioned-parent
replacement is also continuous for standalone nested trees: onwardpg retains
the valid old hierarchy, recursively creates `ON ONLY` shells, builds each
leaf concurrently, attaches bottom-up, and removes the old hierarchy only
after the new root becomes valid. Empty parents use the same shell swap.
Explicit per-key index collations (for example `COLLATE "C"`) are typed,
rendered, and included in that index identity. A collation change therefore
uses the same replacement path rather than being hidden as ambient database
state. The database or server default collation is not a schema operation:
onwardpg reports only explicit object semantics and never proposes changing an
environment default.
A prebuilt, structurally matching standalone child index can be attached to an
incomplete standalone partitioned parent with explicit lock/timeout metadata;
new local primary/unique constraints may also claim same-named matching unique
indexes before attaching them to a constraint-owned parent. Detach, reparent,
structural mutation, mismatched identities, and existing dependent local
constraints remain unsupported rather than inferred.
Ordinary-table primary-key and unique-constraint definition
changes without external dependents build a replacement unique index
concurrently, then swap the constraint to that index in one short contract
transaction; foreign-key dependents reject explicitly instead of receiving an
out-of-order drop. Isolated direct attached-child mutation, partitioned
constraints, and other dependent constraint-backed
variants remain explicit unsupported transitions until their complete
vertical slices can preserve PostgreSQL's attachment and ownership semantics.

Constraint names remain identity-bearing planner input. For an ordinary-table
constraint whose definition and backing index are otherwise identical,
onwardpg asks a fingerprint-bound `rename_constraint` question and then uses
`ALTER TABLE ... RENAME CONSTRAINT`. PostgreSQL's backing-index rename is
modeled as the same identity-preserving operation, and preserved constraint or
index comments are explicitly reconciled. Check, unique, exclusion,
`NULLS NOT DISTINCT`, and foreign-key variants converge on PostgreSQL 15–18.
Partition-propagated constraint renames and ambiguous candidates remain
outside this proof rather than being inferred.

Ordinary views are catalog-modeled, including PostgreSQL-deparsed definitions,
reloptions, comments, and typed dependencies on referenced tables, columns,
views, enums, and modeled user routines. The planner supports create, `CREATE
OR REPLACE` definition/options changes, and approved drops. It recognizes and
receipts semantic view-rename intent. After the rename choice, it asks for a
fingerprint-bound `rename_compatibility_bridge` handoff rather than emitting a
direct cutover. The generated contract file contains an annotated
`ONWARDPG TODO` with the bounded dependent-view analysis; the developer or
agent supplies the compatibility wrapper and verifies convergence.
For a materialized dependent, onwardpg permits PostgreSQL's native retained
catalog rewrite only when a protected-token comparison proves its desired
definition differs solely in deparsed relation references. This handles the
PG15–18 variation in target-list qualification without rewriting literals,
comments, dollar strings, function calls, or arbitrary SQL. Any other
materialized-dependent definition change remains conservative because its
rebuild is destructive and needs a separately reviewed transition.
Materialized-view rename intent and exact old→new index identity are modeled;
the same explicit compatibility-bridge handoff is used rather than a guessed
old-name strategy. Materialized views remain catalog-modeled and support
create/drop. A definition/options change emits an explicit destructive
rebuild question, then a reviewed drop/create contract batch if approved.
Indexes on materialized views are graph-modeled and recreated after an approved
materialized-view rebuild. With the concurrent-index option, their rebuild is
emitted in a separate non-transactional batch after the transactional view
rebuild.

When an ordinary view definition or option change has a typed direct or
transitive materialized-view dependent, onwardpg requires an explicit
`refresh_materialized_view` SQL handoff. Replacing the ordinary view
alone can leave stored materialized rows stale even though schema comparison
converges. Choosing `manual_sql` places an `ONWARDPG TODO` after the view
replacement. The developer or agent supplies the reviewed refresh statement
(ordinary or concurrent), batch boundary, and optional `verify.sql` assertion;
onwardpg never guesses locking, refresh mode, or validation criteria. A materialized view that
is itself rebuilt does not receive a redundant refresh contract.
The same contract is required when a typed routine-body replacement can change
a materialized view's result: replacing a function changes behavior but not
the materialized definition or stored rows. A routine rename proven to retain
the same routine OID does not request a refresh.

For a confirmed column rename, onwardpg also recognizes PostgreSQL's native
rewrite of a direct ordinary or materialized dependent only for the strict
deparsed shape `SELECT column FROM relation`. PostgreSQL preserves the view
output name by rendering `SELECT new_column AS old_column`; PostgreSQL may qualify
the projected column, which is handled without parsing arbitrary SQL. Any
expression, multi-column projection, alias, quoted identifier, or other
dependent-view change is a structured unsupported result rather than an
out-of-order `CREATE OR REPLACE` or guessed rebuild. Even the narrow recognized
case is not emitted as a bare rename. Eligible same-type columns use two
physical columns plus a bidirectional trigger through expand, then final
catch-up and a native identity-preserving rename in contract. Defaults,
generated/identity values, partitions, existing trigger ordering, RLS, and
unproven dependent rewrites block the automatic bridge.

Functions, procedures, and triggers are graph-modeled. Their canonical
PostgreSQL definitions support create, replace/recreate, enable-state changes,
comments, and approved drops; a trigger depends on both its owning relation and
its invoked routine. This includes ordinary and partitioned tables, constraint
triggers, and `INSTEAD OF` triggers on ordinary views. Ordinary and materialized
views also have typed catalog edges to
invoked user routines, so routine creation precedes dependent views and
approved drops remove views before their routine. A same-signature routine
return type is explicit graph state. A changed function return type can be
confirmed for a blocking drop/recreate while unchanged column defaults, CHECK
constraints, and expression indexes are removed and restored around it;
identical comments are reapplied. Catalog-recorded SQL `BEGIN ATOMIC` routine
dependencies also order chain and diamond drops. Dropped or changed
dependents, routine-on-routine return changes, and circular dropped-relation
shapes refuse explicitly. String-body SQL and procedural-language references
remain opaque because PostgreSQL does not record them in `pg_depend`.
A same-signature routine
rename requires a validated semantic hint and then an explicit editable
expand/contract wrapper handoff; onwardpg does not guess or directly apply a
routine cutover.
When that rename is the only change to a dependent materialized view's
protected, schema-qualified routine call, onwardpg retains the materialized
view through PostgreSQL's native OID-preserving rewrite rather than requesting
a destructive rebuild. Literals, bare calls, comments, and broader definition
changes do not satisfy that proof.
When typed trigger dependents all remain in place and point at the desired
routine, onwardpg can prove the required rewrite set and includes that evidence
in the editable compatibility handoff. A
behaviorally identical trigger rename requires a validated semantic hint and
is operational metadata rather than an application-callable identity.
PostgreSQL does not record arbitrary
procedural-body references, so
onwardpg does not rewrite a routine body for a column/table change or claim to
have inferred those hidden dependencies. Routine ownership remains an explicit
blocker. Ordinary and partitioned table ownership is typed; changes require a
fingerprint-bound authorization decision, use stable role quoting, and carry
authorization hazards. A narrow `table_owner:` ignore can suppress that one
attribute while preserving the table. RLS state, policies, and
ordinary/partitioned-table grants are typed verticals: policy column/routine
dependencies are catalog edges; policy and authorization contractions require
explicit semantic decisions; and role identifiers are quoted with `PUBLIC`
retained as the PostgreSQL keyword. Default privileges, column grants,
non-owner grant chains, non-table ownership, and privileges on other relation
kinds remain explicit blockers.

A confirmed, shape-preserving table rename is an expand/contract transition,
not a phase-labelled cutover. In expand, onwardpg preserves the old physical
table and creates a new-name `security_invoker` compatibility view exposing
the original ordered columns. In one transactional contract batch, it drops
that view and renames or moves the physical table to the desired identity.
View-capable `SELECT`, `INSERT`, `UPDATE`, and `DELETE` grants are copied to the
temporary new surface. Existing-column shape changes and table-only privileges
such as `TRUNCATE` block this automatic strategy rather than silently weakening
the compatibility window. New application code must also avoid view-incompatible
operations such as `INSERT ... ON CONFLICT` until contract.

Typed trigger children remain PostgreSQL-retained catalog objects when the
trigger’s table association and its catalog `ON relation` clause are the sole
changes. onwardpg normalizes only that one deparsed clause, never text inside
the trigger’s routine or `WHEN` expression. Any actual trigger-definition or
routine change continues through its independent reviewed lifecycle.
PostgreSQL-derived constraint and index names are normalized during table
rename candidacy. A declarative export may regenerate `old_table_pkey`,
`old_table_column_key`, or `old_table_column_idx` as names for the new table;
onwardpg keeps the rename choice and emits the necessary explicit catalog
renames in contract, because PostgreSQL itself retains those child names when
the relation is renamed. User-selected names remain material changes.
Direct ordinary and materialized view dependencies are retained by the same
confirmed table rename when their catalog definitions differ only at protected
relation references. PostgreSQL preserves their relation identity and stored
materialized rows; a broader simultaneous dependent-view change is explicit
unsupported work, rather than a replacement ordered before the table rename.

A confirmed column rename can still prove which typed triggers PostgreSQL
would rewrite: onwardpg recognizes only `UPDATE OF` column lists and the
trigger `WHEN` predicate. That analysis is included in the editable bridge
handoff but does not authorize a direct production rename. It never rewrites
trigger identity, relation targets, routines, or arbitrary expression text.

Development reconciliation is intentionally narrower and immediate. When an
unaccepted column was already applied locally and the declarative schema gives
it a new name, physical ordinal differences do not hide rename candidacy. A
dev-scoped confirmation emits one direct `ALTER TABLE ... RENAME COLUMN`;
choosing `preserve` retains the old local column. Durable H → W planning remains
trigger-backed expand/contract whenever accepted history actually contains the
old name.

Direct same-name column type changes are blocked with an
`expand_contract_type_bridge_required` result. A type change may still use the
explicit `manual_sql` handoff: the generated bundle remains incomplete
until the agent supplies reviewed SQL and clone verification proves the final
catalog. This preserves intricate product-aware migrations without allowing a
bare `ALTER COLUMN TYPE` to masquerade as an online rollout.

Column physical order is catalog state but PostgreSQL has no ordinary
`ALTER TABLE` operation that moves retained columns or inserts a new column in
the middle. onwardpg preserves the observed positions and reports stable
`column_physical_order:...` compatibility evidence, but does not treat visual
source-file order as semantic schema inequality. New columns are appended; a
developer who genuinely depends on positional rows must review that evidence.

Partition children are graph-modeled. The planner supports an explicit attach
or detach of an existing range/list/hash/default child and marks the
lock/possible-scan hazards in the contract phase. Moving a child to a
different parent, changing a bound, or changing a default partition requires
an explicit fingerprint-bound manual contract. The decision now carries a
non-executable, schema-aware runbook: a matching validated `CHECK` plus
detach/attach path for bounds, or a deterministic shadow hierarchy for
ordinary↔partitioned and key/strategy conversions. The latter enumerates the
complete old/desired trees, keys, indexes, partition locals, foreign keys,
triggers, policies, grants, sequence ownership, and typed view/materialized
view dependents; separates synchronization, copy, catch-up, assertions, and a
brief rename cutover; and leaves the populated old hierarchy for separately
fingerprint-authorized cleanup. Product-specific write synchronization and
conflict policy remain operator-authored. The runbook explicitly does not
treat clone convergence as evidence of acceptable production copy or cutover
time.

A nullable-to-`NOT NULL` transition offers `direct`, `staged`, and
`staged_with_backfill`. The last option hands the application-owned backfill to
an explicit contract SQL TODO after compatible writers are deployed. Contract
installs the `NOT VALID` guard, runs the backfill, validates, sets `NOT NULL`,
and removes the helper constraint. The guard is not placed in expand:
PostgreSQL enforces a `NOT VALID` check for new rows immediately, which could
break old writers. onwardpg never derives the backfill expression from the
schema.

Adding a new `NOT NULL` column without a default is always staged. Expand adds
the column as nullable so old writers and existing rows remain valid; contract
contains an editable backfill TODO and then enforces `NOT NULL` after old
writers drain.
The result remains `needs_sql_edits` until the agent supplies the product-aware
work. A new required column with a retained default can be added directly,
subject to the reported lock/rewrite hazards.

PostgreSQL's propagated parent/child indexes and constraints are graph-modeled
with typed child→parent edges. This includes primary/unique constraints and
inherited partition `CHECK` constraints: PostgreSQL omits `conparentid` for the
latter, so onwardpg creates the edge only when the catalogs prove one direct
partition parent with the same catalog-rendered constraint definition. Creation
emits only the parent operation; PostgreSQL creates the child resources as part
of the hierarchy. Dropping likewise asks once for each owning parent and lets
PostgreSQL tear down propagated children. A standalone parent index definition
change rebuilds through the parent only and lets PostgreSQL replace generated
children. A parent primary-key, unique, foreign-key, or exclusion definition
change follows the same parent-only rebuild rule. An inherited partition
`CHECK` likewise creates, rebuilds, and drops through its parent only.
Ambiguous or non-partition inherited `CHECK` constraints and independent child
mutation remain structured unsupported work until their full lifecycle policy
is implemented.

Partitioned exclusion constraints are a PostgreSQL 17+ capability; PostgreSQL
14–16 reject their creation before onwardpg can plan a transition.

Standalone sequences preserve `OWNED BY NONE` or a typed owning-column edge.
Create and mutation ordering ensures the owning column exists before ownership
is attached, and ownership is detached before a reviewed owner drop can cascade
the sequence. Sequence type/start/increment/min/max/cache/cycle and ownership
can change together. Identity columns support create/add, generation mode,
start/increment/min/max/cache/cycle, and removal. Removing identity is an
explicit destructive decision because PostgreSQL drops the owned identity
sequence; a replacement default is installed afterward in the same `contract`
phase.
