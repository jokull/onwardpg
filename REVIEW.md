# Review round 2 — the PR/bundle DX layer

Date: 2026-07-13. Scope: the team-workflow layer built since round 1 —
`internal/bundle`, `internal/gitbase`, `internal/prflow`, `internal/workspace`,
the reworked `cmd/onwardpg`, and the docs set — reviewed against PLAN.md as the
spec, with a specific brief: how this behaves for a team where branches land,
bases erode, and bundles must be recreated and re-stacked. Round 1
(2026-07-12, planner-core findings) lives in git history for this file.

Process: 8 finder angles (gitbase, bundle, prflow/workspace, CLI wiring, team
lifecycle walker, staleness/immutability, docs parity, DX critique) plus a git
coupling inventory; 51 candidates; the load-bearing claims below were
independently re-verified in code (several gitbase claims were verified
empirically against real git). `go build` / `go vet` clean.

Round-1 carry-over: the worst round-1 finding (DefaultEquivalent executing
default expressions on the live `--from` database) is fixed by removal — the
CLI no longer wires `graphplan.Options.DefaultEquivalent` at all. The unused
hook remains at `internal/graphplan/plan.go:38`; if it is ever re-wired, the
round-1 constraints (dev database only, read-only, never volatile-by-value)
still apply.

---

## 1. On git coupling (the question you raised)

Your instinct is right, and the code itself is the best evidence. The full
inventory (below) shows the coupling is narrower than it looks, and the
gitbase bug list (§3.2) shows what owning that plumbing costs.

### What git actually provides today

All git execution funnels through two executors (`runGit`,
`gitbase.go:407`; `gitWithEnv`, `prepare.go:241`). No other package runs git.
The derived facts fall into three classes:

1. **Pure provenance labels** — base ref, base/head SHA, relationship string.
   Recorded, not verified. `plan --bundle` *already* treats these as caller
   receipts (`--base-ref/--base-commit/--head-revision`, main.go:263-265), and
   PLAN.md concedes they are "caller receipts rather than independently
   verified provenance." The CLI cannot verify a SHA without trusting the same
   git worktree anyway — half-verification is a false comfort.
2. **Content inputs** — the base tree, the synthetic head tree, and the three
   migration-directory snapshots (merge-base / base / head). Git's only role
   is *producing directories*. Everything downstream — `classify()`
   (gitbase.go:214, pure set logic), migration classification, the `BC == BM`
   schema square (analyze.go:148), planning, bundle policy — is already
   git-free Go over those snapshots.
3. **Genuinely git-semantic facts** — merge-base discovery, base-reachability,
   merge-conflict detection (`merge-tree`). These have no git-free equivalent,
   **but they must be re-derived in CI regardless** (a local check is
   attacker/mistake-bypassable), and CI always has real git.

### Recommendation

Make the core git-free; move snapshot production to the caller.

- `pr status` becomes pure classification over supplied directories:
  `--base-migrations <dir> --merge-base-migrations <dir> --head-migrations
  <dir>` plus verbatim receipt flags. When base == merge-base (the common
  case) the merge-base dir defaults to the base dir.
- `pr regenerate` takes `--base-tree <dir> --head-tree <dir>` plus receipts.
  The core computes: content digest of the head tree actually compiled
  (replacing the fragile patch-based dirty digest), migration classification,
  compile-twice determinism, base replay, `BC == BM`, plan, bundle write.
- A **thin optional wrapper** (a documented shell recipe, a `--git`
  convenience layer, or the GitHub Action) runs the four git commands:
  `rev-parse`, `merge-base`, `git worktree add --detach` for the base tree,
  `merge-tree --write-tree` for the synthetic head (conflict = blocked). In
  GitHub Actions, `refs/pull/N/merge` *is* the synthetic merge — checkout of
  the base SHA and the merge ref replaces all of it.
- The three git-semantic checks (reachability, merge-base, conflict) live in
  `ci check`, where git is guaranteed and the check is authoritative. Locally
  they become advisory wrapper behavior, not core requirements.

What you keep: devs and agents stay git-aware (they choose the base, they
produce the trees — agents are already excellent at this). What you shed: the
entire §3.2 bug class (stderr parsing, user gitconfig sensitivity,
criss-cross merge bases, nested repos, digest instability) becomes someone
else's already-solved problem. What you trade: local pre-flight checks weaken
from "derived" to "attested" — but the merge gate (CI) was always the real
enforcement point, and PLAN.md's freshness taxonomy already assumes CI
recomputation.

