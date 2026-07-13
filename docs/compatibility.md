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
| **Plannable** | onwardpg can inspect the state and emit the forward DDL. Review is still required. |
| **Answer required** | The transition is plausible but ambiguous or destructive; a fingerprint-bound answer is required. |
| **Manual contract** | onwardpg orders and records the work, but the operator supplies reviewed SQL and verification. |
| **Blocked** | The listed family is in the current blocker inventory and returns `unsupported` unless explicitly ignored. The preview inventory is not exhaustive. |
| **Not inventoried** | The family is neither modeled nor yet reliably detected as a blocker; this is a known preview safety gap. |

“Migra: yes” means its source has a corresponding change family, not that every
PostgreSQL variation is equivalent to onwardpg or safe for unattended use.

## Inputs, output, and workflow

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Compare two live PostgreSQL schemas | Yes | **Plannable** | Both are PostgreSQL-only schema diff tools. |
| Declarative SQL-file desired input | Indirect / external setup | **Plannable** | onwardpg applies `CREATE` DDL to a disposable PostgreSQL database, then catalog-inspects it. |
| Project DDL export command | External setup | **Plannable** | `schema_command` is executed twice and must emit deterministic PostgreSQL DDL; no framework adapter is required or planned. |
| SQL output | Ordered SQL | **Plannable** | onwardpg also returns JSON plan data. |
| Automatic application | `Migration.apply()` exists | Never to an existing target | onwardpg executes only for self-created disposable clone verification. |
| Down migrations | No separate planner model | Never | Recovery is a new reviewed forward migration. |
| Expand / migrate / contract sections | No | **Plannable** | SQL is annotated for application rollout sequencing. |
| Typed ambiguity answers | No | **Answer required** | Rename, drop, conversion, and manual-work answers bind to both schema fingerprints. |
| Unknown catalog object | Inspector dependent | Catalog tables classified; attribute audit incomplete | Every PG14–18 catalog table has a machine-readable modeled/blocked/atomic/out-of-scope classification. Less-common attributes inside modeled families remain preview audit work. |
| Server policy | Historical project support | PG 14–18 | Support is a policy boundary, not an assertion about every object variation. |

## Schemas, extensions, tables, and columns

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Schema create/drop | Yes | **Plannable** | Drops are destructive and require approval. |
| Schema comments | No verified diff family | **Plannable** | Migra's final fixtures do not establish general object-comment diffing. |
| Extensions create/drop | Yes | **Plannable** | Drop is destructive and approved. |
| Extension version / schema changes | Version changes; no verified schema-move family | **Plannable** | Extension-owned objects remain a boundary risk. |
| Tables create/drop | Yes | **Plannable** | Drops require approval. |
| Table comments | No verified diff family | **Plannable** | |
| Table rename | Drop/create-oriented diff | **Answer required** | Preserves proven retained FKs, triggers, and direct view dependencies. |
| Table persistence | Partial | **Plannable** | Includes logged/unlogged transitions; ownership is separate. |
| Columns add/drop/null/default | Yes | **Plannable** | Drops require approval. |
| Column comments | No verified diff family | **Plannable** | |
| Column rename | Drop/create-oriented diff | **Answer required** | Proven PostgreSQL catalog rewrites are retained; broader dependent rewrites are blocked. |
| Column type change | Yes | **Answer required** when not directly safe | onwardpg never invents a `USING` expression or data conversion. |
| Identity / generated / serial / collation | Yes | **Plannable** for supported forms | Some add/change combinations remain explicit unsupported work. |

## Constraints, indexes, and sequences

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Primary / unique / check constraints | Yes | **Plannable** | Create, drop, and structural rebuilds; destructive changes require approval. |
| Foreign keys and dependency cycles | Yes | **Plannable** | Dependency-aware ordering; `NOT VALID` / validation behavior is modeled. |
| Exclusion constraints | Yes | **Plannable** for supported forms | PostgreSQL and partition-version limits still apply. |
| Constraint rename | No general rename detection | **Answer required** | Migra's name-keyed comparison generally renders remove/add; onwardpg does not guess. |
| Ordinary indexes | Yes | **Plannable** | Create, drop, rename, expression/opclass/include/predicate/method/options. |
| Concurrent index operation | No phase model | **Plannable** | Emitted in a valid non-transactional batch when requested. |
| Index attached to a constraint | Yes | **Plannable** | Attachment requires structural proof. |
| Standalone sequences | Yes | **Plannable** | Create/drop and parameter updates. |
| Sequence `OWNED BY` transitions | Yes | **Not inventoried** | Not yet modeled or reliably detected as a lifecycle transition. |

