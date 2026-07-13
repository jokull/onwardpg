# Compatibility guide

This table is a practical planning guide, not a compatibility certification.
It compares onwardpg's developer preview with the final public
[Migra](https://github.com/djrobstep/migra) source release (v3, 2022). Migra is
deprecated and delegates much of its object inspection to `schemainspect`; its
column therefore describes its intended diff surface, not a current support
commitment from that project.

## Reading the onwardpg column

| Marker | Meaning |
| --- | --- |
| **Planned** | onwardpg can inspect the state and emit the forward DDL. Review is still required. |
| **Answer required** | The transition is plausible but ambiguous or destructive; a fingerprint-bound answer is required. |
| **Manual contract** | onwardpg orders and records the work, but the operator supplies reviewed SQL and verification. |
| **Blocked** | onwardpg returns `unsupported` unless the object is explicitly ignored. It does not silently omit it. |

“Migra: yes” means its source has a corresponding change family, not that every
PostgreSQL variation is equivalent to onwardpg or safe for unattended use.

## Inputs, output, and workflow

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Compare two live PostgreSQL schemas | Yes | **Planned** | Both are PostgreSQL-only schema diff tools. |
| Declarative SQL-file desired input | Indirect / external setup | **Planned** | onwardpg applies `CREATE` DDL to a disposable PostgreSQL database, then catalog-inspects it. |
| Typed schema adapter | Inspector objects | **Planned** | Adapters can supply declarative DDL or complete typed snapshots. |
| SQL output | Ordered SQL | **Planned** | onwardpg also returns JSON plan data. |
| Automatic application | `Migration.apply()` exists | Never | onwardpg only emits reviewable work. |
| Down migrations | No separate planner model | Never | Recovery is a new reviewed forward migration. |
| Expand / migrate / contract sections | No | **Planned** | SQL is annotated for application rollout sequencing. |
| Typed ambiguity answers | No | **Answer required** | Rename, drop, conversion, and manual-work answers bind to both schema fingerprints. |
| Unknown catalog object | Inspector dependent | **Blocked** | onwardpg rejects unmodeled state unless explicitly ignored. |
| Server policy | Historical project support | PG 14–18 | Support is a policy boundary, not an assertion about every object variation. |

## Schemas, extensions, tables, and columns

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Schema create/drop | Yes | **Planned** | Drops are destructive and require approval. |
| Schema comments | Yes | **Planned** | |
| Extensions create/drop | Yes | **Planned** | Drop is destructive and approved. |
| Extension version / schema changes | Yes | **Planned** | Extension-owned objects remain a boundary risk. |
| Tables create/drop/comments | Yes | **Planned** | Drops require approval. |
| Table rename | Drop/create-oriented diff | **Answer required** | Preserves proven retained FKs, triggers, and direct view dependencies. |
| Table persistence | Partial | **Planned** | Includes logged/unlogged transitions; ownership is separate. |
| Columns add/drop/null/default/comment | Yes | **Planned** | Drops require approval. |
| Column rename | Drop/create-oriented diff | **Answer required** | Proven PostgreSQL catalog rewrites are retained; broader dependent rewrites are blocked. |
| Column type change | Yes | **Answer required** when not directly safe | onwardpg never invents a `USING` expression or data conversion. |
| Identity / generated / serial / collation | Yes | **Planned** for supported forms | Some add/change combinations remain explicit unsupported work. |

## Constraints, indexes, and sequences

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Primary / unique / check constraints | Yes | **Planned** | Create, drop, and structural rebuilds; destructive changes require approval. |
| Foreign keys and dependency cycles | Yes | **Planned** | Dependency-aware ordering; `NOT VALID` / validation behavior is modeled. |
| Exclusion constraints | Yes | **Planned** for supported forms | PostgreSQL and partition-version limits still apply. |
| Constraint rename | Yes | **Answer required** | No rename guessing. |
| Ordinary indexes | Yes | **Planned** | Create, drop, rename, expression/opclass/include/predicate/method/options. |
| Concurrent index operation | No phase model | **Planned** | Emitted in a valid non-transactional batch when requested. |
| Index attached to a constraint | Yes | **Planned** | Attachment requires structural proof. |
| Standalone sequences | Yes | **Planned** | Create/drop and parameter updates. |
| Sequence `OWNED BY` transitions | Yes | **Blocked** | Not yet modeled as a lifecycle transition. |

## Types, views, routines, and triggers

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Enum create/drop/add labels | Yes | **Planned** | Drops require approval. |
| Enum rename | Drop/create-oriented diff | **Answer required** | |
| Enum label reorder/removal | Limited / unsafe | **Blocked** or explicit rejection | Never silently treated as safe. |
| Domain, composite, range types | Inspector support varies | **Blocked** | Explicitly outside the preview boundary. |
| Ordinary view create/replace/drop | Yes | **Planned** | Typed dependencies on relations, columns, enums, views, and modeled routines. |
| Ordinary view rename | Drop/create-oriented diff | **Answer required** | |
| Materialized view create/drop | Yes | **Planned** | Drop is destructive. |
| Materialized view rename | Drop/create-oriented diff | **Answer required** | Preserves exact unchanged indexes through PostgreSQL's native rewrite. |
| Materialized view definition rebuild | Yes, via rebuild-style SQL | **Answer required** | Reviewed destructive rebuild; dependent data freshness can require a manual contract. |
| Refresh materialized view after semantic dependency change | No rollout model | **Manual contract** | Operator supplies normal/concurrent refresh and optional verification. |
| Functions / procedures create, replace, drop | Yes | **Planned** | Typed dependencies are limited to catalog-visible references. |
| Routine rename | Drop/create-oriented diff | **Answer required** | Same-signature, fingerprint-bound transition. |
| Procedural-body dependency rewrite | No safe general guarantee | **Blocked** | PostgreSQL does not catalog arbitrary body references. |
| Triggers create/drop/recreate | Yes | **Planned** | Includes typed table/routine dependency ordering. |
| Trigger rename | Drop/create-oriented diff | **Answer required** | |

## Partitioning, security, and other PostgreSQL objects

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Partitioned-table creation | Limited by final release era | **Planned** | Range/list/hash keys and typed hierarchy. |
| Partition child creation | Limited | **Planned** | Parent/bound child state is catalog-modeled. |
| Attach / detach range/list/hash/default child | Limited | **Planned** | Hazards record locks and possible validation scans. |
| Parent/bound/default-partition reconfiguration | Limited | **Manual contract** | onwardpg will not infer detach/attach order, casts, or data movement. |
| Propagated partition indexes and constraints | Limited | **Planned** for modeled parent operations | Child resources are handled through their parent; ambiguous independent child mutation is blocked. |
| Roles, grants, default privileges, ownership | Privileges optional | **Blocked** | Security policy must be modeled before it can be safely planned. |
| RLS and policies | Yes | **Blocked** | Explicit unsupported result. |
| Rules, text-search objects, foreign tables | Inspector support varies | **Blocked** | Explicit unsupported result. |

## What this means in practice

For conventional application schemas—tables, columns, constraints, indexes,
enums, sequences, standard views, and common routines/triggers—onwardpg can
already be more operationally useful than a plain SQL diff because it produces
a staged plan and refuses to invent intent. For advanced PostgreSQL object
families and data-dependent transitions, the correct preview behavior is to
stop with a precise question, manual-work contract, or unsupported diagnostic.

The machine-readable [feature map](../parity/pgmig-roadmap.json) is the source
for ongoing implementation status; [supported features](supported-features.md)
records the detailed dependency and safety semantics.