One change worth making even if gitbase stays: replace the patch-based dirty
digest (`workingRevision`, gitbase.go:329) with a content digest of the tree
that was actually compiled. It hashes what was used rather than a
format-sensitive diff, and it kills findings §3.2(b) and §3.3(digest framing)
in one move.

---

## 1b. Direction update (2026-07-13): no drizzle-kit adapter

Decision: onwardpg does not integrate with drizzle-kit's migration machinery
(generate, `meta/_journal.json`, snapshots, the runtime `migrate()` journal).
drizzle-kit remains only as the DDL compiler — `drizzle-kit export` turns
schema.ts into declarative DDL, which the already-generic `adapter.Compiler`
consumes. onwardpg's bundles become the migration history.

The code is mostly aligned already: the compiler seam is command-agnostic;
the only kit-migration code is `loadDrizzleHistory`
(`internal/workspace/migrations.go:29-84`), which becomes removable along
with the `adapter = "drizzle"` config branch.

What this dissolves:
- §2.1 (journal edits block every branch) — moot; no kit-generated
  migrations exist in the repo at all.
- §2.7's second half (journal-idx collision detection) — moot; migration
  identity is onwardpg bundle identity, which onwardpg controls and can make
  collision-resistant by construction.
- §3.3 lexical replay ordering — the replayed history becomes
  onwardpg-owned artifacts, so ordering is enforceable by construction
  (naming scheme + manifest order) instead of guessed from foreign runners.
- PLAN.md's two hardest CI rows (runner handoff integrity, runner identity
  safety) shrink to checks over formats onwardpg itself writes; the whole
  `ArtifactWriter` translate-to-ORM-format responsibility drops out for
  Drizzle users.
- The BM corner of the schema square becomes "replay onwardpg bundle
  history" — deterministic and fully under onwardpg's control, rather than
  emulating a foreign runner's semantics.

Application boundary (decided 2026-07-13): **the developer orchestrates
execution; there is no runner and no runner gap.** A real rollout is a
stepped process — expand before merge, migrate/backfill observed over time,
contract only after old connections drain, sometimes in a follow-up PR — so
applying is inherently a human/agent-orchestrated act outside onwardpg.
onwardpg's job is to *expose the steps*: crisp per-phase artifacts with
execution-mode and timing notes, and receipts that make the orchestration
checkable.

Two consequences worth building toward:

- **The catalog is the journal.** Because planning is diff-first, onwardpg
  never needs an applied-migrations ledger to know where an environment
  stands — re-planning against the live database yields the residual diff
  (nothing owed = converged; contract still pending = exactly those
  statements). State-based accounting replaces the bookkeeping a runner
  journal fakes, and drift is first-class instead of invisible. This makes
  "did the dev remember the contract step?" a query, not a convention —
  and it is the safety net that makes manual application acceptable.
- **Two speeds, one tool.** In fast prototyping, an agent keeps the dev DB
  in sync itself as columns/tables are added — mostly expand-only plans,
  few or no contract files; the dev-mode loop should stay that light (no
  bundle ceremony required). The PR/bundle machinery is for the production
  path where phases, receipts, and review matter. The CLI should make the
  cheap loop cheap and reserve the ceremony for the boundary that needs it.

Also: the base-integrity check (`BC == BM`) now compares
export-at-base-commit against replayed onwardpg history — same mechanism,
simpler inputs; the plain-SQL history path in migrations.go becomes the only
path and deserves the ordering fix in §3.3 regardless.

**Two baselines, two receipts (2026-07-13).** The same diff mechanic runs at
two boundaries and they must not be conflated. Inside the PR, the bundle is
planned from the *git base schema* — deterministic, CI-reproducible, owned by
the PR; branch history squashes away because the diff is state-based. Ahead
of merge, planning from the *prod database* is a separate release check, not
a replacement baseline: a bundle planned directly from prod would silently
absorb other PRs' merged-but-undeployed migrations and any prod drift into
this PR's migration. The pre-merge check verifies the composition —
prod + main's pending bundles + this PR's bundle ⇒ head schema — so a
failure attributes cleanly to your bundle, someone's undeployed bundle, or
drift.

