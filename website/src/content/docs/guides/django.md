---
title: Django
description: Materialize Django migrations in a fresh database and export the resulting PostgreSQL schema.
---

Django exposes SQL one migration at a time, not one complete desired `CREATE` schema. The reliable adapter is to migrate a fresh disposable PostgreSQL database and dump its schema.

## Add an export-only database alias

```py
# settings.py
import dj_database_url

DATABASES["onward_schema"] = {
    **dj_database_url.config(env="DJANGO_SCHEMA_DATABASE_URL"),
    "ENGINE": "django.db.backends.postgresql",
}
```

This example uses `dj-database-url`; use your project’s existing URL parser if it has one. `DJANGO_SCHEMA_DATABASE_URL` must identify a newly provisioned, empty database on every export.

## Write the exporter

```sh
#!/usr/bin/env bash
set -euo pipefail
: "${DJANGO_SCHEMA_DATABASE_URL:?must point at a fresh disposable database}"

# Keep command chatter away from the DDL stream.
python manage.py migrate --noinput --database=onward_schema >&2

pg_dump "$DJANGO_SCHEMA_DATABASE_URL" \
  --schema-only \
  --no-owner \
  --no-privileges \
  --exclude-table=django_migrations \
  | sed '/^\\restrict /d; /^\\unrestrict /d'
```

Save it as `scripts/export-django-schema`, make it executable, and have your local or CI wrapper provision and destroy the database around the command.

```toml
[targets.app]
schema_command = ["bash", "scripts/export-django-schema"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

:::caution[Data migrations run during export]
Custom `RunPython` and `RunSQL` operations execute while Django builds the disposable database. They must be deterministic and safe on an empty database. Do not point this exporter at a persistent or shared database.
:::

The output is the destination shape; onwardpg still owns the compatibility plan. Continue committing Django model and migration-state changes if your application relies on them, but ensure production does not apply a second migration path for the same DDL.
