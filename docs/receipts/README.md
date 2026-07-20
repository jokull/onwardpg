# Documentation receipts

Public examples that look like onwardpg output are grounded here instead of
being invented in prose. `scripts/test-documentation-receipts.sh` builds the
current CLI and, against disposable PostgreSQL:

1. initializes the checked-in `base.sql`;
2. replaces it with `desired.sql` and runs the documented `plan` command;
3. compares the emitted phase files byte-for-byte with the generated receipts;
4. installs the explicitly labeled application-owned required-column cleanup
   and optional clone assertion, while preserving the generated inline contract
   gate;
5. runs `verify` and then read-only `verify --check`;
6. proves positional `verify NAME` is rejected; and
7. runs `TestRequiredColumnStagingConvergesOnPostgreSQL`, which inserts a row
   through a legacy writer after expand before completing contract; and
8. runs the ordinary-view and materialized-view type-change closure integration
   tests against PostgreSQL, including their edited conversion pockets.

`scripts/check-documentation.mjs` separately ties the website's displayed code
blocks to these files, rejects stale verify syntax and protocol versions, and
checks that every named Go test in the documentation still exists.

The scenarios are intentionally small:

- `required-column/` captures generated expand/contract SQL, the generated
  inline contract gate, a clearly separated product edit, and an optional
  clone assertion.
- `type-change/` captures the real `manual_sql` expand and contract handoffs;
  it demonstrates that onwardpg does not present a bare `ALTER TYPE` as a
  rolling-deployment bridge.
- `rename/` captures the complete same-type column compatibility bridge,
  including synchronization, an explicitly accepted single-transaction
  backfill, equality assertion, cleanup, and final native rename.
- `dependency-type-change/` captures a table column beneath an ordinary view,
  materialized view, and unique index. It proves the generated reverse-drop and
  forward-recreation order around the application-owned conversion pocket.

Regenerate by changing the planner and deliberately reviewing the resulting
black-box diff. Do not hand-edit a receipt merely to make CI green.
