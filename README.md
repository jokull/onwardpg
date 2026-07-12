# onwardpg

`onwardpg` is a forward-only PostgreSQL migration planner for people and
agents.

It compares two desired/current schema states, emits a reviewable forward SQL
plan, and never invents answers to questions that schema state cannot answer:

- is this a rename or a drop/create?
- what cast/backfill expression makes this type conversion safe?
- is removing this table or column intentional?

Those questions are JSON, not terminal prompts. An agent supplies a separate
answer document and re-runs the same command.

## Principles

- **Forward only.** No generated down migrations. Production rollback is a new,
  explicitly reviewed forward migration.
- **Diff first.** The source of truth is the current and desired schema state,
  not a linear history that can drift from either.
- **Contracts over guesses.** A schema diff can identify ambiguity, but cannot
  safely resolve it. Intent is a versioned input to planning.
- **Expand, then contract.** Additive operations are emitted in the `expand`
  phase. Drops, direct renames, and data-sensitive operations are marked
  `contract`: they are still forward migrations, but should land only after the
  application no longer relies on the prior contract. `onwardpg` does not
  pretend a generated down migration is a rollback strategy.
- **Postgres is the SQL parser.** A `file://` DDL source is loaded into a
  disposable PostgreSQL database and then catalog-inspected. This accepts DDL
  emitted by Drizzle without implementing a partial SQL parser.
- **Agent-native.** Standard output is JSON; exit codes communicate whether a
  plan is ready, answers are required, or a schema feature is unsupported.

## Intended interface

```sh
onwardpg plan \
  --from postgres://app@localhost/current \
  --to file://schema.sql \
  --dev-url postgres://admin@localhost/postgres \
  --ignore extension:pgcrypto
```

`0` means `planned`, `2` means `needs_input`, and `3` means `unsupported`.

Each planned statement carries `phase: "expand" | "contract"`, `safety`, and
SQL. The CLI never applies a plan. An agent or CI workflow can commit the
expand phase, deploy compatible application code, then create a separate
forward contract migration after observing the rollout.

The initial implementation is PostgreSQL-only. It is deliberately a new Go
codebase rather than a wrapper around Atlas: Atlas is the behavioral benchmark;
onwardpg's distinctive contract is its explicit, resumable agent question
protocol and forward-only migration philosophy.

## Current implementation boundary

The first Go slice plans ordinary tables, columns, primary/unique/check/foreign
key constraints, type-conversion questions, and destructive-change questions.
Indexes, extensions, routines, sequences, views, materialized views, partitions
and other unmodelled resources are reported as `unsupported` rather than being
silently ignored. This is intentional: safety comes before broad feature claims.