**Bundle stacking should be self-ordering.** Replaying main's bundle
directories as BM needs a deterministic order, and directory names won't
provide one. Each manifest already records its baseline schema fingerprint,
so derive the order by chaining: the next bundle is the one whose baseline
fingerprint equals the schema state after replaying the previous bundles. No
central journal file to merge-conflict on; a broken chain exposes a missing
or hand-edited bundle; two bundles claiming the same baseline *is* the
concurrent-PR collision, detected at replay time. "Directories presumably
reflect origin/main" becomes a provable property instead of a convention.

## 2. Workflow-blocking findings (a team hits these in week one)

### 2.1 Drizzle's own journal update blocks every branch — VERIFIED, then MOOTED by §1b
`internal/gitbase/gitbase.go:233` (classify), no exemption anywhere

*Retained as the case study that motivated §1b: integrating a foreign
runner's metadata means every one of its self-edits needs classification.*

`drizzle-kit generate` modifies `migrations/meta/_journal.json` on every run.
That file is in the base tree and under `MigrationPath`, so `classify()` marks
the branch `base_history_modified` and both `pr status` and `pr regenerate`
exit blocked with "branch changes migration history reachable from the PR
base." The flagship flow is unreachable for the flagship adapter. Fix: the
adapter must declare journal/metadata files that are *expected* to be
modified (append-only validation for the journal would keep the safety).

### 2.2 The second command of every workflow dead-ends — VERIFIED
`internal/bundle/write.go:101`

The golden path is regenerate → needs_input → write answers → regenerate. The
second regenerate hits "bundle destination ... already exists; use explicit
draft replacement" — an error that never names `--replace-draft`. Every
first-time user and agent stalls on step 2. Related: every re-run bumps
`Generation` (`NextCoordinates`, write.go:34), so the routine two-pass loop
commits generation 2+ and each idempotent re-run diffs manifest.json. Answer
passes on the same inputs should not consume generations.

### 2.3 Every rebase invalidates every answer — VERIFIED
`internal/protocol/protocol.go:144`, `internal/graphplan/plan.go:131,157`

Answers are bound to the whole-graph (current, desired) fingerprint pair, and
question fingerprints are the same global pair. Any change anywhere — an
unrelated table landing on main — makes `--answers` fail hard with "answer
fingerprints are stale" (generic exit 1), without printing the new
fingerprints or offering a rebind path. On a team merging daily, a rename
decision is re-answered on every rebase, and the incentive is to script
fingerprint rewriting — exactly the guessing PLAN.md forbids. Fix direction:
per-question binding to the *participating objects'* fingerprints (the
drop/create pair for a rename), so unrelated erosion leaves answers valid;
plus a `--rebind-answers` flow that re-asks only invalidated questions and
carries valid decisions forward as receipts.

### 2.4 `pr status` never reads the bundle; no staleness classifier exists — VERIFIED
`cmd/onwardpg/main.go:60+`

PLAN.md's six staleness classes (fresh / git-stale / schema-stale /
decision-stale / artifact-stale / immutable-successor-required) are computed
nowhere. `pr status` reports git classification only and exits 0 "ready" even
when the checked-in bundle was planned against a dead base or its SQL was
hand-edited. The only freshness probe is running full `pr regenerate`, which
*mutates the bundle* to answer a read-only question. This is the single most
valuable missing piece: `pr status` should read the manifest and compare
base_commit, fingerprints, planner options, answers, and digests, and print
the exact remediation command per class.

### 2.5 The execution-receipt lifecycle deadlocks — VERIFIED
`internal/bundle/write.go:19` (NextCoordinates → strict Read), `bundle.go`
manifest shape

The v1 manifest has no fields for execution receipts, verification files,
amendments, or handoff artifacts, and `Read` rejects any file not recorded in
the manifest. So the moment `execution.json` or `verification/*` is written
per PLAN.md's own directory shape, `NextCoordinates` fails with "bundle
contains unexpected or unrecorded files" — *before* `validateReplaceableBundle`
can emit its intended "bundle has an execution receipt and is immutable"
message (write.go:210, unreachable via the CLI). The successor-generation
flow PLAN.md mandates after execution is exactly the flow that breaks, and
the error reads as corruption, inviting `rm -rf` of the receipt. The manifest
needs first-class slots for lifecycle files (or a declared "extension paths"
namespace) before any executor writes a receipt.

