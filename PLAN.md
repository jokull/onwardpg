# PostgreSQL compatibility hill climb

## Outcome

Earn a strong context-blind PostgreSQL review by closing correctness gaps in
modeled schema dependencies, proving catalog-attribute coverage, and making
operational assumptions impossible to miss.

This is not a plan to claim support for every PostgreSQL feature. A good
outcome is:

- modeled objects carry every catalog-proven dependency needed for correct
  create, change, and drop ordering;
- every schema-relevant catalog attribute is modeled, explicitly blocked, or
  deliberately classified outside the database-schema boundary;
- generated operations say what application and operational assumptions they
  require;
- scratch execution is clearly isolated from cluster-global authority; and
- a fresh specialist can inspect only the README, source, and tests and find
  no unmitigated high-severity schema-compatibility gap.

## Architectural verdict

The core architecture should stay.

~~~text
real PostgreSQL catalogs
        |
        v
typed objects + dependency edges
        |
        +--> semantic diff
        +--> dependency-safe create / reverse drop
        +--> explicit decisions and editable work
        +--> disposable-clone convergence
~~~

The review findings land at existing extension points. The first implementation
pass has now exercised each one:

| Concern | Existing way forward | Revision needed |
| --- | --- | --- |
| Missing dependency edges | Snapshot accepts arbitrary typed edges; the scheduler and closure planners already consume them | Implemented as one post-inspection catalog-address index and normal-dependency projection pass |
| Attribute blind spots | Typed fields represent supported semantics; unsupported selectors fail closed | Implemented as a generated PostgreSQL 15–18 column ledger, live drift tests, and initial semantic probes |
| Add-column application risk | Statements already carry stable hazard metadata | Implemented with `application_row_shape_change` on every existing-table addition |
| Aggressive rename backfill | Fingerprint-bound manual work, edited phase SQL, assertions, and batch boundaries already exist | Implemented as explicit manual, single-transaction, or split-plan strategy |
| Scratch trust | Materialization is already isolated at the database boundary | Implemented with a short-lived restricted database owner created by the scratch administrator |

No public graph rewrite, SQL parser, deployment engine, or new migration phase
is required. The one architectural change belongs inside catalog inspection:
physical PostgreSQL identities must be projected consistently onto logical
onwardpg objects after all objects have been discovered.

## Hill-climb method

Work in small vertical slices. Each slice begins with a PostgreSQL fixture that
demonstrates the gap, ends with PostgreSQL 15–18 convergence evidence, and is
then offered to a fresh reviewer without this plan or PR context.

~~~text
reproduce -> classify -> fix or block -> converge on PG15–18 -> blind review
     ^                                                        |
     +-------------------- next highest-risk finding <---------+
~~~

Do not optimize for the number of supported features. Optimize for the absence
of silent equality and invalid ordering inside the claimed boundary.

## Climb 1: make dependency completeness architectural

### Developer failure

A developer adds or removes two individually supported objects, but onwardpg
orders them incorrectly because PostgreSQL recorded a dependency that the
source inspector did not project into the graph.

Representative fixtures:

~~~sql
CREATE FUNCTION app.is_positive(integer) RETURNS boolean
LANGUAGE sql IMMUTABLE RETURN $1 > 0;

CREATE DOMAIN app.positive_integer AS integer
CHECK (app.is_positive(VALUE));
~~~

~~~sql
CREATE FUNCTION app.timestamp_distance(timestamptz, timestamptz)
RETURNS double precision
LANGUAGE sql IMMUTABLE RETURN EXTRACT(EPOCH FROM ($1 - $2));

CREATE TYPE app.time_window AS RANGE (
  subtype = timestamptz,
  subtype_diff = app.timestamp_distance
);
~~~

~~~sql
CREATE TYPE app.state AS ENUM ('open', 'closed');

CREATE FUNCTION app.normalize_state(app.state) RETURNS app.state
LANGUAGE sql IMMUTABLE RETURN $1;
~~~

### Architecture

Use a source-internal catalog identity index:

~~~text
(classid, objid, objsubid) -> logical pgschema.ID
~~~

