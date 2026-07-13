# CLI reference

The only command today is:

```sh
onwardpg plan --from SOURCE --to SOURCE [options]
```

`SOURCE` is either a PostgreSQL URL or `file:///absolute/path/schema.sql`.
Live URLs are catalog-inspected read-only. A DDL file is executed in a
disposable database reached through `--dev-url`, then catalog-inspected; it is
not parsed by onwardpg. Treat a DDL file as executable input and use an
isolated disposable database role.

| Flag | Meaning |
| --- | --- |
| `--from SOURCE` | Current database or declarative DDL source. Required. |
| `--to SOURCE` | Desired database or declarative DDL source. Required. |
| `--dev-url URL` | Administrative/development PostgreSQL URL required whenever either source is `file://`. |
| `--answers FILE` | Fingerprint-bound JSON answer document from a prior `needs_input` result. |
| `--ignore SELECTOR` | Narrowly ignore a catalog blocker. Repeatable; every selector must match an ignored object on at least one side. |
| `--concurrent-indexes` | Create standalone indexes concurrently in non-transactional batches. |
| `--if-not-exists` | Add `IF NOT EXISTS` where the renderer supports it for schema/table creates. |
| `--if-exists` | Add `IF EXISTS` where the renderer supports it for schema/table drops. |
| `--cascade-drops` | Permit `CASCADE` for supported schema/table drops after destructive approval. |
| `--sql` | Print reviewable, commented SQL instead of JSON only for a ready plan. It includes `EXPAND`, `MIGRATE`, `CONTRACT`, and batch-boundary comments. |
| `--indent STRING` | Prefix every line in `--sql` output. |
| `--unsorted-dump` | Rejected by the URL/DDL CLI. Only a typed `adapter.OrderedSnapshot` with a validated, complete order may request it through the library boundary; that output is review-only, not executable migration SQL. |
| `--bundle PATH` | Write a versioned receipt bundle in addition to the normal stdout result. |
| `--bundle-id ID` | Stable logical feature/bundle identifier. Required with `--bundle`. |
| `--target NAME` | Database target name. Required with `--bundle`. |
| `--bundle-purpose VALUE` | `feature` (default), `repair`, or `contract`. |
| `--bundle-mode VALUE` | `pr` (default), `develop`, `release`, or `verify`. |
| `--base-ref REF` | Logical base ref receipt; required for a PR bundle. |
| `--base-commit SHA` | Full lowercase base commit; required for a PR bundle. |
| `--head-revision VALUE` | Head commit or dirty-tree digest; required for a PR bundle. |
| `--intent FILE` | Markdown developer-intent receipt included in the bundle. |
| `--replace-draft` | Replace only a validated, unexecuted bundle draft while preserving prior decisions. |

Identifier quoting and expression rendering are performed by the planner from
catalog state. Do not interpolate shell values into DDL merely to satisfy a
planner question; record intended answers in the answer file.

See [the protocol](protocol.md) for exit codes and JSON fields.

Repository configuration can be checked with:

```sh
onwardpg config check --config .onwardpg.toml
```

See [migration bundles](bundles.md) for the current receipt workflow and its
explicit Git-provenance boundary.

## PR status

```sh
onwardpg pr status --base origin/main --target primary-postgres
```

This read-only command resolves the base, head, and merge base to exact object
IDs and classifies migration-path changes from `.onwardpg.toml`. It includes
the working tree by default when the head is `HEAD`; use
`--working-tree=false` for committed state only.

The merge-base-to-head patch is the PR contribution. The exact base tree is
the protected migration lineage. This distinction prevents an unrelated
migration newly landed on main from appearing as a branch deletion. Existing
base-history edits and concurrent path collisions produce `outcome: "blocked"`
and exit `4`. An advanced base produces `relationship: "base_advanced"` and
`synthetic_head_needed: true`. `pr status` remains read-only and does not
compile that tree.

## PR regeneration

```sh
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile
```

Regeneration performs the currently implemented portions of the PR schema
square:

1. classify protected and branch-owned migration files;
2. compile the declarative schema from the exact base commit twice;
3. replay the base migration history in disposable PostgreSQL and require
   `BC == BM`;
4. construct the would-be merge of the PR contribution onto the exact base,
   overlay a fingerprinted dirty checkout when requested, and compile `HC`
   twice;
5. plan `BC → HC`; and
6. write the decision or ready phase bundle under the configured
   `bundle_root`.

The configured `dev_database_env` must contain a disposable PostgreSQL admin
URL. A second decision pass uses `--answers` and `--replace-draft`; prior
question receipts are retained. The bundle root is excluded from dirty source
fingerprints, and configuration rejects any overlap between `bundle_root` and
the migration runner's `migration_path`.

This command does not yet write a Drizzle/ORM migration journal entry. Its
schema-square receipt therefore reports `head_artifact_fidelity:
"not_generated"`. The adapter handoff and `HC == HM` clone proof remain an
explicit missing layer, not a successful claim.
