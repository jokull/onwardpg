# Django

This recipe exports **Django migration state**, not unapplied edits to
`models.py`. Run `makemigrations` after editing models. The exporter checks for
model changes missing from migration state and fails instead of exporting stale
DDL. It materializes the final state without executing historical `RunPython`
or `RunSQL` operations.

## Set up the exporter

Copy `export-schema.sh` and `export-current-model-state.py` into the same
directory in your project, for example `scripts/onward/`. Run them from the
directory containing `manage.py`, with Django and its PostgreSQL driver installed.
Provide `psql` and a `pg_dump` matching the scratch server major. If the matching
client is outside PATH, set `PG_DUMP=/path/to/pg_dump`.

Add this export-only alias to your settings; leave `default` unchanged:

```python
import os
from urllib.parse import parse_qsl, unquote, urlsplit

if url := os.environ.get("DJANGO_SCHEMA_DATABASE_URL"):
    parsed = urlsplit(url)
    DATABASES["onward_schema"] = {
        "ENGINE": "django.db.backends.postgresql",
        "NAME": unquote(parsed.path.lstrip("/")),
        "HOST": parsed.hostname or "",
        "PORT": parsed.port or "",
        "USER": unquote(parsed.username or ""),
        "PASSWORD": unquote(parsed.password or ""),
        "OPTIONS": dict(parse_qsl(parsed.query)),
    }
```

The script creates this database from `ONWARDPG_SCRATCH_DATABASE_URL`, then drops
it on exit. `ONWARDPG_DJANGO_ADMIN_URL` optionally overrides that scratch URL.
Use a PostgreSQL URL with connection identity in its authority/path; query
parameters are PostgreSQL driver options such as `sslmode`.

```toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_command = ["bash", "scripts/onward/export-schema.sh"]
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
```

Run `onwardpg config check` and `onwardpg init` with the old migration state,
before changing models. Then edit models, run `makemigrations`, and plan.

If you compare with an existing Django database, inspect and exclude its
bookkeeping table before planning, so live contract checks use that same
boundary:

```toml
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
dev_mode = "workspace"
ignore = ["table:public.django_migrations"]
```

These fields belong under `[targets.app]`. Set the dev URL to the local
database containing the journal before `config check`. Inspect any separately
reported sequence or nonstandard journal schema instead of broadening the
ignore to all tables.

## Keep Django state while onwardpg owns DDL

For a reviewed schema-only change, wrap the newly generated Django operations:

```python
operations = [
    migrations.SeparateDatabaseAndState(
        database_operations=[],
        state_operations=[
            # The generated RenameField, AddField, etc. go here.
        ],
    ),
]
```

The exporter sees the new state, while `migrate` records that state without
executing its DDL. Your runner applies onwardpg expand first; the new app must
tolerate the overlap schema. Record the reviewed state-only migration at the
appropriate application rollout step, drain old processes, then complete
contract. Check `makemigrations --check --dry-run` and `migrate --plan` afterward.

Do not wrap unrelated data operations or third-party migrations blindly.
`RunPython` data work and `RunSQL` objects absent from model state need an
explicit execution/export plan. The exporter does not reproduce them. Django
may also need data such as content types and permissions created by migration
signals; verify those application requirements separately.

A fresh environment must use an explicit bootstrap path. Either replay onwardpg
baseline/history and reconcile Django's journal after verifying the schema, or
apply the old Django baseline migrations and then onwardpg feature phases.
Never apply both baselines. Running Django's chain alone leaves the old physical
schema because the new migrations are state-only. Do not use a blanket
`migrate --fake` as an adoption shortcut.
Use the same bootstrap in test-database setup; Django's default test runner
replaying a chain with state-only changes cannot create those physical changes.

See Django's [SeparateDatabaseAndState documentation](https://docs.djangoproject.com/en/5.2/ref/migration-operations/#separatedatabaseandstate).