One final catalog query runs after every typed inspector and maps physical
addresses only when the corresponding logical ID exists. Embedded catalog
objects map to the logical object that owns their lifecycle:

- a domain CHECK constraint maps to its Domain ID;
- a column default maps to its Column ID and, where required by existing
  lifecycle behavior, its Table ID;
- a view rewrite rule maps to its View or MaterializedView ID;
- index, constraint, trigger, policy, routine, type, relation, and column
  addresses map directly to their typed IDs.

After every modeled object has been loaded, run one dependency projector over
pg_depend. It must:

1. translate allowlisted dependency rows whose endpoints are both modeled;
2. add the logical edge once, regardless of how many catalog rows prove it;
3. preserve hand-authored semantic edges that pg_depend cannot express, such
   as rollout ordering and selected ownership/topology relationships;
4. classify extension-owned, system, automatic, and internal dependencies
   instead of importing them blindly;
5. leave unsupported user-schema endpoints to the catalog-family blockers and
   test the admitted modeled-object address corpus for complete projection; and
6. keep arbitrary procedural-body references outside the proof, because
   PostgreSQL itself does not catalog them.

Do not add dependency provenance to canonical graph fingerprints. Provenance
is inspection evidence; logical edge identity is the schema contract.

### First closures

Implement and prove, in order:

1. domain constraint and domain default dependencies on routines and modeled
   types;
2. range subtype-difference dependencies and retained canonical-function
   edges; a custom canonical lifecycle remains an explicit renderer blocker
   because a generic SQL function cannot accept PostgreSQL's shell range type;
3. routine argument, return, OUT-parameter, variadic, array-element, domain,
   composite, enum, and range type dependencies;
4. expression dependencies from column defaults, generated expressions,
   constraints, indexes, policies, and view rules onto modeled routines and
   types; and
5. create and drop ordering when these edges form mixed chains or cycles.

### Evidence gate

- Source integration tests assert the exact projected edges.
- Planner integration tests create the dependency before its consumer and
  drop the consumer before its dependency.
- Each fixture converges with an empty residual on PostgreSQL 15, 16, 17, and
  18.
- A negative fixture proves an opaque PL/pgSQL string reference is neither
  claimed nor rewritten.
- An inventory test fails if a relevant mapped object has an unclassified
  pg_depend relationship.

## Climb 2: move from catalog-table coverage to attribute coverage

### Developer failure

Two schemas differ in a schema-relevant attribute, but the typed snapshots
compare equal because the catalog table was classified while that attribute
was not.

The first concrete probe is auxiliary TOAST state:

~~~sql
CREATE TABLE app.events (payload text);
ALTER TABLE app.events
  SET (toast.autovacuum_enabled = false);
~~~

### Architecture

Add a checked-in, versioned PostgreSQL 15–18 attribute ledger. For every column
of every in-scope pg_catalog relation, record one classification:

- modeled: retained in a typed object and tested for equality;
- blocked: detected and emitted as a narrow unsupported selector;
- derived: intentionally represented by another stable catalog fact;
- environment: required for materialization but not a migration target;
- runtime: data, statistics, or transient state outside schema comparison;
- secret: intentionally neither retained nor printed; or
- not_applicable: irrelevant to the object shapes onwardpg admits, with a
  reason.

Live matrix tests must compare the ledger with pg_attribute on each supported
major. A new or removed catalog column then fails CI until classified.

Start the semantic audit with the catalogs carrying the claimed core:

1. pg_class and auxiliary relations, including reltoastrelid;
2. pg_attribute and pg_attrdef;
3. pg_type, pg_range, and pg_enum;
4. pg_proc;
5. pg_constraint and pg_index;
6. pg_rewrite, pg_trigger, and pg_policy; and
7. ownership, ACL, comment, and dependency catalogs.

TOAST reloptions and tablespace state must initially become parent-table
blockers. Modeling and planning those options can be a later feature; silent
equality cannot.

### Evidence gate

- PostgreSQL 15–18 live catalog columns exactly match the ledger.
- Every modeled or blocked entry names an executable test.
- Mutation probes show a fingerprint change or unsupported selector for every
  schema-relevant attribute.
- Secret-bearing fields remain absent from diagnostics and snapshots.
- README language distinguishes inventoried families from completed
  attribute coverage until the ledger reaches complete status.

