---
title: Drizzle
description: Export complete Drizzle PostgreSQL DDL and carry one plan through development, merge, and the next feature.
---

Drizzle Kit's official `export` command prints the SQL representation of the configured schema, making Drizzle models a clean desired-state source (`W`) for onwardpg ([Drizzle export reference](https://orm.drizzle.team/docs/drizzle-kit-export)).

```ts
// drizzle.config.ts
import { defineConfig } from "drizzle-kit";

export default defineConfig({
  dialect: "postgresql",
  schema: "./src/db/schema.ts",
});
```

Use the package-manager spelling your repository already owns:

| pnpm | npm | yarn | bun |
| --- | --- | --- | --- |
| `pnpm exec drizzle-kit export --sql=true` | `npx drizzle-kit export --sql=true` | `yarn drizzle-kit export --sql=true` | `bunx drizzle-kit export --sql=true` |

Install dependencies and approve any package-manager build scripts before
running onwardpg. Export is deliberately checked as a read-only operation, so
an exporter invocation that changes lockfiles or package-manager state is
rejected instead of making planning depend on a dirty side effect.

Every model must be reachable from the configured file or glob. If functions, triggers, extensions, policies, or other PostgreSQL objects live outside Drizzle, append their complete deterministic definitions in a small export wrapper. An incomplete export is an incomplete desired schema, not an onwardpg ignore.

:::caution[`pgSchema()` needs its schema DDL]
In a blind Drizzle Kit test, `export` emitted tables qualified with `"app"` but
did not emit `CREATE SCHEMA "app"`. PostgreSQL correctly rejected that stream.
If the model uses `pgSchema()`, use the checked
[`export-schema.sh`](https://github.com/jokull/onwardpg/blob/main/examples/frameworks/drizzle/export-schema.sh)
and list every named schema before invoking Drizzle Kit.
:::

```toml
version = 1
bundle_root = "onward-bundles"

[targets.app]
schema_command = ["bash", "examples/frameworks/drizzle/export-schema.sh"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

## One feature, start to finish

```sh
onwardpg config check
onwardpg plan add-booking-status
```

The first command proves deterministic export and source coverage. `plan` writes the cumulative PR bundle and separately reports any SQL needed only to reconcile the current dev database. For a required field, select a choice from `next_actions`; `manual_sql` writes the exact contract edit pocket before returning `needs_action`.

After editing, run `onwardpg verify`. Commit the Drizzle model and onwardpg bundle, but do not generate/apply a Drizzle production migration for the same change. The production runner owns onwardpg expand → deploy → drain/reconcile → contract.

After merge, pull accepted history and run `onwardpg plan` once more. When the
bundle has been absorbed into that history, onwardpg retires its worktree-local
PlanID automatically. Begin the next feature with `onwardpg plan next-feature`.
On branch switches, onwardpg parks and restores PlanIDs from the bundles visible
in each checkout; there are no lifecycle commands to coordinate.

For a rename or type conversion, show onwardpg both sides of the application contract: typed Drizzle code for the new writer and any raw SQL/queue/preview writer that can still use the legacy shape. That evidence determines the hint and later writer-drain attestation; the schema export alone cannot reveal it.
