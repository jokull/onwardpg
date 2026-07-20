---
title: Production runbook
description: Operational checks for safely applying an onwardpg bundle.
---

onwardpg gives you reviewed SQL and clone-convergence evidence. The production operator supplies environment evidence through a deliberately weak observer.

## Provision the contract observer

Create one inert grant role and one login role. Run the grants as the owner of
the application schemas and relations; repeat the relation grant when expand
adds a table or view.

```sql
CREATE ROLE onwardpg_observer NOLOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE onwardpg_contract LOGIN PASSWORD 'use-your-secret-manager'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT onwardpg_observer TO onwardpg_contract;

GRANT CONNECT ON DATABASE app_production TO onwardpg_contract;
GRANT USAGE ON SCHEMA app TO onwardpg_observer;
GRANT SELECT ON ALL TABLES IN SCHEMA app TO onwardpg_observer;
```

`contract check` accepts only direct grants to the authenticated role or its
direct NOLOGIN grant role. It rejects nested/built-in memberships, login or
elevated grant roles, write privileges, grant options, and incomplete access.
The readiness receipt lists every projected observer-only grant. Grants to
application roles or `PUBLIC` remain catalog drift. Because this observer
cannot see rows hidden by RLS completely, onwardpg refuses readiness when
application RLS is enabled instead of claiming a partial assertion passed.

## Before expand

- Confirm the bundle is the reviewed immutable artifact and `onwardpg verify --check` passed on the same PostgreSQL major.
- Read every statement’s hazards, transaction boundary, and timeout guidance.
- Estimate table scans, rewrites, index builds, WAL, lock queues, and replica impact against real relation sizes.
- Confirm backups and the application rollback path. A down migration is not generated.
- Ensure the new application can tolerate both the expanded and contracted shapes.

## Apply expand

Run batches exactly as rendered. Nontransactional work such as concurrent index creation must remain outside a transaction. Observe lock waits, error rate, database saturation, WAL generation, and replica lag.

If expand partially fails, stop. Diagnose the exact batch and reconcile forward; do not assume rerunning arbitrary SQL is idempotent.

## Deploy and drain

Deploy the dual-compatible application. Prove old instances and writers are gone, including background workers, scheduled jobs, queues, stale connection pools, and rollback traffic. Run any `after_expand_before_contract` operation artifact through your deployment runner. A one-shot backfill embedded in a contract edit pocket runs later, as part of the exact contract batch, before its dependent enforcement statement.

Potential writers matter, not only current sessions. Include scaled-to-zero or
stale previews with production credentials and ad-hoc/third-party writers.
Create expiring evidence bound to the exact PlanID, bundle entry, environment,
and release.

```sh
onwardpg contract check \
  --target app \
  --bundle "$BUNDLE" \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json
```

`reconciliation_required` means catalog and writer evidence are ready but a
named receipted cleanup/operation must run. Drive the exact `phase_path` through
the release runner, then repeat this read-only check. Proceed to enforcement
only on `ready`. `needs_evidence`, `reconciliation_required`, `blocked`, and
`stale` are distinct states; the command never applies migration SQL.

## Apply contract

Apply the exact contract with the same batch and timeout discipline. Generated
contract SQL repeats data assertions after cleanup, so the prior readiness
check is not stale authority. Contract may validate constraints, revoke
behavior, remove bridges, or perform a final rewrite; “after deploy” does not
mean “low risk.”

## After contract

Confirm application health, database errors, locks, replicas, and the expected schema. Record bundle digest, phases, timestamps, operator, database major, and deployment release. Run [drift check](/reference/cli/#drift-check) on your normal operational cadence.
