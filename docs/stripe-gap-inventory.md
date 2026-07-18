# Stripe pg-schema-diff gap inventory

This inventory compares onwardpg with Stripe `pg-schema-diff` v1.0.7 at the
pinned commit `6208f8f3ceccae8ca634055dc47907a6a864cb76`. The detailed
415-case receipt remains in
[`parity/stripe-pg-schema-diff-v1.0.7.json`](../parity/stripe-pg-schema-diff-v1.0.7.json);
this page is the human-scale map.

The labels below describe how often a branch tends to matter, not how important
it is when it does matter.

```text
PostgreSQL schema changes
├── Everyday schema                         very likely
│   ├── tables, columns, defaults, nullability, type changes
│   └── sequences/identity, enums, extensions, schemas
│
├── Integrity and access paths              very likely
│   ├── primary/unique/check/foreign-key constraints
│   └── ordinary, unique, expression, and concurrent indexes
│
├── Derived data and database code          likely → occasional
│   ├── views                               likely
│   ├── functions and triggers              occasional
│   └── materialized views and procedures   less common
│
├── Partitioning                            niche, but central at scale
│   ├── partitioned tables and parent indexes
│   └── local indexes, attachment, and hierarchy replacement
│
├── Database security                       situational, high consequence
│   ├── grants and grant options
│   └── row-level-security policies
│
└── Planner workflow and physical layout    deliberately out of scope
    └── table data-packing optimization and Stripe's apply workflow
```

## What the gaps mean

### Everyday schema

This is the branch most teams will encounter. Onwardpg already models the
ordinary lifecycle and has strong PostgreSQL convergence evidence, especially
for identity/serial columns, sequences, generated columns, enums, extensions,
and basic table/column changes. The Stripe gap is mostly **certification debt**:
Stripe's exact fixtures have not yet been run differentially case by case.

The likely behavioral differences are concentrated in direct column type
casts and online `NOT NULL` transitions. Stripe is willing to perform more
one-deployment casts and validation choreography; onwardpg may require an
expand/contract handoff when application-version overlap is not provably safe.

### Integrity and access paths

Constraints and indexes are just as common as columns. Both planners cover the
normal operations. Stripe's acceptance suite is particularly detailed about
online index builds, constraint-backed indexes, validation order, and avoiding
periods without a usable index. Onwardpg has adopted continuous index and
primary/unique replacement primitives, but **81 of 83 cases still lack exact
case-level differential certification**. This is the highest-priority audit
branch because it is common and operationally sensitive.

### Derived data and database code

Views are common; functions and triggers depend on how much logic a team keeps
inside PostgreSQL; materialized views and procedures are less common. Recent
onwardpg work covers lifecycle and dependency ordering broadly, but Stripe has
many combinations involving recreated tables/columns and dependent objects.
The remaining gap is mostly **dependency/rebuild proof**, with a genuine safety
boundary around dependencies hidden inside opaque procedural bodies.

### Partitioning

Most applications never need advanced partition maintenance. For time-series,
audit, or very large multi-tenant tables it becomes central. Onwardpg directly
handles normal hierarchy creation, partition add/remove or attach/detach, and
several parent/local index operations. Parent changes, bound/key/strategy
rewrites, and ordinary↔partitioned conversions remain explicit handoffs
because synchronization and cutover policy are product decisions. They are no
longer blank handoffs: onwardpg enumerates both trees and emits deterministic
shadow identities, key/index and partition-local scaffolding, copy/catch-up
stages, blocking row/bound assertions, the typed derived-object closure, a
brief rename cutover, and separately authorized cleanup. Stripe's pinned
conversion cases use a data-deleting DROP/CREATE replacement, while its
partition-key case rejects; the differential receipt records onwardpg's
retained-data runbook as a deliberate strategy difference. These operations
remain niche for ordinary applications and operationally significant at
scale.

### Database security

Grants are common in tightly controlled environments; RLS is less common but
critical when used. Onwardpg deliberately differs from Stripe by requiring
fingerprint-bound authorization for policy changes, revocations, and grant
contractions. The five currently proven cross-planner differences are safety
choices, not missing catalog support. Remaining uncertainty is around role
edge cases and policies/privileges on partitions.

### Workflow and physical layout

Stripe can optimize column packing and includes an application workflow.
Onwardpg intentionally does neither: declarative column order is compatibility
information, not a reason to rewrite a table, and onwardpg retains its no-apply
boundary. These two acceptance cases are out of scope rather than product gaps.

## Summary

The pinned corpus contains 415 cases:

- **31** have exact differential parity evidence;
- **11** have exact differential evidence of a deliberate strategy difference;
- **10** are supported with onward-only evidence but lack an exact Stripe run;
- **361** remain family-classified and need case-level differential audit;
- **2** are deliberately out of scope;
- **0** are currently proven missing—but that means “not yet demonstrated,”
  not that the 361 unresolved cases are known to work.

The practical order is: certify everyday columns/constraints/indexes first,
then views and dependency-heavy database code, then advanced partitioning.
Security should be compared for final catalog outcome while preserving
onwardpg's stricter authorization model.