## Types, views, routines, and triggers

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Enum create/drop/add labels | Yes | **Plannable** | Drops require approval. |
| Enum rename | Drop/create-oriented diff | **Answer required** | |
| Enum label reorder/removal | Limited / unsafe | **Blocked** or explicit rejection | Never silently treated as safe. |
| Domain, composite, range types | No verified Migra diff family | **Blocked** | Explicitly outside the onwardpg preview boundary. |
| Ordinary view create/replace/drop | Yes | **Plannable** | Typed dependencies on relations, columns, enums, views, and modeled routines. |
| Ordinary view rename | Drop/create-oriented diff | **Answer required** | |
| Materialized view create/drop | Yes | **Plannable** | Drop is destructive. |
| Materialized view rename | Drop/create-oriented diff | **Answer required** | Preserves exact unchanged indexes through PostgreSQL's native rewrite. |
| Materialized view definition rebuild | Yes, via rebuild-style SQL | **Answer required** | Reviewed destructive rebuild; dependent data freshness can require a manual contract. |
| Refresh materialized view after semantic dependency change | No rollout model | **Manual contract** | Operator supplies normal/concurrent refresh and optional verification. |
| Functions create, replace, drop | Yes | **Plannable** | Migra's historical fixtures should not be read as broad modern procedure coverage; onwardpg typed dependencies are limited to catalog-visible references. |
| Routine rename | Drop/create-oriented diff | **Answer required** | Same-signature, fingerprint-bound transition. |
| Procedural-body dependency rewrite | No safe general guarantee | **Blocked** | PostgreSQL does not catalog arbitrary body references. |
| Triggers create/drop/recreate | Yes | **Plannable** | Includes typed table/routine dependency ordering. |
| Trigger rename | Drop/create-oriented diff | **Answer required** | |

## Partitioning, security, and other PostgreSQL objects

| Capability | Migra | onwardpg preview | Notes |
| --- | --- | --- | --- |
| Partitioned-table creation | Yes in final fixtures | **Plannable** | Range/list/hash keys and typed hierarchy. |
| Partition child creation | Yes in final fixtures | **Plannable** | Parent/bound child state is catalog-modeled. |
| Attach / detach partition child | Yes in final fixtures | **Plannable** | onwardpg hazards record locks and possible validation scans. |
| Parent/bound/default-partition reconfiguration | Limited | **Manual contract** | onwardpg will not infer detach/attach order, casts, or data movement. |
| Propagated partition indexes and constraints | Limited | **Plannable** for modeled parent operations | Child resources are handled through their parent; ambiguous independent child mutation is blocked. |
| Roles, grants, default privileges, ownership | Privileges optional | **Table grants plannable; remainder blocked** | Ordinary/partitioned-table grants are typed with safe role quoting and intent questions. Default privileges, column/non-table ACLs, non-owner grant chains, and ownership deviations block. Role creation is outside scope. |
| RLS and policies | Yes | **Plannable** | Enable/force state, policy modes/commands/roles/expressions, typed dependencies, authorization hazards, and fingerprint-bound contractions. |
| Rules and text-search objects | Inspector support varies | **Blocked** | Catalog selectors are reported and require exact validated ignores until modeled. |
| Foreign tables | Inspector support varies | **Blocked** | Explicit unsupported result. |

## What this means in practice

For conventional application schemas—tables, columns, constraints, indexes,
enums, sequences, standard views, and common routines/triggers—onwardpg can
already be more operationally useful than a plain SQL diff because it produces
a staged plan and refuses to invent intent. For advanced PostgreSQL object
families and data-dependent transitions, the correct preview behavior is to
stop with a precise question, manual-work contract, or unsupported diagnostic.

The machine-readable [pgmig comparison ledger](../parity/pgmig-roadmap.json)
tracks ongoing implementation status but is not an exhaustive catalog map;
[supported features](supported-features.md) records the detailed dependency
and current safety semantics.