### 2.6 Hand-edited SQL locks the bundle with no exit — VERIFIED
`internal/bundle/write.go:219-222`

A tweak to `phases/expand.sql` makes both `Read` and `validateReplaceableBundle`
fail on digest mismatch, so `--replace-draft` can never recover; the only
remediation is manual deletion (losing decision history). PLAN.md promises
"digest failure until imported as a reviewed amendment" — but amendments
don't exist, and the error suggests nothing. Until amendments land, the error
should at least name the recovery (delete + regenerate) explicitly.

### 2.7 Stacked PRs are misdiagnosed after the parent merges — VERIFIED
`internal/gitbase/gitbase.go:228-232`

`base_path_collision` fires on any `A` status whose path exists in the base
tree, without content comparison. Branch B carrying its squash-merged parent's
byte-identical migration files is blocked as a "concurrent identity
collision" — wrong diagnosis, no suggested remediation (rebase). Identical
content should classify as `base_reachable` with a "rebase to drop inherited
files" hint, not a collision.

Conversely (same function): collision detection is exact-path-only. Under
§1b this narrows to onwardpg's own bundle identities — two concurrent PRs
using the same `--bundle` id for one target. That is detectable by
construction (bundle path + manifest identity), which is the argument for
owning the identity scheme rather than inheriting a foreign journal's.

### 2.8 Merge conflicts surface as generic tool failure — VERIFIED
`internal/prflow/analyze.go:96` → `cmd/onwardpg/main.go:169`

`gitbase.MergeConflictError` is typed, carries structured fields, and is
`errors.As`-checked by nobody: it propagates as `pr_analysis_error` exit 1 —
indistinguishable from a planner crash. Agents keying on outcome=blocked/exit
4 will retry instead of reporting "rebase and resolve." (If git moves out of
the core per §1, the wrapper owns this; either way the typed error must not
die generically.)

---

## 3. Integrity and correctness findings

### 3.1 Bundle storage (`internal/bundle`)

- **TOCTOU on replacement** (write.go:103 vs 133-149): `validateReplaceableBundle`
  runs, then the temp build takes seconds, then stat/rename/RemoveAll run with
  no re-check — an `execution.json` written in between is deleted with the
  backup. Re-validate immediately before the rename pair.
- **Crash window resets lineage** (write.go:140-147): a crash between
  rename(dest→backup) and rename(temp→dest) leaves no bundle and an orphan
  `.onwardpg-replaced`. Verified consequence: the *next* regenerate sees no
  manifest, silently restarts at generation 1 (decision history gone), and
  only later replacements hit "backup already exists" with no recovery hint.
  Recovery should be automatic (detect orphan backup, restore or instruct).
- **No fsync** (write.go:129,143): rename can be durable while file contents
  are not; a post-crash bundle can hold truncated SQL that still "installed
  successfully." At minimum fsync phase files and the parent dir before rename.
- **`safeName` accepts dot-only names** (bundle.go:~500, duplicated in
  workspace/config.go:162): bundle ID `..` or `.` passes validation and
  `BundlePath`'s `filepath.Join` collapses it — a bundle can resolve to the
  target directory or the shared bundle root and clobber the namespace.
  `validateRepositoryPath` (config.go:156) similarly accepts bare `..`.
  Reject `.`/`..`/dot-only outright in both copies (or better: one shared
  helper — see §5).
- **Replacement doesn't check identity/generation** (write.go:103): nothing
  verifies the incoming artifact's BundleID/Target match the bundle being
  replaced or that Generation increases; two racing agents both produce
  "generation 2" and the second silently clobbers the first.
- **Redaction is `://`-only** (bundle.go:~480): a libpq keyword DSN
  (`host=... password=...`) in `SourceReceipt.Description` passes validation
  and lands in a committed manifest. Scan for password/credential patterns,
  not just URL schemes.
