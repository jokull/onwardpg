---
title: Django
description: Materialize final Django migration state hermetically, export PostgreSQL DDL, and give one runner ownership of production changes.
---

Django's final migration `ProjectState` is the authoritative desired schema (`W`). onwardpg materializes that state in a new disposable database and plans the rolling-deploy route from accepted history (`H`) to that destination. Run `makemigrations --check` so models and migration state agree. Django must not also execute physical DDL for the same production change.

## Install the hermetic exporter

Copy [`export-schema.sh`](https://github.com/jokull/onwardpg/blob/main/examples/frameworks/django/export-schema.sh) and [`export-current-model-state.py`](https://github.com/jokull/onwardpg/blob/main/examples/frameworks/django/export-current-model-state.py) into your project's `scripts/` directory. The wrapper:

- creates and force-drops a randomly named database;
- sets `PYTHONDONTWRITEBYTECODE=1` so imports do not dirty the checkout;
- refuses a `pg_dump` whose major differs from the server;
- asks Django's `MigrationLoader` for the final `ProjectState`, then uses `SchemaEditor` to materialize those models without executing historical database/data operations;
- sends Django chatter to stderr and only deterministic DDL to stdout; and
- removes Django's runtime migration-recorder table from the desired application schema.

Add an export-only database alias:

```py
# settings.py
import dj_database_url

DATABASES["onward_schema"] = {
    **dj_database_url.config(env="DJANGO_SCHEMA_DATABASE_URL"),
    "ENGINE": "django.db.backends.postgresql",
}
```

Point the wrapper at an administrative **disposable PostgreSQL control plane**, never production:

```sh
export ONWARDPG_DJANGO_ADMIN_URL='postgres://.../postgres'
bash scripts/export-schema.sh > /tmp/django-schema.sql
```

This deliberately does **not** replay `RunPython`, `RunSQL`, or custom migration operations. Those describe history-dependent work, not the final declarative catalog. Product data changes belong in onwardpg's receipted reconciliation/operation workflow; migration state belongs in Django's ledger. Django documents the distinction between database and state operations in `SeparateDatabaseAndState` ([migration guide](https://docs.djangoproject.com/en/6.0/topics/migrations/), [operations reference](https://docs.djangoproject.com/en/6.1/ref/migration-operations/)).

```toml
[targets.app]
schema_command = ["bash", "scripts/export-schema.sh"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_DJANGO_ADMIN_URL"
dev_mode = "workspace"
```

`onwardpg config check` executes the exporter twice. Byte differences, checkout mutations, command failures, and underlying stderr are reported before planning.

## Normal development loop

1. Change models and run `python manage.py makemigrations`.
2. Convert the new migration to a state-only production record as described below. This does not hide the field from `W`: `ProjectState` includes `state_operations`, not the migration's empty `database_operations` list.
3. Run `onwardpg plan feature-name` and follow `next_actions`.
4. Apply only the returned development reconciliation SQL to your dev database.
5. Review and commit the Django migration plus the onwardpg bundle.

Django's one-off default prompt may create an `AddField(... preserve_default=False)` operation. That default is migration-time assistance, not the final database default. If the final model is required without a default, onwardpg still sees `NOT NULL` in `W` and stages nullable expand, reconciliation, and contract.

## Give one runner production ownership

Keep Django's migration graph current without letting it execute the same physical DDL. Wrap the generated operations as state operations with no database operations:

```py
from django.db import migrations, models


class Migration(migrations.Migration):
    dependencies = [("bookings", "0041_previous")]
    operations = [
        migrations.SeparateDatabaseAndState(
            database_operations=[],
            state_operations=[
                migrations.AddField(
                    model_name="booking",
                    name="status",
                    field=models.TextField(),
                ),
            ],
        )
    ]
```

The release sequence is then unambiguous:

1. the migration runner applies onwardpg `expand.sql`;
2. the new Django release deploys while old and new writers overlap;
3. writer evidence, receipted reconciliation, and `contract check` authorize `contract.sql`;
4. the runner applies contract; and
5. `python manage.py migrate --noinput` records the state-only migration.

Do not run step 5 before the physical plan is known to have completed, or a later Django migration may assume schema that contract has not installed. If contract succeeds but state recording fails, rerun the state-only migration. If expand or contract fails, leave Django state unapplied and resume the onwardpg release. `migrate --fake` can mark a normal migration applied, but Django explicitly warns that incorrect fake state may require manual recovery; state-only operations make the ownership decision reviewable in source ([command reference](https://docs.djangoproject.com/en/6.0/ref/django-admin/)).

This workflow is forward-only at the database boundary. Reversing Django state does not undo an onwardpg production contract; author a new forward plan for rollback.
