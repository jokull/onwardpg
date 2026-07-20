---
title: SQLAlchemy and Alembic
description: Turn Alembic history into deterministic from-empty DDL for onwardpg.
---

Alembic can render the upgrade from base to the current head without connecting to a database. This works well when the full revision chain can build a clean database.

```sh
alembic upgrade head --sql
```

Create an exporter that removes Alembic’s runtime revision table from the desired application schema:

```sh
#!/usr/bin/env bash
set -euo pipefail

alembic upgrade head --sql
printf '%s\n' 'DROP TABLE IF EXISTS alembic_version;'
```

Route log output to stderr in `alembic.ini` or `env.py`; stdout must contain SQL only.

```toml
[targets.app]
schema_command = ["bash", "scripts/export-alembic-schema"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

If the project has multiple Alembic heads, use `heads` or merge the branches intentionally. If offline rendering cannot execute a custom migration, use the Django-style adapter instead: apply `alembic upgrade head` to a fresh disposable PostgreSQL database and `pg_dump --schema-only --no-owner --no-privileges` the result.

Data migrations and dialect-dependent Python must remain deterministic from empty. As with every framework adapter, the output describes the destination; onwardpg generates the compatibility route.

