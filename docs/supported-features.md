# Supported features

The developer-preview comparison ledger is
[`parity/pgmig-roadmap.json`](../parity/pgmig-roadmap.json). It tracks onwardpg
against pgmig's public roadmap and is intentionally marked `in_progress`; it
is not an exhaustive onwardpg catalog inventory. The separate
[`parity/atlas-postgres.json`](../parity/atlas-postgres.json) is a pinned
reference-behavior study used to find regressions and design cases; it is not a
promise of one-for-one compatibility. The
[`parity/stripe-pg-schema-diff-v1.0.7.json`](../parity/stripe-pg-schema-diff-v1.0.7.json)
matrix indexes Stripe's complete pinned acceptance corpus and records observed
or still-unverified differences. The supported PostgreSQL server range is 14–18.

`implemented` means code and at least one project test exist. It does **not**
mean a feature is ready for unattended production use. A preview feature earns
stronger confidence only with current real-PostgreSQL convergence evidence.

For explicitly inventoried unsupported families, onwardpg blocks rather than
silently ignoring the object. Every PostgreSQL 14–18 catalog table is
classified, while the finer modeled-attribute audit remains open; see [the
safety model](safety-model.md) for that distinction and ignore-selector
semantics, and the [reference behavior
study](atlas-postgres-parity.md) for the separate Atlas research boundary.

## Dependency-aware work in progress

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

Ordinary views are catalog-modeled, including PostgreSQL-deparsed definitions,
reloptions, comments, and typed dependencies on referenced tables, columns,
views, enums, and modeled user routines. The planner supports create, `CREATE OR REPLACE` definition/options
changes, fingerprint-bound direct renames, and approved drops. For ordinary
dependent views, onwardpg performs the confirmed base rename first, then
reapplies each dependent desired definition in the `contract` phase; it does
not emit a statement referring to the new base name before that rename exists.
For a materialized dependent, onwardpg permits PostgreSQL's native retained
catalog rewrite only when a protected-token comparison proves its desired
definition differs solely in deparsed relation references. This handles the
PG14–18 variation in target-list qualification without rewriting literals,
comments, dollar strings, function calls, or arbitrary SQL. Any other
materialized-dependent definition change remains conservative because its
rebuild is destructive and needs a separately reviewed transition.
Materialized views support a fingerprint-bound direct rename. When every index
has an exact old→new relation identity match, onwardpg preserves those indexes
through the rename instead of dropping or rebuilding them, and applies any
index comment change afterward in `contract`; index definition changes remain
conservative. Materialized views remain catalog-modeled and support
create/drop. A definition/options change emits a fingerprint-bound destructive
rebuild question, then a reviewed drop/create `migrate` batch if approved.
Indexes on materialized views are graph-modeled and recreated after an approved
materialized-view rebuild. With the concurrent-index option, their rebuild is
emitted in a separate non-transactional batch after the transactional view
rebuild.

When an ordinary view definition or option change has a typed direct or
transitive materialized-view dependent, onwardpg requires a fingerprint-bound
`refresh_materialized_view` manual-work contract. Replacing the ordinary view
alone can leave stored materialized rows stale even though schema comparison
converges. The contract supplies the reviewed refresh statement (ordinary or
concurrent), execution mode, and optional data verification query; onwardpg
places it after the view replacement in a distinct `MANUAL` batch and never
guesses locking, refresh mode, or validation criteria. A materialized view that
is itself rebuilt does not receive a redundant refresh contract.
The same contract is required when a typed routine-body replacement can change
a materialized view's result: replacing a function changes behavior but not
the materialized definition or stored rows. A routine rename proven to retain
the same routine OID does not request a refresh.

For a confirmed column rename, onwardpg also recognizes PostgreSQL's native
rewrite of a direct ordinary or materialized dependent only for the strict
deparsed shape `SELECT column FROM relation`. PostgreSQL preserves the view
output name by rendering `SELECT new_column AS old_column`; PG14–15 may qualify
the projected column, which is handled without parsing arbitrary SQL. Any
expression, multi-column projection, alias, quoted identifier, or other
dependent-view change is a structured unsupported result rather than an
out-of-order `CREATE OR REPLACE` or guessed rebuild.