## Climb 3: make application contracts visible

### Developer failure

This additive migration is valid PostgreSQL but can break positional writers
or rigid row decoders:

~~~sql
ALTER TABLE app.accounts ADD COLUMN timezone text;

-- Existing application code may still do this:
INSERT INTO app.accounts VALUES (1, 'person@example.com');
~~~

### Change

Every ADD COLUMN against an existing table must carry an
application-contract hazard, including nullable columns without defaults.
Columns created as part of CREATE TABLE do not need it.

The rendered review guidance and README must state the assumption:

- writes list their target columns;
- readers tolerate the additional result shape or avoid positional SELECT *;
- generated, identity, defaulted, and required columns may add stronger
  rewrite, validation, or writer-compatibility hazards; and
- onwardpg cannot inspect application queries to prove those contracts.

Use one stable hazard vocabulary across JSON, rendered SQL, documentation, and
tests. Avoid calling an operation safe when the claim means only
catalog-additive.

### Evidence gate

- Every existing-table ADD COLUMN planner path has the hazard.
- CREATE TABLE fixtures do not receive a misleading compatibility warning.
- SQL rendering explains the developer action, not only the internal hazard
  token.
- A blind reviewer can find the assumption from the five-minute workflow,
  without discovering it only in the ownership table.

## Climb 4: make rename backfill strategy explicit

### Developer failure

The compatibility bridge is logically correct, but its generated UPDATE can
turn into a long transaction with heavy WAL, bloat, replica lag, and lock
pressure. Disposable history contains no production row-count evidence.

### Change

Keep automatic trigger installation and the final native rename. Replace the
implicit full-table backfill with a fingerprint-bound strategy decision:

- manual_sql is the default and creates an editable, verifiable backfill
  pocket with explicit batch boundaries;
- single_transaction retains the generated UPDATE only after an affirmative
  choice and carries unbounded-update, WAL, replica-lag, table-scan, and
  long-transaction hazards; or
- split_plan explains when the bridge and cleanup need separate operational
  work.

The final catch-up must use the same chosen strategy or a verified assertion;
it must not quietly reintroduce an unbounded UPDATE in contract.

The existing manual-work protocol remains the execution seam. Require at least
one boolean assertion proving old and new values agree before compatibility
objects are removed.

### Evidence gate

- Accepting rename identity alone never emits an unbounded UPDATE.
- The single-transaction strategy is explicit, receipted, and regenerated
  deterministically.
- Manual backfill edits survive regeneration and clone verification.
- Contract cannot drop the bridge until the equality assertion succeeds.
- Small-table and manual/batched paths converge on PostgreSQL 15–18.

## Climb 5: harden the scratch trust boundary

### Developer failure

Dropping a disposable database does not undo cluster-global SQL executed by a
powerful scratch login. Materialization can also fail mysteriously when
extensions or referenced roles do not exist on the scratch cluster.

### Immediate contract

Prominently state in the README and configuration guide:

- schema and migration SQL are trusted project code;
- the scratch URL must point to a dedicated local or CI PostgreSQL cluster,
  not merely a disposable database on a shared cluster;
- supported PostgreSQL major means bundles are bound to one major, not portable
  across all supported majors; and
- referenced extension packages and roles must already exist in the scratch
  environment.

### Authority separation

Create separate scratch administration and execution identities for every
materialization:

~~~text
scratch admin URL:  creates and force-drops the random database and login role
scratch DDL owner:  random one-hour login; owns only the random database; no
                    SUPERUSER, CREATEROLE, CREATEDB, REPLICATION, INHERIT, or
                    BYPASSRLS
~~~

The administrator creates both objects; there is no configured powerful
execution identity to reuse accidentally. DDL execution and catalog inspection
use only the generated credentials. Cleanup remains on the administrative
connection and propagates failures.

### Matrix finding and architectural revision

The first PostgreSQL 15–18 run proved that database-role restriction and full
privileged-DDL compatibility cannot be the same execution mode. A role that
cannot `SET ROLE` cannot materialize `ALTER ... OWNER TO external_role`, and a
non-superuser cannot install extensions whose control files require superuser.
Granting those powers back to arbitrary project SQL would recreate the original
cluster-global escape.

