# pgmig scenario-parity plan

This plan tracks behavioral parity with pgmig's executable PostgreSQL scenario
corpus. It complements the high-level
[`parity/pgmig-roadmap.json`](../parity/pgmig-roadmap.json) ledger; the roadmap
is useful for orientation, but it is not granular enough to prove that onwardpg
handles every upstream case.

## Baseline and objective

- Upstream: [`Apakottur/pgmig`](https://github.com/Apakottur/pgmig)
- Pinned commit: `d2cccb6886bfb0b6ad0649bbe1430a9ab57ae983`
- Reviewed: 2026-07-18
- Executable upstream API scenarios: 454 tests across 9 areas
- Existing onwardpg roadmap baseline: `4685adaf1636aca2e1303effc1d5f130795708e1`
- Upstream movement since that baseline: 90 commits

The immediate objective is scenario parity on the PostgreSQL majors shared by
both projects: PostgreSQL 15, 16, 17, and 18. PostgreSQL 14 remains outside
onwardpg's deliberate server policy. Adding it would be a separate support
decision, not a hidden side effect of this compatibility work.

"Parity" means matching the observable schema outcome, not copying pgmig's SQL
or weakening onwardpg's expand/contract safety model:

1. For every pgmig scenario that successfully converges, onwardpg must produce
   `planned`, `needs_input`, or `needs_sql_edits` work that can be completed and
   verified to the same desired catalog state. A terminal `unsupported` result
   does not count as parity for a pgmig success case.
2. For every pgmig scenario that deliberately rejects a shape, onwardpg must
   either safely support it or return an explicit blocking result. Empty plans,
   silently ignored catalog state, and apply-time surprises do not count.
3. Successful cases must apply on a disposable real-PostgreSQL clone and replan
   to an empty structural diff. Planner-unit coverage alone is insufficient.
4. Version-specific syntax must be proved on every applicable major in the
   15–18 CI matrix.
5. Destructive pgmig strategies may become confirmed operations, editable SQL
   handoffs, or safer online strategies in onwardpg. The final catalog and the
   refusal boundary are the compatibility contract.

## What changed upstream today

Eight feature changes landed on 2026-07-18. They form the first catch-up slice:

| Upstream change | Current onwardpg position | Required work |
| --- | --- | --- |
| Trigger enable/disable, replica, and always states | Verified for create, recreate, rename, and unchanged-state behavior | Keep the pinned scenario evidence current across the version matrix |
| Constraint triggers | Create/drop/recreate/rename/deferrability are verified | Keep the pinned scenario evidence current across the version matrix |
| `INSTEAD OF` triggers on views | Lifecycle, comments, and identity-preserving view replacement are verified | Keep view-trigger state restrictions explicit |
| Enum label rename | Pure positional single/multi-label renames are verified | Use the confirmed rewrite for mixed rename+insert shapes |
| Enum comments and empty enums | Create/add/change/remove/unchanged comments and empty-enum round-tripping are verified | Keep the typed catalog evidence current across the version matrix |
| Extension comments | Package defaults are normalized and custom comment changes are verified | Version ignores and schema moves are now verified separately |
| Sequence persistence | Unlogged create, logged/unlogged flips, and unchanged state are verified | Keep temporary sequences explicitly blocked |
| Constraint rename | Confirmed ordinary check/unique/exclusion/foreign-key renames, backing indexes, and comments are verified | Keep partition-propagated and ambiguous shapes outside the proof |
| Replica identity | DEFAULT/FULL/NOTHING/USING INDEX create and transition cases are verified | Preserve the explicit identity-to-index graph edge |
| Domains | Full lifecycle, attributes, named checks, comments, dependencies, and base-type rejection are verified | Keep the typed dependency evidence current across the version matrix |
| Composite types | Empty/populated lifecycle, attributes, comments, CASCADE changes, nested/array ordering, and reorder rejection are verified | Keep retained-attribute reorder as an explicit refusal boundary |
| Range and multirange types | Lifecycle, options, comments, typed subtype/column dependencies, and property recreation are verified | Keep dependent rewrites and custom canonical-function choreography explicit boundaries |
| Enum value removal/reorder rewrite | Confirmed recreation, scalar/array column casts, defaults, comments, and retained data are verified | Keep generated/index/constraint/domain/view dependents as explicit refusals |
| Routine return-type replacement with dependents | Return identity, defaults, checks, expression indexes, comments, and catalog-proven routine chains are verified | Keep opaque procedural bodies and unsafe changed/dropped/circular dependents as explicit refusals |

## Corpus inventory

The pinned pgmig API suite is the source of scenario names and fixtures. The
checked-in [`scenario inventory`](../references/pgmig-d2cccb6.json) records one
row per upstream API test and is regenerated by `scripts/pgmiginventory` from
the exact pinned checkout.

| Area | Upstream API scenarios | Initial assessment |
| --- | ---: | --- |
| Constraints and indexes | 92 | Full lifecycle, ordering, validation, comments, concurrent indexes, PG15 null semantics, and PG18 temporal/enforcement cases are directly verified |
| General safety and catalog guards | 15 | Every case is linked to direct support, filtering, or an explicit typed rejection |
| Materialized views | 32 | Lifecycle, indexes, dependency chains, rebuild closure, external views, and scoped handoffs are directly verified |
| Routines and triggers | 58 | Trigger and routine-dependency gaps are verified; opaque procedural-body references remain an intentional catalog boundary |
| Schemas and extensions | 18 | Full schema/extension lifecycle, comments, dependencies, version ignores, and schema moves are directly verified |
| Sequences | 28 | Parameters, ownership, persistence, comments, identity/serial filtering, and lifecycle are directly verified |
| Tables and columns | 124 | Full direct lifecycle is verified; type changes and partition reconfiguration use scoped, clone-verified handoffs where application/data policy is required |
| Types | 66 | All 59 successful scenarios are verified; seven destructive/unsafe shapes are verified intentional rejections |
| Views | 21 | Ordinary lifecycle, options, dependencies, comments, triggers, and scoped type-change handoffs are directly verified |
| **Total** | **454** | Every row has an explicit onwardpg outcome and resolvable evidence link |

Current classified progress at the pinned commit:

- 405 `verified` scenarios;
- 0 `implemented_unverified` scenarios;
- 25 `needs_handoff` scenarios;
- 24 `intentional_rejection` scenarios;
- 0 remaining `unsupported_gap` scenarios.

The trigger workstream is complete for this pin: ordinary enable-state
transitions, constraint triggers, view triggers, comments, rename behavior, and
recreate-state restoration are verified on PostgreSQL 15–18.
Enum comments and empty enum creation are also typed and verified across the
same PostgreSQL matrix.
Custom extension comments are typed and verified across the matrix while
control-file defaults are normalized away.
Standalone sequence parameters, comments, persistence, and manually owned
sequence lifecycle/cascade behavior are verified on real PostgreSQL. Identity
residue, serial backing sequences, extension members, and partition-trigger
clones are filtered at their catalog boundaries; temporary sequences remain
outside that support boundary.
Fingerprint-confirmed ordinary constraint renames are verified across the
matrix, including backing-index and comment convergence.
Replica identity is typed as an index-order-aware table child and all nine
pinned scenarios are verified across the matrix.
All pinned domain success cases and the base-type-change rejection are verified
across the matrix with dependency-safe enum/domain/table ordering.
All pinned composite success cases and attribute-reorder rejection are verified
across the matrix, including nested, array, table-use, and reverse-drop order.
All 12 pinned range success cases are verified across the matrix, including
options, comments, subtype/table dependencies, recreation, and approved drop.
All eight pinned successful enum rewrites and all five unsafe-dependent
rejections are verified across the matrix, including retained scalar/array
data, defaults, and comments.
All pinned routine-dependency cases are verified across the matrix: catalog
edges order default/check/index and chain/diamond drops, bounded return-type
changes recreate unchanged dependents and comments, and unsafe shapes refuse.
Stored generated-expression changes converge across PostgreSQL 15–18, while
PostgreSQL 18 virtual columns support create/add/drop and in-place expression
changes. Same-type column collation changes and reset-to-default transitions
also converge across the matrix.
Ordinary and partitioned table ownership is typed in live graphs, requires a
fingerprint-bound authorization decision, and can be narrowly ignored without
weakening the non-table ownership blockers. Default restricted declarative
materialization cannot assume membership in an external owner role; closing
that pgmig workflow gap requires an isolated privileged-cluster path.
Exact-name extension version ignores are repeatable and harmless when a name
does not match; identity-preserving extension schema moves and both ignore
branches converge across PostgreSQL 15–18.
Schema create/drop/comments, empty-search-path introspection, base table
lifecycle/persistence, and table/column comment state are directly verified.
Ordinary views now have exact create/drop/replace/comment, reloption, and
cross-schema/transitive dependency evidence. Column type changes remain an
editable rolling-deployment handoff, now with the precise affected view closure
listed and unaffected views excluded.
Materialized views now have exact lifecycle, index, dependency-chain, external
view, and cascading rebuild evidence. Type-change handoffs list the complete
affected ordinary/materialized view and index closure.
Identity and serial cases now cover creation, option changes, generation flips,
add/remove ordering, and primary-key composition. Base-column lifecycle cases
cover defaults, nullability, physical-order normalization, and primary-key-aware
contraction ordering.
Partitioned tables now cover range/list/default/hash creation, nested and
cross-schema hierarchies, parent indexes and primary keys, child add/remove and
attach/detach, and parent-only subtree drops. Parent/bound/key/strategy changes
use a fingerprint-bound manual contract, including primary-key preservation.
Constraint and index cases now cover validation in both directions, pure
deferrability changes, PostgreSQL 15 `NULLS NOT DISTINCT`, concurrent paths,
comment-preserving name reuse, and foreign-key drop ordering when one or both
related tables are removed.

The generated inventory should record at least:

- stable scenario ID derived from upstream path and test name;
- pinned pgmig commit and applicable PostgreSQL majors;
- expected upstream outcome: `converges` or `rejects`;
- catalog objects and transition tags exercised by the fixture;
- onwardpg classification: `verified`, `implemented_unverified`,
  `needs_handoff`, `unsupported_gap`, or `intentional_rejection`;
- exact onwardpg unit, integration, and differential test references;
- a short note when onwardpg intentionally chooses a different migration
  strategy.

CI should reject an upstream pin change that introduces, removes, or renames a
scenario without updating the inventory. This makes future bursts of upstream
activity visible immediately.

## Delivery sequence

### 0. Pin and classify the executable corpus

Build a small importer that reads the pinned pgmig test tree and emits a
machine-readable scenario inventory under `references/`, following the Stripe
reference-corpus precedent. Seed all 454 API scenarios and add a parity test
that validates unique IDs, the pinned commit, allowed classifications, and
resolvable onwardpg evidence.

Do not mark a scenario verified merely because a broad feature appears in
`docs/supported-features.md`. Verification requires a scenario-specific real
PostgreSQL convergence or rejection test.

Exit criteria:

- the inventory contains exactly the 454 pinned API scenarios;
- every scenario has an owner workstream and current classification;
- CI detects corpus drift;
- the high-level roadmap ledger points to this inventory for proof.

### 1. Close the 2026-07-18 catch-up slice

Implement the eight changes listed above in vertical slices. Prefer one object
family per change so catalog modeling, graph edges, rendering, safety choices,
and convergence tests land together.

Recommended order:

1. Prove ordinary trigger enable-state behavior already present.
2. Generalize constraint and view triggers.
3. Add enum label rename, then the confirmed dependent-column rewrite.
4. Add composite types, then ranges; reuse a shared type-dependency model rather
   than another phase-specific ordering list.
5. **Completed:** add routine return-type identity and typed dependent recreation.

Exit criteria:

- every successful scenario introduced or materially changed by today's eight
  commits is verified on its applicable PostgreSQL versions;
- upstream rejection cases have explicit onwardpg refusal evidence;
- no view, trigger, type, or routine catalog state is silently discarded.

### 2. Finish small typed-attribute gaps from the intervening 90 commits

These are narrower than the free-standing type work and should not wait for a
large planner redesign:

- **Completed:** table replica identity: `DEFAULT`, `FULL`, `NOTHING`, and
  `USING INDEX`, with a typed index dependency and correct
  create/drop/replacement order;
- **Completed:** unlogged sequences and logged/unlogged transitions;
- **Completed:** PostgreSQL 18 virtual generated columns and stored/virtual
  expression changes;
- **Completed:** same-type column collation changes and reset-to-default
  transitions, rendered as reviewed type operations;
- **Completed:** PG18 temporal primary/unique/foreign-key definitions and
  `NOT ENFORCED` transitions, accepting rebuilds where PostgreSQL lacks an
  in-place form;
- **Completed:** extension schema moves plus repeatable exact-name extension
  version ignores;
- **Completed:** table ownership with an explicit authorization decision and
  stable role quoting. The broader non-table ownership blocker remains intact.

Exit criteria: every corresponding pgmig success case is verified, and the
catalog inventory no longer reports these modeled states as generic blockers.

### 3. Complete free-standing types and type dependency ordering

Domain, composite, and range lifecycle and dependency ordering are complete for
the pinned corpus.
The shared model now carries free-standing type dependencies onto consuming
table statements and views.

The completed verticals cover domain attributes and named constraints,
composite attributes and nested/array dependencies, and range subtype options,
multirange identity, comments, and consuming table columns. Creation is
dependency-first and destructive removal is dependent-first in reverse.
Base-type domain replacement, retained composite-attribute reorder, range
rewrites with retained dependents, and custom range canonical-function
choreography remain explicit refusal boundaries.

The complete 66-scenario type corpus is now accounted for: 59 successful cases
are verified and seven unsafe shapes are verified intentional rejections. No
pinned type scenario remains `implemented_unverified` or `unsupported_gap`.

Exit criteria: all 66 pinned type scenarios are either verified successes or
verified intentional rejections.

### 4. Automate currently editable/destructive scenarios

Onwardpg can already hand some of these transitions to reviewed SQL. Promote
the bounded, mechanically provable shapes to typed plans while keeping manual
fallbacks for application-specific cases:

- partition reparenting and bound changes via detach/attach, including default
  partitions;
- partition key/strategy changes through a confirmed subtree rebuild that
  preserves modeled indexes and constraints;
- implicit and explicit column casts, using direct `ALTER TYPE` only when the
  one-deployment application contract is safe;
- generated-expression replacement where PostgreSQL requires drop/add;
- enum rewrites with data-dependent assertions before old type removal.

Exit criteria: pgmig's successful destructive fixtures no longer end in
`unsupported`; each produces either a verified typed plan or a complete,
clone-verified editable handoff.

### 5. Certify and keep pace

Run the full classified corpus on PostgreSQL 15–18. Publish counts by
`verified`, `needs_handoff`, `intentional_rejection`, and remaining gap. Update
the pin deliberately after reviewing upstream changes, with a generated diff
of added/removed scenarios.

Parity is complete only when:

- every pinned pgmig success scenario is verified as convergent in onwardpg;
- every pinned pgmig rejection scenario is verified as supported or explicitly
  rejected;
- no scenario remains `implemented_unverified` or `unsupported_gap`;
- the high-level roadmap ledger and ecosystem comparison cite the same pin;
- docs describe deliberate differences such as safety decisions, online
  strategies, and PostgreSQL 14 scope.

## Architecture implications

Several gaps share causes and should be solved once:

- **Relation-bound objects:** triggers must target an ordinary table,
  partitioned table, or ordinary view without losing the owning relation kind.
- **Free-standing types:** domain, composite, range, and enum dependency edges
  need one graph vocabulary and ordering path.
- **Dependent definitions:** routine return changes and enum rewrites require
  typed edges from defaults, constraints, indexes, columns, and views. Parsing
  arbitrary procedural text remains out of scope.
- **Authorization:** ownership is not a comment-like attribute. It needs an
  explicit authorization decision, role identity, hazards, and clone setup.
- **Version gates:** PG18 virtual columns, temporal constraints, and enforced
  state remain version-scoped catalog semantics with explicit PostgreSQL 18
  convergence evidence.

## Work that does not block scenario parity

Pgmig currently lists roles, grants, default privileges, non-table ownership,
RLS, and many advanced PostgreSQL catalogs as planned or non-goals. Onwardpg
already exceeds pgmig in some of these areas, notably typed table grants and
RLS policies. Keep those safety blockers and existing features healthy, but do
not put unrelated catalog expansion on the critical path for this pinned
scenario-parity milestone.