- **plan.json and phases/*.sql are never cross-checked**: any phase SQL whose
  digest matches the manifest validates, even if it diverges from plan.json's
  batches. `renderPhases` is deterministic — Validate could recompute and
  compare.
- **Digest framing is forgeable** (gitbase.go:342, same pattern in
  workspace/compiler.go:134 and migrations.go:132): name/content boundaries
  are `\x00name\x00` with no length prefix, so crafted content can make two
  different trees serialize identically. Length-prefix every field.

### 3.2 Git plumbing (`internal/gitbase`) — all verified empirically

Evidence for §1 as much as a fix list:

- **`CombinedOutput` everywhere** (gitbase.go:413): benign stderr (e.g.
  "warning: refname 'main' is ambiguous") interleaves into parsed stdout —
  corrupting NUL-separated diff parsing and the dirty-digest bytes
  nondeterministically. Parse stdout only.
- **Dirty digest uses abbreviated blob IDs** (gitbase.go:329: `diff --binary`
  without `--full-index`): auto-scaling abbreviation width means a `git gc`
  can change the digest of an identical tree → false "working tree changed
  after git status" failures, non-reproducible receipts across clones.
- **User gitconfig changes patch format** (prepare.go:173): overlay
  generation doesn't pin `diff.noprefix`/`diff.mnemonicPrefix` (apply side
  does set GIT_CONFIG_NOSYSTEM); with the popular `diff.noprefix=true` the
  overlay mis-applies or falsely conflicts.
- **Criss-cross merge bases hard-error** (gitbase.go:109): a routine
  "update branch" + partial back-merge produces two merge bases; Inspect
  fails "expected exactly one merge base" and the PR flow is unusable.
- **Nested repo bricks Inspect** (gitbase.go:361): an untracked inner git
  clone is listed as `dir/` by `ls-files --others`; the not-a-regular-file
  check then fails every working-tree inspection.
- **ExcludePaths inconsistency** (gitbase.go:148): untracked migration
  listing ignores excludes while the fingerprint and overlay honor them —
  reported change list and prepared tree can disagree.
- **Head/checkout mismatch undetected** (gitbase.go:140): passing
  `--head feature-b` with feature-a checked out attributes the inter-branch
  delta to the working tree; rejected only later with a misleading message.
- **Conflict detection is text-fragile** (prepare.go:119): requires literal
  `CONFLICT ` substring; submodule conflicts word it differently and fall
  through to a generic error.

### 3.3 Workspace and analysis (`internal/workspace`, `internal/prflow`)

- **Migration replay order is byte-wise lexical** (migrations.go:92):
  goose/Flyway-style unpadded numbering replays 10 before 2, so a healthy
  base blocks as `base_integrity_mismatch`, and the wrong order is baked into
  the history digest. Under §1b the plain-SQL path becomes the only path and
  onwardpg controls the naming: enforce a zero-padded/manifest-ordered scheme
  at write time and validate it at read time, rather than trusting sort
  order. (`loadDrizzleHistory` and the `adapter = "drizzle"` branch become
  removable.)
- **The determinism check can't see env nondeterminism** (compiler.go:54):
  `command.Env = os.Environ()` passes the full ambient env and both
  double-compile runs share it — a compiler branching on NODE_ENV or reading
  a live-DB env var self-certifies "deterministic" per machine while
  fingerprints diverge across machines. Run with a minimal allowlisted env.
  Also: the nondeterminism error shows no diff of the two outputs — a
  timestamp header in a schema export becomes an undebuggable hard stop.
- **`equivalentSnapshots` swallows the real error** (analyze.go:200): a
  Fingerprint() failure reports as "schema compiler is nondeterministic",
  sending users to debug the wrong thing.
- **Ignore selectors skipped for typed snapshots** (analyze.go:205): a
  snapshot-returning adapter bypasses ignore application on its side while
  the DDL side applies them — spurious diffs for exactly the excluded
  objects. Latent (no snapshot adapter ships yet) but the seam is public.
- **Config gaps** (config.go:73-99): no check that two targets share a
  migration_path (one target's migration silently shifts the other's base
  fingerprint); schema_file may live inside migration_path (the recursive
  `*.sql` walk then ingests the declarative schema as executed history);
  `pathsOverlap` is case-sensitive on case-insensitive filesystems (macOS).
- **Bundle root is excluded from all protection** (main.go:95,166): a branch
  editing a *merged* bundle's phases/manifest raises no problem and doesn't
  even perturb the dirty digest — merged receipts are rewritable today.
  Base-reachable bundle files deserve the same protection as migrations.

### 3.4 CLI (`cmd/onwardpg`)

- **Mutation before disclosure** (main.go:224): `pr regenerate --replace-draft`
  replaces the draft *then* prints the analysis; PLAN.md requires printing the
  proposed mutation set first. On write failure the computed analysis
  (including questions) is discarded.
- **Planner receipts are incomplete**: `--ignore` selectors are not recorded
  in `PlannerOptions` (bundle.go:34), the migration-history digest prflow
  computes is dropped rather than stored, and `buildVersion` is always "dev"
  (main.go:22, no ldflags anywhere) — so "options changed" and "planner
  changed" staleness are unevaluable even once a classifier exists.
- **Nine bundle flags silently ignored without `--bundle`** (main.go:306):
  `plan --bundle-id ... --intent ... --replace-draft` without `--bundle` exits
  0 having written nothing, no diagnostic.
- **`--config` resolution inconsistent** (main.go:82,140,371): the literal
  default joins the repo root; any other relative value (even
  `./.onwardpg.toml`) resolves against CWD; `config check` always uses CWD.
- **`config check` JSON is versionless** (main.go:380) while docs/protocol.md
  tells consumers to branch on `protocol_version`.
- **`[policy]` is parsed, validated, and consumed by nothing** — VERIFIED
  (config.go:37; zero references elsewhere). `require_one_bundle_per_pr` does
  not prevent a second bundle; `approval_hazards` gates nothing. Inert
  config that looks enforced is worse than absent: either wire it into
  regenerate/ci or delete it until Wave 4.

### 3.5 Docs vs code

- docs/protocol.md:73 — answer-key example uses dot separators
  (`table:public.old_name`); real keys are colon-joined
  (`table:public:old_name`). An agent following the doc gets "unused answers".
- docs/protocol.md:16 — documents always-present arrays; all five fields are
  `omitempty` and vanish when empty.
- docs/cli.md:3 — "The only command today is onwardpg plan" contradicts the
  same file documenting `config check` / `pr status` / `pr regenerate`.
- docs/agent-workflow.md:151 — shows batch markers (`contract-transactional`)
  that the renderer never emits (`batch-001`).
- docs/bundles.md:29 — says 40-char commits required; validation also accepts
  64-char (SHA-256 repos). The code is right; the doc undersells it.

---

## 4. DX observations (opinions, not defects)

- **The freshness oracle is the product.** For a team, the daily question is
  "is my bundle still valid and if not, what one command fixes it?" That is
  `pr status` reading the manifest and printing a staleness class + exact
  remediation (§2.4). It's also the cheapest of the missing pieces — pure
  comparison of receipts already recorded. I'd build it before amendments,
  before `ci check`, before more adapter work: it converts every other
  failure from a surprise into a plan.
- **Answer survival is the second product.** The regeneration story is only
  tolerable if decisions survive base erosion (§2.3). Per-object fingerprint
  binding + a rebind flow that re-asks only what changed turns "rebase =
  re-answer everything" into "rebase = usually nothing."
- **Every error should name the next command.** The codebase's error
  discipline is good about *what* failed and silent about *what to do*. The
  three highest-traffic errors (exists-without-replace-draft, stale answers,
  base_integrity_mismatch) all dead-end today. A `Remediation string` field
  on the diagnostic protocol would institutionalize it.
- **Generation semantics need one decision**: is a generation "a draft that
  survived base erosion" (PLAN.md's story) or "any write"? Today it's any
  write (§2.2), which makes the number meaningless to reviewers. Suggest:
  same inputs → same generation (idempotent rewrite), new attempt within a
  generation for the answer loop, new generation only on base/head change.
- **Cost of the inner loop**: each regenerate is 4 compiler runs + 2 disposable
  DBs + full history replay; base-side work is repeated although the base
  commit is immutable. Cache base compilation and replay by (base_commit,
  target, adapter digest) in an ignored cache dir — this alone probably halves
  the loop.
- **Scaffolding duplication to retire while the packages are young**:
  `safeName` ×2 (config.go:162, bundle.go:~495) with diverging rules vs
  `safeMigrationTag`; `validateRepositoryPath` ×2 (gitbase.go:432,
  config.go:148); two exec wrappers; two tree-hash implementations
  (digestTree, workingRevision); the ~25-line repo/config/target preamble
  duplicated per subcommand — and `dev diff`, `release plan`, `ci check` will
  each need it next.