Keep the restricted database owner as the default and fail clearly for those
inputs. The safe way to close the remaining pgmig ownership and privileged
extension gap is a cluster-per-materialization provider: project SQL may be
privileged only inside an ephemeral PostgreSQL cluster whose destruction is the
security boundary. A future provider interface may launch that cluster locally
or in CI and return a one-use administrative URL plus a mandatory destroy
operation. A random database on a shared cluster is not an acceptable
substitute. Until that provider exists, documentation must describe typed
ownership as planner/live-graph support rather than normal declarative workflow
support.

### Evidence gate

- A fixture attempting CREATE ROLE or CREATE TABLESPACE fails without leaving
  cluster-global state.
- Ordinary schemas, trusted extensions, comments, policies, grants, routines,
  and migrations still materialize under the DDL owner; superuser extensions
  and external ownership transfers fail at this boundary.
- Failure messages distinguish missing extension packages, missing roles, and
  insufficient local privileges where PostgreSQL exposes that distinction.
- Disposable databases are force-dropped on every tested failure path.

## Blind-review climb log

The first fresh review was positive about the developer-preview boundary, but
found one high-severity ordering defect: an extension schema move was emitted
after work that already referenced extension members in the new schema. The
fix keeps the move in contract, emits it before same-phase work, and contracts
every transitive dependent create or modification behind it. A live `pg_trgm`
fixture moves the extension and rebuilds a GIN index with the newly qualified
operator class, then proves convergence.

That review also tightened three boundaries:

- extensions are atomic by package name, version, and schema; member contents
  and upgrade history are not independently compared;
- constraint-backed primary-key, unique, and exclusion indexes remain
  synchronous builds and now report blocking-build and access-exclusive-lock
  hazards even when ordinary concurrent indexes are enabled; and
- scratch databases clone pristine `template0`, while a dependency-edge-only
  graph difference now fails closed instead of producing an empty plan.

The completion gate still requires two consecutive fresh reviews after these
corrections. A finding closes through an executable fix or blocker plus a
regression fixture; documentation alone closes only a claim mismatch.

The second fresh review found another admitted-state gap: ordinary-view column
defaults were absent from both the graph and blockers, a manually created
sequence owned by an identity column was filtered with the internal identity
sequence, and references to extension-owned opclasses did not alias to the
atomic extension node. The next climb therefore:

- blocks view defaults and unmodeled domain-constraint, composite-attribute,
  and view-column comments;
- distinguishes PostgreSQL's internal identity sequence (`deptype = 'i'`)
  from a standalone sequence later attached with `OWNED BY`;
- maps every extension-member catalog address to its typed Extension ID so a
  newly created index using a moved opclass is contracted behind the move;
- blocks table grants issued onward by a non-owner grantor, which a random
  `NOINHERIT` scratch role cannot reproduce; and
- defers trigger creation, policy creation, and RLS activation on existing
  tables to contract, while preserving expand behavior for a newly created
  table.

The review also proposed unlogged materialized views as a missing state.
PostgreSQL 15–18 rejects `CREATE UNLOGGED MATERIALIZED VIEW`; `relpersistence`
is therefore recorded as a PostgreSQL-fixed invariant for that object kind,
not a new planner feature. Scratch locale, encoding, provider, and default
collation remain environmental and are now called out beside the scratch URL.

Because this second review found unmitigated highs, the consecutive-clean
counter restarts after these fixes.

The third fresh review found two more catalog-state distinctions and one
renderer defect. PostgreSQL 18 can mark one NOT NULL constraint both local and
inherited; that later-drop behavior is now a narrow blocker. An interrupted
`DETACH PARTITION ... CONCURRENTLY` leaves `pg_inherits.inhdetachpending`; the
table inspector now emits `partition_detach_pending` and the attribute ledger
classifies that field as blocked. The live fixture holds an old snapshot,
terminates the detaching backend after its first internal commit, and proves
the intermediate state cannot compare as an ordinary attached partition.

