# Plan-loop shipping checkpoint

Status: shipped on `main`. The implementation, PostgreSQL 15–18 matrix, blind
DX retests, documentation receipts, commit, and push are complete. The exact
commit is recorded in the final handoff rather than hard-coded into its own
tree.

## Outcome

Ship one elegant authoring loop:

```sh
onwardpg plan FEATURE-NAME
# edit schema, answer a decision, switch branches, or rebase
onwardpg plan
```

The command maintains one evolving durable migration from accepted history
(`H`) to the declarative working schema (`W`) while independently describing
how a caller-owned development database (`D`) can catch up. It never treats
local database state as migration history and never applies SQL automatically.

This checkpoint fixes only correctness, safety, and material DX failures found
by the field tests. Broader release simulations and exhaustive ecosystem
harnesses are follow-up work, not gates for this shipment.

## Delivered in the working tree

### One automatic plan lifecycle

- `plan FEATURE-NAME` creates a local PlanID once; later plain `plan` calls
  revise that same logical migration.
- Branch switches park and restore identities based on the bundle visible in
  the checkout.
- Rebases recalculate the same bundle from the new accepted history.
- When accepted history absorbs a generated bundle, `plan` removes the empty
  bundle and retires its local selector automatically.
- No lifecycle verbs, planner modes, scopes, or speculative protocol versions
  were added; machine output is status-oriented.

### Durable planning stays separate from local reconciliation

- The durable bundle remains strictly `H → W`.
- Development reconciliation remains strictly `D → W`.
- Workspace mode preserves objects found only in `D`, so parallel branch work
  is neither deleted nor absorbed into the durable migration.
- Incompatible changes to the same local object stop for a development-only
  decision.

### Agent-ready workspace fast-forward

When `D → W` has safe statements, ordinary `plan` output includes a
`workspace_fast_forward` action containing:

- rendered SQL;
- statement count;
- `accepted_history_changed` after a rebase, otherwise
  `development_database_behind_desired_schema`;
- every preserved D-only object; and
- exact replay argv for `onwardpg plan --output sql`.

Consumed `--dev-hint` values are repeated cumulatively in later choice and SQL
argv. If `H = W` or an absorbed bundle has already retired, replay argv names
the plan explicitly and remains executable without an active durable bundle.

Top-level `ready` means the durable artifact is ready. A development
fast-forward remains an explicit optional `next_actions` item because onwardpg
does not mutate caller-owned databases.

### Legible decisions, edits, and verification

- Durable and development statuses are separate and accompanied by ordered
  executable next actions.
- Raw pre-edit planner output is named `durable.generated_plan`; the effective
  bundle state remains `durable.status`.
- `base_history_head` identifies accepted history, while verification
  `history_head` identifies the finalized chain including the selected bundle.
- Decision history is normalized before clone verification, so embedded,
  installed, and standalone verification digests agree.
- Manual SQL handoffs write the candidate and bounded edit pocket before
  returning.
- A pocket has one executable `ONWARDPG TODO`; unresolved SQL is rejected
  structurally with the exact file and line before PostgreSQL execution.
- Required contract gates, optional clone assertions, and operator work remain
  distinct artifacts.

### Contract readiness and PostgreSQL correctness

- Contract enforcement binds to typed data and writer gates.
- Operator-batched and externally attested work is represented separately from
  transactional phase SQL.
- A dedicated observer can inspect readiness through a validated
  least-privilege overlay without hiding unrelated application ACL drift.
- CHECK, uniqueness, exclusion, foreign-key, required-column, rename, and
  type-transition paths retain the loose expand / gated contract safety model.
- A standalone unique index recreated after dropping its owning constraint now
  carries its reconciliation gates on the real deferred `CREATE INDEX`.
- Partitioned exclusion replacement permits PostgreSQL-owned child
  dependencies while still requiring reviewed reconciliation.

### Framework and agent documentation

- Django has a deterministic ProjectState exporter that does not execute
  historical `RunPython` or `RunSQL`.
- Drizzle has a checked `pgSchema()` wrapper that emits named schema DDL and a
  standalone config example.
- Prisma 7 has a `prisma.config.ts` recipe and a wrapper that rejects empty
  successful export output.
- The README, plan narrative, framework guides, production runbook, and agent
  skill describe the same `H/W/D` model and plan-only lifecycle.
- Documentation SQL is tied to executable receipts.

## Blind field evidence

The agents began with the README and public docs, used isolated `/tmp`
projects and disposable PostgreSQL 17 containers, and did not patch onwardpg.

Initial reports:

- Drizzle: `/tmp/onwardpg-drizzle-blind.tUbm4z/`
- Prisma: `/tmp/onwardpg-prisma-dx-itMkTU/REPORT.md`
- SQLAlchemy: `/tmp/onwardpg-dx-sqlalchemy-6/REPORT.md`

Targeted retests on the repaired worktree:

- Drizzle: `/tmp/onwardpg-drizzle-retest.Lz6CFa/`
- Drizzle no-change replay:
  `/tmp/onwardpg-drizzle-bootstrap-retest.rwfziG/`
- Prisma 7: `/tmp/onwardpg-prisma-retest-wKAUBr/REPORT.md`
- SQLAlchemy: `/tmp/onwardpg-dx-targeted-7/REPORT.md`

The targeted claims passed: cumulative development argv, executable workspace
fast-forward, unambiguous generated/effective plan naming, identical finalized
history digests, exact unresolved-TODO diagnostics, deterministic checked
framework exports, repeated plain `plan`, and the next-feature lifecycle.

## Shipping gates

Only failures in these gates may expand this checkpoint:

- [x] complete the full repository matrix on PostgreSQL 15, 16, 17, and 18;
  after the final PostgreSQL 18-only classification fix, rerun the affected
  graph-plan package on 15–17 and the full suite on 18;
- [x] keep the focused plan-loop, fast-forward, edit, digest, reconciliation,
  and least-privilege regressions green;
- [x] run `go test ./... -count=1` without a database;
- [x] run `scripts/test-readme-workflow.sh`;
- [x] run `scripts/test-documentation-receipts.sh`;
- [x] run `node scripts/check-documentation.mjs`;
- [x] run `pnpm --dir website check` and `pnpm --dir website build`;
- [x] run `git diff --check` and review the complete staged scope;
- [x] stop the disposable containers created for this work;
- [x] commit the intended working tree and push `main`.

## Deferred follow-up

These are worthwhile but are explicitly not blockers for this checkpoint:

- an exhaustive closure test that replays every emitted decision choice through
  the public CLI;
- repository-owned Drizzle, Django, Prisma, SQLAlchemy, and plain-SQL framework
  projects in CI;
- a high-fidelity old/new-writer deployment harness with queues, reconnecting
  previews, writer evidence, retries, and contract application;
- an end-to-end large-table operator-batched rename journey;
- direct normalization receipts for every default-equivalent owner ACL form;
- a retained hidden-developer/two-agent relay and separate blind-human review;
- richer standalone dirty-build identity;
- cosmetic normalization of every nested question/action shape.

Reopen one of these only as a focused goal. Do not infer that it is already
implemented from the current documentation or field reports.

## Final handoff

The shipping report records:

1. the exact commit pushed to `main`;
2. PostgreSQL 15–18 matrix outcomes;
3. documentation, website, README, and receipt outcomes;
4. the three blind retest verdicts;
5. any matrix-discovered correctness fixes; and
6. confirmation that disposable containers were removed.