Functions, procedures, and triggers are graph-modeled. Their canonical
PostgreSQL definitions support create, replace/recreate, enable-state changes,
and approved drops; a trigger depends on both its table and its invoked
routine. Ordinary and materialized views also have typed catalog edges to
invoked user routines, so routine creation precedes dependent views and
approved drops remove views before their routine. A same-signature routine
rename requires a fingerprint-bound answer.
When that rename is the only change to a dependent materialized view's
protected, schema-qualified routine call, onwardpg retains the materialized
view through PostgreSQL's native OID-preserving rewrite rather than requesting
a destructive rebuild. Literals, bare calls, comments, and broader definition
changes do not satisfy that proof.
When its typed trigger dependents all remain in place and point at the desired
routine, onwardpg renames/redefines the routine first and reapplies each
trigger with `CREATE OR REPLACE TRIGGER`; other trigger lifecycle transitions
remain conservative. A behaviorally identical trigger rename also requires a
fingerprint-bound answer. PostgreSQL does not record arbitrary
procedural-body references, so
onwardpg does not rewrite a routine body for a column/table change or claim to
have inferred those hidden dependencies. Routine ownership remains an explicit
blocker. RLS state, policies, and ordinary/partitioned-table grants are typed
verticals: policy column/routine dependencies are catalog edges; policy and
authorization contractions require fingerprint-bound decisions; and role
identifiers are quoted with `PUBLIC` retained as the PostgreSQL keyword.
Default privileges, column grants, non-owner grant chains, and privileges on
other relation kinds remain explicit blockers.

A confirmed table rename treats its typed trigger children as PostgreSQL-retained
catalog objects: onwardpg emits only the table rename when the trigger’s table
association and its catalog `ON relation` clause are the sole changes. It
normalizes only that one deparsed clause, never text inside the trigger’s
routine or `WHEN` expression. Any actual trigger-definition or routine change
continues through its independent reviewed lifecycle.
Direct ordinary and materialized view dependencies are retained by the same
confirmed table rename when their catalog definitions differ only at protected
relation references. PostgreSQL preserves their relation identity and stored
materialized rows; a broader simultaneous dependent-view change is explicit
unsupported work, rather than a replacement ordered before the table rename.

A confirmed column rename likewise retains a typed trigger when PostgreSQL's
catalog rewrite is the only change: onwardpg recognizes only `UPDATE OF` column
lists and the trigger `WHEN` predicate. It never rewrites trigger identity,
relation targets, routines, or arbitrary expression text. A broader trigger
change remains an independent reviewed transition.

Partition children are graph-modeled. The planner supports an explicit attach
or detach of an existing range/list/hash/default child and marks the
lock/possible-scan hazards in the `migrate` phase. Moving a child to a
different parent, changing a bound, or changing a default partition requires a fingerprint-bound
`partition_reconfiguration` answer containing an operator-authored manual
work contract: a summary, explicit transactional/non-transactional execution
mode, reviewed SQL statements, and optional verification SQL. onwardpg emits
that contract in its own `MANUAL` batch and never invents
a detach/attach sequence, cast, or data movement. An acknowledgement without
the actual contract is rejected. Summaries and verification queries are
single-line receipt fields; verification queries execute only during
self-created clone verification and must each return one boolean `true` row.
Only explicitly supplied work statements change schema or data.

A nullable-to-`NOT NULL` transition offers `direct`, `staged`, and
`staged_with_backfill`. The last option asks a second fingerprint-bound
`backfill_not_null` question. Its application-owned SQL runs in the manual
phase after the `NOT VALID` guard is installed and before contract validation,
`SET NOT NULL`, and helper-constraint removal. onwardpg never derives the
backfill expression from the schema.

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
start/increment/min/max/cache/cycle, and removal. Removing identity is a
fingerprint-bound destructive question because PostgreSQL drops the owned
identity sequence; a replacement default is installed afterward in the same
`contract` phase.
