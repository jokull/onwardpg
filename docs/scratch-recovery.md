# Recovering a failed scratch cleanup

Use this procedure only on the dedicated scratch PostgreSQL cluster. A cleanup
error names one generated database and one generated login role. Those names
are the recovery scope; do not search by prefix and delete matches in bulk.

1. Copy the **exact** database and role names from the error. Confirm that the
   configured scratch administrator URL points to the intended dedicated
   scratch cluster. Connect as that administrator to its maintenance database
   (usually `postgres`), not to the named scratch database.
2. Inspect both catalog entries and active sessions. For example, in `psql`,
   set `scratch_db` and `scratch_role` to the exact names from the error and run:

   ```sql
   SELECT d.datname, r.rolname AS owner
   FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
   WHERE d.datname = :'scratch_db';

   SELECT rolname, rolcanlogin, rolvaliduntil, rolsuper, rolcreatedb, rolcreaterole
   FROM pg_roles WHERE rolname = :'scratch_role';

   SELECT pid, usename, state, query_start
   FROM pg_stat_activity WHERE datname = :'scratch_db';
   ```

   Review that the database, owner, role, and sessions belong to this failed
   onwardpg run. A missing entry is normal if one DROP already succeeded.
   If the identity is uncertain, stop and investigate before deleting anything.
3. After review, use a short statement timeout and drop the **exact** entries:

   ```sql
   SET statement_timeout = '5s';
   SELECT format('DROP DATABASE IF EXISTS %I WITH (FORCE)', :'scratch_db') \gexec
   SELECT format('DROP ROLE IF EXISTS %I', :'scratch_role') \gexec
   ```

   `DROP DATABASE ... WITH (FORCE)` disconnects active sessions. If a lock or
   other dependency prevents the drop, inspect and resolve that specific
   blocker, then retry the two commands. Do not use `CASCADE` or a prefix-wide
   cleanup command.
4. Re-run the two catalog queries and confirm both exact entries are absent.

`Database.Close()` can be called again after a failed attempt while the process
still holds its `Database` value. It retries these same exact names with bounded
operations. Each DROP has a 30-second deadline; reconnecting after a broken
connection uses the remaining time for that DROP. Closing a connection has a
separate two-second deadline. In the worst case a Close attempt can take about
66 seconds, including attempts to close broken connections. A failed DROP is
not automatically repeated indefinitely; call Close again after resolving the
reported blocker. Once the process exits, use the reviewed manual procedure
above.