Invalid domain CHECK constraints exposed a duplicated `NOT VALID` clause
because PostgreSQL's deparser and the renderer both supplied it. Inspection now
canonicalizes the definition and retains validation as typed state; the
renderer remains defensive against pre-canonicalized typed input. The live
fixture creates, plans, applies, and converges an invalid domain constraint on
each supported major.

This review also suggested unlogged materialized views. Direct probes against
PostgreSQL 15, 16, 17, and 18 all reject that syntax, confirming the earlier
fixed-invariant classification. The clean-review counter restarts again after
the three real fixes.

The review's medium composite-type concern also becomes executable intent:
attribute removal or type change asks for a fingerprint-bound `CASCADE`
confirmation and reports data-loss, implicit-cast, table-rewrite, dependent-row,
and product-semantics hazards.

The fourth fresh review found two high-severity false-equality and ordering
holes. A serial column previously collapsed any dependent `nextval` expression
to the serial pseudo-type while excluding its backing sequence, so customized
sequence allocation, comments, or expressions could disappear. Inspection now
admits only the canonical implicit shape and emits `serial_sequence_state` for
the rest. Typed identity options remain supported, while customized backing
sequence names, comments, or persistence emit `identity_sequence_metadata`.
An ordinary serial fixture proves the blocker is narrow on PostgreSQL 15–18.

The same review observed that a relation's implicit row `pg_type` was absent
from the catalog-address index. Table, view, materialized-view, and row-type
array OIDs now alias to the owning logical relation; standalone composite
attribute addresses alias to their composite object. Live fixtures prove that
routines, domains, composites, and columns consuming a table row type create
after and drop before that table on every supported major. Because these were
unmitigated highs, the consecutive-clean counter restarts again.

The fifth fresh review found a compatibility-window defect rather than a final
catalog mismatch. Modifying an existing row-security node assigned `ENABLE`
and `FORCE ROW LEVEL SECURITY` to expand. When a policy changed in the same
plan, phase regrouping could therefore move RLS tightening ahead of the policy
edge it depends on. Existing-table RLS enable/force modifications now remain
in contract, carry an application-behavior hazard, and retain policy-before-RLS
ordering. A combined policy mutation and RLS-force fixture applies and
converges on PostgreSQL 15–18.

That review also exposed a medium environment ambiguity in `dev plan`: the
development catalog and desired scratch materialization could use two different
supported majors. The command now discovers both majors and rejects a mismatch
before comparison. The high-severity finding restarts the consecutive-clean
counter once more.

The sixth fresh review generalized the phase-order concern and found a more
serious stored-state case. A same-signature routine replacement could leave a
retained expression or partial index, stored generated column, or validated
constraint carrying values computed under the old body. Catalog replay could
still converge. These retained catalog-proven dependents now emit
`routine_semantic_change_stored_dependent`; newly created dependents are
promoted behind the contract routine replacement. Live PostgreSQL 15–18
fixtures prove both the refusal and the safe new-index order.

Phase assignment now has a transitive dependency closure: any create or modify
that depends on a contract-phase provider is promoted to contract before final
batch grouping. This covers routine changes, enum-label changes, extension
updates, and other typed edges instead of adding special cases per object.
Uncoordinated create/drop transitions in PostgreSQL's shared relation and type
namespaces also fail closed. Finally, semantic-default suppression can no
longer return an empty plan with unequal raw fingerprints; it reports
`default_equivalence_not_fingerprintable`. Because the review found a high,
the consecutive-clean counter restarts again.

The seventh fresh review found four high-severity cases where the general
architecture was sound but its safety closures were incomplete:

- phase promotion changed a dependent's phase without guaranteeing its rename
  provider appeared first inside that phase;
- an opaque extension upgrade could change member semantics while retained
  stored users remained catalog-equal;
- a routine rename combined with a body change escaped the Modify-based stored
  dependency guard; and
- an explicitly named multirange type was missing from PostgreSQL's shared type
  namespace collision check.

Rename rendering now separates providers with no prerequisites from table
renames that must follow mutations of the old physical table. New table
dependents and source-schema drops are held until the physical rename; index,
view, enum, and column rename providers are emitted before promoted dependents.
Fixtures cover replica identity after an index rename, an index after a column
bridge, and a cross-schema table rename before both a new view and old-schema
drop.

