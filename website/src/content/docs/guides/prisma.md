---
title: Prisma
description: Generate complete PostgreSQL DDL from an empty source to the current Prisma schema.
---

Prisma’s `migrate diff` can render a script from an empty source to the current declarative schema.

```sh
pnpm exec prisma migrate diff \
  --from-empty \
  --to-schema ./prisma/schema.prisma \
  --script
```

Connect that deterministic stdout stream directly:

```toml
[targets.app]
schema_command = [
  "pnpm", "exec", "prisma", "migrate", "diff",
  "--from-empty",
  "--to-schema", "./prisma/schema.prisma",
  "--script"
]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

Run `onwardpg config check` whenever Prisma or its engines change. Native type annotations matter: for example, Prisma `String` may map to different PostgreSQL types depending on the schema declaration.

Prisma Schema Language does not represent every PostgreSQL feature. If your application relies on custom functions, triggers, extensions, or other SQL, wrap the command in a script that appends their deterministic full definitions.

:::note[Migration ownership]
Keep Prisma’s model as the desired-state source, but do not run `prisma migrate deploy` and the onwardpg bundle for the same production change. Pick one system to own application of that DDL.
:::

