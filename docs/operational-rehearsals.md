# Operational release rehearsals

The native acceptance suite runs two bounded release rehearsals against a disposable PostgreSQL database. Both use the compiled `onwardpg` CLI to plan and verify a same-type column rename, then apply the generated phase files to a separate workload database.

`TestReleaseOperationalRenameWorkload` starts with 64 populated rows, including a NULL value. Prepared legacy and new clients first update disjoint row ranges, then both update every row for 16 rounds during the expanded phase (2,048 contended updates). The test checks bidirectional reads, NULL propagation, rejection of conflicting dual-column writes, an exact count of 33 updates per row, and final values after contract. It also requires no bridge artifacts and two independent zero-residual comparisons.

`TestReleaseOperationalTransactionalLockCancellation` holds an `ACCESS SHARE` lock while the generated transactional expand batch requests a conflicting table lock. The test observes the exact blocking backend through `pg_stat_activity` and `pg_blocking_pids`, cancels the attempt, and checks that an earlier diagnostic insert in the same batch and the schema change both rolled back. After releasing the lock, it retries the batch, applies contract, and checks the final catalog and data.

Run these through `scripts/test-acceptance.sh` with `ONWARDPG_ACCEPTANCE_DATABASE_URL` set to a disposable administrative PostgreSQL URL. The script requires both exact test names in its event receipt, so a skip or missing run fails the gate. `ONWARDPG_EXPECTED_POSTGRES_MAJOR=18` can assert the server major version.

These fixtures demonstrate transaction rollback, retry, and row-level compatibility behavior. They do not measure production backfill duration, replication lag, WAL volume, or sustained throughput. The single-transaction backfill used here should be reviewed against a real table's size and lock budget before deployment. Operator-batched backfill windows remain a separate procedure requiring explicit progress tracking and verification.