Extension version changes now fail closed when any retained catalog-proven
dependent exists. Stored-dependency detection includes semantic routine
rename-plus-replacement and materialized views. Range namespace keys include
both the range and explicit multirange names. The catalog ledger now describes
non-table owner and routine ACL attributes as blocked, connection-relative
state rather than modeled absolute equality.

Finally, scratch creation still uses pristine `template0`, but explicitly
copies encoding, locale provider, locale, collation, and ctype from the
connected control database and rejects a collation-version mismatch. These
changes restart the consecutive-clean counter.

Two subsequent context-blind PostgreSQL reviews inspected the frozen README,
typed graph, implementation, tests, and catalog ledger without this plan or
change history. Both concluded `SATISFIED/EXCITED` with no unmitigated
high-severity correctness or completeness issue. The second review therefore
satisfies the consecutive-clean completion gate.

The reviews left three non-high follow-ups for a future climb rather than
reasons to reopen this one:

- sequence parameter reconciliation intentionally does not reposition live
  `last_value`/`is_called` state; make that operator decision more explicit;
- a column-rename bridge combined with a standalone sequence `OWNED BY` change
  can produce a plan that disposable verification rejects; teach rename
  dependency consumption about that sequence edge or fail it closed earlier;
- development/scratch locale parity is not compared independently, although
  each disposable database now exactly inherits its own control database's
  environment.

These are the first candidates for the next hill climb. They do not weaken the
completed gate: one is an explicitly disclaimed runtime-data decision, one is
caught by mandatory disposable execution before acceptance, and one concerns
development reconciliation rather than a verified production bundle.

## Claims pass

After the five climbs, align README, architecture, safety, security, feature,
and installation documents around the same precise claims:

- PostgreSQL 15–18 are supported targets, with each bundle bound to one major.
- Inventoried unsupported state fails closed; completed attribute coverage is
  claimed only when the live ledger proves it.
- Catalog convergence does not prove application, data-volume, lock, traffic,
  or replica safety.
- Opaque procedural dependencies remain an explicit PostgreSQL boundary.
- Scratch execution is trusted code inside a dedicated cluster boundary.

Documentation changes are part of each vertical slice, not a final cleanup
that can drift from behavior.

## Blind-review protocol

After every climb, use a fresh context-blind PostgreSQL specialist. Give it:

- README.md;
- pgschema and all internal source packages;
- source, planner, verification, and live PostgreSQL tests; and
- the checked-in catalog ledgers.

Do not give it this plan, prior reviews, PR descriptions, competitor notes, or
the intended answer.

Ask:

1. What schema states can compare equal despite a meaningful difference?
2. What supported-object dependencies can be ordered incorrectly?
3. Which generated plans are logically convergent but operationally unsafe?
4. What PostgreSQL-major, privilege, extension, role, or scratch assumptions
   are hidden?
5. What is the highest-severity reason not to trust this as a schema review
   aid?

Classify every finding as:

- correctness: can emit invalid SQL, wrong ordering, or false convergence;
- completeness: schema-relevant state is absent or silently equal;
- operational: locks, data volume, WAL, replication, or rollout assumptions;
- environment: version, role, extension, or cluster prerequisites; or
- claim: documentation is stronger than implementation.

The next hill-climb item is always the highest-severity reproducible finding.
Fix it, block it explicitly, or narrow the claim. Do not answer a reviewer only
with prose when an executable blocker or test can encode the boundary.

## Completion gate

This plan is complete when:

- the dependency projector has no unclassified relevant modeled-object edges
  across the PostgreSQL 15–18 fixture corpus;
- the live attribute ledger is complete for all supported majors;
- TOAST and auxiliary-relation state cannot compare silently;
- existing-table ADD COLUMN operations expose application assumptions;
- column rename performs no unbounded backfill without explicit consent;
- scratch DDL lacks cluster-global authority by default;
- all repository, race, vet, static, differential, and PostgreSQL 15–18
  convergence suites pass; and
- two consecutive fresh blind reviews find no unmitigated high-severity
  correctness or completeness issue.

Operational and environment limitations may remain, but they must be visible,
actionable, and stated at the point where a developer encounters them.
