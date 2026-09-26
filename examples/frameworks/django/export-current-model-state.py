"""Materialize Django migration state without replaying database operations.

This runs inside ``manage.py shell`` after Django has loaded the selected
settings. MigrationLoader computes the final ProjectState (including
SeparateDatabaseAndState state operations), then SchemaEditor materializes
those models without executing historical database or data operations.
"""

from django.db import connections
from django.db.migrations.loader import MigrationLoader


if "onward_schema" not in connections.databases:
    raise RuntimeError(
        "configure DATABASES['onward_schema'] from DJANGO_SCHEMA_DATABASE_URL; "
        "see examples/frameworks/django/README.md"
    )
connection = connections["onward_schema"]
loader = MigrationLoader(connection, ignore_no_migrations=True)
state = loader.project_state()

with connection.schema_editor() as schema_editor:
    for model in state.apps.get_models(include_auto_created=False):
        options = model._meta
        if options.managed and not options.proxy:
            schema_editor.create_model(model)
