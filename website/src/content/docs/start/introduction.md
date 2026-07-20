---
title: Introduction
description: Plan the PostgreSQL compatibility window around an application deployment.
---

onwardpg is a PostgreSQL migration planner built around two facts most diff tools leave to the developer:

1. Old and new application code overlap, so the migration needs a compatibility window—not only a final schema.
2. A feature and its accepted migration base keep changing, so intent must survive inside one evolving plan—not a growing stack of draft fixups.

The core loop is one command:

```sh
onwardpg plan checkout-preferences

# Repeat after model edits, decisions, and rebases.
onwardpg plan
```

:::tip[Using a coding agent?]
Point it at [`https://onwardpg.solberg.is/skill.md`](/skill.md) before it changes the schema. The skill contains the operating loop, evidence boundaries, stop conditions, and required review handoff.

```text
Read and follow https://onwardpg.solberg.is/skill.md for this migration.
Inspect this repository's schema source, accepted migration history, and the
application paths affected by the change. Maintain one evolving onwardpg plan,
supply only evidence-backed decisions, never apply to production, run onwardpg
verify, and report the operational gates verification cannot prove.
```
:::

It turns replayed accepted history and exported working DDL into one reviewable, forward-only bundle around one application deployment:

```text
compare -> expand -> deploy -> drain -> contract -> prove convergence
```

The first call creates one `PlanID`; later calls revise and restack that same logical migration. Decisions whose meaning still holds are carried forward. Decisions invalidated by schema erosion are rejected and asked again.

It is built for the awkward truth behind “simple” schema changes: a column rename can break an old reader, a new `NOT NULL` column can break an old writer, and a cast from `text` to `integer` needs a product rule for malformed values.

The [plan-command walkthrough](/concepts/plan-command/) follows that complexity
curve from easy to nightmare. At the far end, an agent supplies product-aware
conversion SQL while onwardpg still discovers, orders, recreates, and verifies
the PostgreSQL dependency closure around it. The examples are backed by current
CLI phase files and executable integration tests.

## The operating model

**Expand** changes the database while old code is still running. It adds compatible shape, synchronization, indexes, and explicitly reviewed data work.

**Deploy** rolls out code that understands both the expanded and contracted schemas.

**Drain** is an operational fact, not a timer: old instances, workers, queues, connection pools, and stale write paths are gone.

**Contract** validates assumptions, tightens constraints, and removes compatibility scaffolding.

**Verify** replays the exact bundle in disposable PostgreSQL and proves that the final catalog converges on the declared schema.

:::caution[Planner, not deployer]
onwardpg never applies SQL to production or to another caller-owned database. Your deployment system runs the reviewed phase files because it is the system that can observe rollout and drain state.
:::

## What makes it different

onwardpg knows which schemas are playing which role. The durable migration is replayed history → working intent. A separate development reconciliation handles a branch-worn local database without contaminating the production bundle. Production appears only in an explicit drift audit or as externally gathered read-only evidence. Verification uses independent disposable replays.

Desired DDL is materialized in real PostgreSQL, then inspected as typed catalog objects and dependencies. When schema state cannot prove intent—a rename, destructive change, backfill, or cast—onwardpg asks for a semantic decision, captures it in the plan, and binds it to the exact participating schema scope instead of guessing.

The proof is deliberately narrow. Catalog convergence does not prove safe lock duration, application compatibility, valid production data, WAL budget, replica health, or rollback readiness. Those remain operational review inputs.

## Where to begin

1. [Install and configure onwardpg](/start/installation/).
2. Understand [the plan command](/concepts/plan-command/).
3. [Create your first plan](/start/first-plan/).
4. Read the [expand/contract contract](/concepts/expand-contract/) before wiring deployment automation.
5. If a coding agent authors migrations, use the [agent-assisted planning workflow](/agents/agent-assisted-planning/).
