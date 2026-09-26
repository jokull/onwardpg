# Adopting onwardpg in an existing application

Keep your framework models. Give onwardpg a deterministic export of the complete
desired PostgreSQL schema, and choose one runner to apply future DDL.

1. Start with the models and database at the same, already-deployed revision.
   Resolve existing drift before adopting a new migration planner.
2. Configure the exporter and a dedicated scratch cluster. Run `onwardpg config
   check` and `onwardpg init` **before editing the models**. The baseline is for
   replay in scratch or an empty database; do not apply it to the existing database.
3. Edit models, export their new schema, then run `onwardpg plan NAME`. Follow
   its decisions, edit the indicated SQL pockets, and run `onwardpg verify`.
   Use each complete suggested `argv` command; during an edited-plan revision
   it can carry earlier confirmations that are not yet saved.
4. Rehearse expand, application overlap, and contract on a populated disposable
   copy. Your deployment runner applies those phases. Remove the old framework
   DDL runner from that deployment path so the change is not applied twice.

Generated phase files contain `-- onwardpg:batch` directives. They are comments
to `psql`, not executable transaction controls. Your runner must execute each
transactional batch in its own transaction and each nontransactional batch
outside one. Neither bare `psql -f` nor wrapping a whole mixed phase in `psql -1`
reproduces those boundaries. See [the batch format](../../docs/bundles.md#integrity).

Keep historical framework files and journal tables. onwardpg does not import or
update their applied-migration records. Bootstrap new environments with the
chosen runner. A retired Drizzle or Prisma migrator needs no new journal rows;
Django still needs a deliberate migration-state workflow, described below.

Recipes:

- [Drizzle](drizzle/README.md): TypeScript models, SQL export, migration-runner handoff.
- [Django](django/README.md): migration-state export, settings, and state-only migrations.
- [Prisma](prisma/README.md): schema export, client generation, migration-runner handoff.

## Optional development and journal comparisons

Start with `schema_command` and `scratch_database_env`. Add `dev_database_env`
when you also want SQL for reconciling your local database. Its `--dev-hint`
decisions are separate from durable `--hint` decisions; a verified durable
bundle can coexist with an exit-2 development question. Inspect both sections
of the result. Leaving development configuration out does not weaken clone
verification of the durable bundle.

The development URL can use the same dedicated read-only observer as contract
checks. Its minimum inspection grants are projected out of the comparison.
A database-owner comparison retains grants to other roles, which can appear as
unsupported ACL changes. See [observer access and development planning](../../docs/migration-workflow.md)
before changing or excluding privileges.

Exporters normally omit framework bookkeeping such as `django_migrations` or
`drizzle.__drizzle_migrations`. Workspace development mode preserves those extra
objects. A strict live drift comparison can still report them even when app
tables match. Inspect them and use exact `ignore` selectors for bookkeeping you
deliberately leave with the framework. Do not ignore application tables to make
drift disappear. Schema selectors are not recursive; review the table and any
standalone sequence separately.

See [schema inputs](../../docs/schema-inputs.md) for selector validation and
[contract readiness](../../docs/contract-readiness.md) for writer evidence.
