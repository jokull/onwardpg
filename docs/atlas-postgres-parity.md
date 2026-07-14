# PostgreSQL reference behavior study

This repository studies the PostgreSQL planner in Atlas commit
[`a5e0aecc2bb64143bf522734f8ad88e04885fca6`](https://github.com/ariga/atlas/tree/a5e0aecc2bb64143bf522734f8ad88e04885fca6), not against the broad Atlas product surface or a generic SQL feature list.

The reference is useful for discovering PostgreSQL cases and testing our
planner outcomes. It is not onwardpg's release definition and does not impose
one-for-one SQL or feature compatibility. The reference planner covers
PostgreSQL schemas; tables, columns and comments;
primary/unique/check/exclusion/foreign-key constraints; indexes; enum types;
and the ordering needed to apply those changes. It also has PostgreSQL-specific
behaviour around identity, collations, generated columns, partition keys,
foreign-key cycles, index concurrency and other options.

The Atlas reference supports older PostgreSQL majors, but onwardpg deliberately
supports PostgreSQL 15 through 18 only. This is a new library: its supported
server policy is independent of Atlas's historical compatibility range.

Atlas source-level renderer support and CLI schema-diff behavior are not always
the same contract. The matrix records observed pinned-CLI behavior: for
example, it creates partition-key expressions but omits a partition-key
`COLLATE` clause, silently ignores enum-label removal, and rejects enum-label
reordering. onwardpg does not treat an empty Atlas CLI diff as evidence that a
destructive or unmodeled mutation is safe.

## Status of the study

The current Go implementation is graph-native but still an evolving developer
preview. It snapshots its supported objects directly from a repeatable-read
PostgreSQL catalog transaction and has initial pinned-Atlas differential
convergence tests. The matrix remains incomplete; it is not a coverage claim.
Explicitly inventoried outside-core families are blockers, but the
developer-preview catalog inventory is not yet exhaustive.

| Capability | Reference-study status | Current onwardpg status |
| --- | --- | --- |
| Schema/table/column semantic snapshot | Atlas core | Partial, typed graph |
| Comments, identity options, collation | Atlas core | Partial |
| Constraints including exclusion/NOT VALID | Atlas core | Partial |
| Semantic indexes and concurrent execution | Atlas core | Partial |
| Enum rename/add values | Atlas core | Partial |
| Partitioning and FK cycles | Atlas core | Partial |
| Dependency-aware ordering | Atlas core | Implemented foundation; coverage incomplete |
| Table persistence | onwardpg extension scope | Typed `UNLOGGED` create and logged/unlogged transitions |
| Standalone sequences | extension scope | Typed snapshot, create/drop, and parameter updates |
| Extensions | onwardpg extension scope | Typed create/drop/version update; schema move recognized but blocked pending a compatibility path |
| Views, routines, domains, composites | onwardpg extension scope | Views and basic routines are graph-modeled; domains/composites blocked |
| Policies, triggers, rules, grants, text-search, foreign tables | Mixed | Triggers, RLS/policies, and ordinary/partitioned-table grants are typed; foreign tables, rules, default/column/non-table ACLs, and text-search objects explicitly block until their verticals exist |

## Safety gate

Before snapshotting modeled objects, catalog inspection detects several known
families that the planner cannot faithfully preserve. A detected object makes
the plan `unsupported`; `--ignore kind:name` is an explicit user acceptance of
that blind spot. The gate intentionally blocks an unchanged detected object
too: a name-only comparison cannot establish that its definition is
unchanged. This gate is not yet a complete PostgreSQL catalog inventory.

## How this study is used

1. Typed PostgreSQL graph snapshot with dependency edges and capabilities.
   **In progress:** schemas, enums, tables, columns, constraints and structured
   indexes, plus standalone sequences, load directly from a consistent catalog
   snapshot; known inventoried unmodeled objects are blockers. Remaining work
   includes catalog-inventory completeness, dependency coverage, and
   differential verification.
2. Semantic change model, validated answer fingerprints, and a dependency DAG.
3. Forward-only expand/contract hazard policy and transactional batches.
4. Differential convergence tests against the pinned Atlas PostgreSQL planner
   where it helps establish expected PostgreSQL behavior.
5. Decide support from onwardpg's safety model and real PostgreSQL evidence,
   not from Atlas coverage alone.
