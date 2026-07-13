# CLI reference

The smallest, repository-independent command is:

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
| `--unsorted-dump` | Rejected by the URL/DDL CLI. It is reserved for an internal typed snapshot with a validated, complete object order; that output is review-only, not executable migration SQL. |
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

## Development loop

```sh
onwardpg dev plan --target primary-postgres
onwardpg dev plan --target primary-postgres --answers answers.json --sql
```

`dev plan` reads `.onwardpg.toml`, runs the target's `schema_file` or
`schema_command` export twice, rejects nondeterministic output, materializes the
DDL in disposable PostgreSQL, and compares it with the live database named by
`dev_database_env`. It has the same planner questions, answer validation,
options, JSON, SQL, and exit codes as `plan`. It never executes the emitted SQL
against the developer database. After deliberate local application, rerun it
without stale answers to see the residual diff.

Repository configuration can be checked with:

```sh
onwardpg config check --config .onwardpg.toml
```

See [migration bundles](bundles.md) for the current receipt workflow. The
planner, bundle, freshness, and history cores do not require Git; `pr` commands
are convenience wrappers that prepare directory snapshots and provenance.

## Initialize onwardpg history

```sh
onwardpg history init --target primary-postgres
onwardpg history init --target primary-postgres --bundle ground-floor
```

`history init` is the one-time, Git-independent onboarding command for a
database target. It compiles the configured declarative DDL twice, plans from
an empty PostgreSQL catalog, constructs a `purpose: baseline`, `mode: init`
root bundle, and clone-verifies the complete result before installing it under
`bundle_root`. The bundle ID defaults to `baseline`.

The command accepts `--answers`, `--ignore`, and `--concurrent-indexes`. A
`needs_input` or `unsupported` result is emitted without writing history. It
also writes nothing when disposable convergence fails. If any target history
already exists, it exits `4` with `history_already_initialized`; it never
extends, replaces, or imports an existing chain.

Initialization does not inspect or apply to an application database. For an
existing project, its empty-to-schema SQL is replay genesis for clones and new
environments—not work to run against the existing database. Commit and merge
the baseline into the protected base before ordinary feature branches use
`pr regenerate`. Separately run a normal read-only diff against the team's
chosen ground-truth environment before adoption; initialization proves replay
equals declarative code, not that a deployed database has no drift.

## PR status

```sh
onwardpg pr status --base origin/main --target primary-postgres

# Also compare an existing bundle with freshly compiled base/head trees.
onwardpg pr status \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile
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
`synthetic_head_needed: true`. If the PR contribution cannot be applied to the
exact base, status returns `outcome: "conflicting"` with a `merge_conflict`
problem instead of guessing a desired schema.

Without `--bundle`, status reports repository/migration-history classification
only. With `--bundle`, it remains read-only but also compiles the prepared base
and desired trees, replays base history, reruns the recorded planner contract,
and emits `onwardpg.freshness/v1`.
Findings distinguish `history_stale`, `schema_stale`, `decision_stale`, and
`artifact_stale`, and each carries remediation text. A receipt-only commit or
no-op rebase with equivalent schemas and history is fresh with a
`provenance_changed` notice. Freshness checks never replace a draft.

## PR regeneration

```sh
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile
```

Regeneration performs the currently implemented portions of the PR schema
square:

1. classify protected and branch-owned onwardpg bundle files;
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
question receipts are retained without advancing the generation when the
source/planner contract is unchanged. The bundle root is excluded from dirty
source fingerprints, and configuration rejects placing the DDL schema file
inside `bundle_root`.

Regeneration validates and replays the per-target, content-addressed onwardpg
bundle chain as its base, then replays the proposed plan on top and requires
both `BC == BM` and `HC == HM` before writing a ready bundle.

There is intentionally no migration-runner handoff command. Drizzle and other
frameworks may compile declarative schemas to DDL, but onwardpg does not read
or write their journals. Developers and agents run reviewed phase artifacts at
the appropriate rollout points; the live catalog and residual diff show what
work remains.

## Clone verification and CI

```sh
onwardpg bundle verify --target primary-postgres --bundle customer-profile
onwardpg bundle verify --target primary-postgres --bundle customer-profile --through manual

onwardpg ci check \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile
```

`bundle verify` accepts `expand`, `migrate`, `manual`, or `contract` as the
last phase. It can only create and destroy randomly named disposable databases
through the configured admin URL; there is no flag for applying to an existing
database. Exit `0` means the selected checkpoint has no residual relative to
the full bundle; exit `4` reports a residual or execution failure.

`ci check` is the strict, read-only checkout gate. It requires committed input,
exactly one PR-owned bundle directory for the requested target, a fresh and
answer-complete schema-square analysis, a valid hash-chain head, and full clone
convergence. It rejects protected-history edits, forks, altered/unrecorded
artifacts, stale parents, incorrect stacks, unanswered decisions, dirty source
inputs, and non-convergent plans. It writes no repository files and cannot
apply to production.

## Integration boundary

Git is not part of the migration engine. PR analysis accepts prepared base and
desired directories plus opaque revision receipts. The built-in `pr` commands use
Git to produce those inputs because that is convenient in a checkout. CI or
another tool can produce equivalent snapshots without embedding Git behavior
in onwardpg's semantic core.
