# Testing strategy

onwardpg tests six different contracts. A green test in one layer must not be
used as evidence for a broader layer.

| Layer | Question answered | Authority |
| --- | --- | --- |
| Unit and property | Does one decision, graph rule, renderer, or fingerprint behave deterministically? | Go values and invariants |
| Catalog convergence | Does planned SQL execute and reach a desired PostgreSQL graph? | Native PostgreSQL 15–18 |
| Differential and parity | Which PostgreSQL features are modeled relative to pinned reference tools and catalog inventories? | Pinned corpora and native PostgreSQL |
| Release acceptance | Can the compiled CLI carry one feature through plan, expand, old/new clients, drain, contract, restack, and an empty final residual? | Compiled onwardpg plus native PostgreSQL |
| PGlite preflight | Do scoped phase and client-contract variants work for PGlite's single-connection PostgreSQL 17 subset? | PGlite capability report, always paired with native acceptance |
| Documentation and blind review | Can a developer or source-blind agent follow the public workflow without relying on implementation knowledge? | Replayed receipts and isolated reviewers |

## Release acceptance invariant

Every supported non-additive scenario must prove both sides of the product
promise:

```text
application oracle: prepared legacy SQL and new SQL work after expand;
                    final new SQL and product data expectations work after contract

schema oracle:      independently materialized desired DDL compared with the
                    contracted live database produces an empty onwardpg residual
```

Contract readiness must refuse cleanup before its writer and data evidence is
present. Rebase variants additionally identify exactly which decisions and
developer-owned SQL survive, invalidate, conflict, or disappear through
absorption.

## PGlite boundary

PGlite increases table-driven DDL, data, and client-query variants. It does not
prove separate PostgreSQL backends, writer drain, locks, roles, ACLs, database
isolation, concurrent indexes, or PostgreSQL 15/16/18 catalogs. Each PGlite
variant names a native acceptance owner; a PGlite discovery becomes a minimal
native regression before the product changes.

The fast lane currently contains 84 registered variants across additive and
required columns, CHECK transitions, bidirectional renames, ordinary views,
and materialized views. Registration fails when an ID is duplicated, a native
owner is missing, or a case asks PGlite to prove an unsupported capability.

## CI rule

Required acceptance jobs fail when their database is unavailable or no
scenario runs. They do not silently skip. Failure artifacts include the
redacted command transcript, generated bundle, phase SQL, workload assertion,
and final semantic residual.

The claim-to-receipt index lives in `acceptance/coverage.json` and is validated
as part of the acceptance suite.

## Running the gates locally

Use a PostgreSQL administrative URL which onwardpg is allowed to use for
short-lived databases and roles:

```sh
export ONWARDPG_ACCEPTANCE_DATABASE_URL='postgres://...'

scripts/test-pglite-preflight.sh   # fast PostgreSQL 17 subset
scripts/test-acceptance.sh         # compiled CLI and native release journeys
scripts/test-release.sh            # complete local shipping gate
```

When `ONWARDPG_ACCEPTANCE=1`, a missing native database URL is an error rather
than a skipped green test. CI retains redacted command output, the Go test log,
and generated workspaces on acceptance failure.
