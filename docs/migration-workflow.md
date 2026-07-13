# Forward-only migration workflow

1. Produce the desired schema from your ORM or declarative source.
2. Plan from the real current catalog to that desired schema:

   ```sh
   onwardpg plan --from "$DATABASE_URL" --to file://"$PWD/schema.sql" \
     --dev-url "$DEV_POSTGRES_URL" > plan.json
   ```

3. If the command exits `2`, review the typed questions. Commit a
   fingerprint-bound answer document, then rerun with `--answers`.
4. If it exits `3`, remove or narrowly ignore the unsupported catalog state,
   or write a manually reviewed migration. Do not treat `unsupported` as an
   empty diff.
5. For a manual-work question, supply reviewed statements, verification SQL,
   and an explicit transactional/non-transactional execution mode in the
   answer file. The resulting `MANUAL` batch is executable only as reviewed;
   onwardpg never creates its contents.
6. Review SQL, hazards, and batch boundaries. `--sql` groups the emitted file
   with `EXPAND`, `MIGRATE`, and `CONTRACT` comments: run expand before the
   compatible deployment, put/run observed application-specific backfills in
   migrate, and defer contract until old code is gone. Store reviewed SQL as a
   forward migration in the application repository. onwardpg never applies it.
7. Test the reviewed plan against a clone with representative data. Confirm a
   new onwardpg plan from the migrated clone to the desired state is empty.
8. Apply compatible `expand` work, deploy application code that tolerates both
   old and new shapes, perform any required data migration/backfill outside the
   generated DDL, then plan/review/apply the later `contract` work.

Do not generate or rely on down migrations. Recovery is a new, reviewed
forward migration. In particular, a rename, type conversion, dropped object,
or `NOT NULL` transition may require application rollout, data validation, and
an explicit backfill decision that schema state alone cannot provide.

For concurrent indexes, execute the non-transactional batch exactly as
reported; PostgreSQL rejects `CREATE INDEX CONCURRENTLY` inside a transaction.
Every other batch marked transactional should be executed as one transaction:
the integration suite verifies that a failing statement rolls back earlier
statements in that batch. Do not combine or split reported batches casually.
