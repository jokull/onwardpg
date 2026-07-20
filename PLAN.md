# Acceptance-compatible expand/contract planning

## Outcome

Make this promise true for every schema transition onwardpg plans:

> After expand, both the in-flight legacy application and the newly deployed
> application can execute their database operations. The overlap schema may be
> deliberately looser than either endpoint schema. Contract restores the exact
> desired schema only after legacy instances have drained.

This is an acceptance-compatibility promise, not a promise to preserve every
legacy database guarantee during the overlap window. If retaining an old
constraint would reject a new write, expand may weaken or remove that
constraint. Onwardpg must make the temporary enforcement gap explicit and must
prove the desired contract can be restored.

The motivating bug is a same-name CHECK widening:

~~~sql
-- current
CHECK (tier IN ('shift_push', 'assigned_email'))

-- desired
CHECK (tier IN ('slack_new_conversation', 'shift_push', 'assigned_email'))
~~~

Before this hill climb, the generic constraint-mutation renderer rebuilt this
in `contract`. That let newly deployed code fail before contract. The planner
now replaces the constraint in `expand`; legacy values remain valid, the new
value becomes valid, and no contract operation is needed.

## Non-negotiable invariant

Let:

- `Old` be the operations issued by pre-deployment application instances;
- `New` be the operations issued by the newly deployed application; and
- `Accepts(schema, operation)` mean PostgreSQL can execute the operation
  without the transitional schema itself rejecting it.

The expand state must satisfy:

~~~text
for every operation in Old union New:
    Accepts(expand_schema, operation)
~~~

The contract state must satisfy:

~~~text
contract_schema == desired_schema
~~~

The expand state does **not** have to preserve all guarantees of the old
schema. In set terms, its accepted state space may be a superset of both
endpoint schemas:

~~~text
allowed(expand) >= allowed(current) union allowed(desired)
~~~

That distinction keeps the planner honest. Preserving old enforcement and
accepting every new operation are sometimes mutually exclusive. Temporary
weakening is valid; silently rejecting one application version is not.

## Implemented in this hill climb

- CHECK predicates are inspected directly from `pg_get_expr`, with bounded
  finite-set, DNF branch, preserved-NOT-NULL, and finite-to-fixed-regex
  implication proofs.
- Proven CHECK widenings converge in expand; narrowing stays in contract;
  unknown changes use an explicit loose expand envelope and staged contract
  restoration.
- Unambiguous cross-name CHECK families are correlated through typed constrained
  columns. The checkout-quote settlement-currency transition is an executable
  PostgreSQL receipt, not a name guess.
- Same-name unique/primary-key and standalone unique-index relaxations converge
  in expand. Incomparable unique constraints and unknown unique-index
  replacements remove obsolete enforcement in expand and restore desired
  enforcement in contract.
- Exclusion mutations and same-name constraint-kind changes use the same loose
  fallback, with retained catalog dependents blocking instead of being dropped.
- New uniqueness involving a newly added column on an existing table captures
  a verified-clean or manual preparation decision, covering the OTP purge and
  dedupe migration shape.
- Standalone constraint and unique-index removals are expand relaxations with
  enforcement-specific questions and hazards, never generic `data_loss`.
- New existing-table CHECKs and foreign keys use `NOT VALID` plus a separate
  validation batch. Foreign-key mutations relax in expand and restore in
  contract.
- Adding/changing a same-type default is expand; removing one remains contract.
- Legacy/new acceptance and final convergence receipts pass on PostgreSQL
  15–18. The complete graph-planner PostgreSQL 18 suite also passes.

History-only product choreography remains intentionally operator-authored. An
endpoint graph cannot reconstruct the favorite-list ranking backfill, OTP
expiry judgment, or a defensive drop/update/re-add sequence whose endpoint
constraint is unchanged. Those belong in the evolving plan as fingerprinted
manual work and verification, not in guessed SQL.

## Corpus audit: Trip migration history

The implementation fixtures must be grounded in the complete migration corpus
at `~/Code/triptojapan/trip/packages/db/migrations`, including `origin/main` at
`2bba8c1824a7fe687b558d8050fee14315cc31bd`, scanned on 2026-07-20.

Corpus size:

- 69 generated migrations containing 4,998 lines of migration SQL;
- one 124-line production runbook;
- the latest snapshot contains 161 tables, 2,064 columns, 174 CHECK
  constraints, 302 foreign keys, 61 unique constraints, and 487 indexes.

### What the history demonstrates

| Transition family | Evidence in the corpus | Planner lesson |
| --- | --- | --- |
| Same-name CHECK lifecycle | 19 CHECK names appear in both DROP and ADD operations in one migration; 13 change the endpoint predicate and six are defensive/backfill choreography with no endpoint predicate change | Constraint identity is not enough to choose `contract`; compare accepted writes and preserve intentional plan work that is absent from the endpoint diff |
| Literal-set widening | `support_escalation_delivery_tier_check` adds `shift_push`; `20260720094851_support-new-conversation-slack` then adds `slack_new_conversation` | Proven widening belongs wholly in expand |
| Complex predicate widening | `chk_trip_line_item_destination_manual_amount` begins allowing a NULL manual amount when a Dato hotel is present | If widening is not proven, use a looser overlap state instead of incorrectly delaying the change |
| Mixed-looking CHECK evolution | `tt_translation_value_source_field_check` adds Package branches while also adding explicit non-NULL guards, then later adds `co_host_bio` | Reason over acceptance, including SQL three-valued CHECK semantics and other table invariants; fall back safely when implication is unknown |
| Finite set to general predicate | six currency checks move from `IN ('usd', 'eur', 'jpy', 'krw')` to `~ '^[a-z]{3}$'` | A finite old domain can prove a general new predicate is a widening; otherwise the compatibility fallback still works |
| Role widening | three admin relay/impersonation CHECKs add `super_admin` | Separate DROP and ADD statements are the same rollout transition as a combined ALTER TABLE |
| Cross-name constraint evolution | JPY-only currency constraints become supported-currency or mode-dependent constraints | Classify an enforcement family, not only same-ID object mutations |
| Unique-key relaxation | two flight-segment UNIQUE constraints add `direction` to their key | The old key implies the new weaker key; the swap is needed in expand because new writers may duplicate sequence across directions |
| Deliberate uniqueness gap | the presentment-currency runbook drops and recreates `idx_order_trip_addon_open_unique` concurrently and explicitly accepts a brief unenforced interval | A looser overlap schema is operationally realistic; surface and verify the gap rather than pretending it does not exist |
| Nullability polarity | one `DROP NOT NULL`; three `SET NOT NULL` operations after backfills | Relaxation is expand; restriction and validation are contract |
| Temporary defaults | three required columns are added with sentinel defaults and then have those defaults removed | Add the compatibility default in expand; remove it in contract |
| New interface plus enforcement | `trip_template_day_item.transfer_mode` is added with a CHECK accepting both legacy place/tour rows and new transfer rows | The column is required in expand; the CHECK can be contract unless new code explicitly relies on its enforcement during overlap |
| Destructive cleanup | 14 column drops, seven table drops, and five actual index drops | Application-facing removals remain contract, but enforcement-object drops need kind-aware classification because some are required relaxations |

No generated Trip migration changes a column type, renames a column, or evolves
an enum after creation. Those paths still belong in the full engine audit, but
they must not be presented as corpus-derived findings.

### Required corpus fixtures

Check in minimal, self-contained PostgreSQL fixtures derived from these cases:

1. tier literal-set widening, including the checked-in
   `slack_new_conversation` migration;
2. manual-amount complex widening;
3. translation source/field mixed predicate and later branch widening;
4. six-table currency `IN` to regex widening;
5. three admin-role widenings expressed as separate DROP and ADD statements;
6. flight-segment unique-key relaxation;
7. settlement-currency cross-name replacement;
8. NOT NULL relaxation and backfill-then-tighten;
9. add-required-with-temporary-default then drop-default; and
10. concurrent partial-unique replacement with an explicit enforcement gap;
    and
11. additive `transfer_mode` plus a CHECK that admits both application
    versions.

Keep the fixtures small enough that a reviewer can see the rollout property.
Their purpose is not to reproduce the Trip schema.

## Architectural revision: classify before rendering

Phase selection is currently distributed across create, drop, and modify
renderers. That made all structural constraint replacements become contract
operations even when their semantic effect was a relaxation.

Introduce a compatibility-classification pass between semantic diffing and SQL
rendering:

~~~text
current graph + desired graph + retained decisions
                    |
                    v
       transition families and proofs
                    |
                    v
     compatibility-envelope classifier
                    |
          +---------+---------+
          |                   |
          v                   v
       expand              contract
  accepts Old + New     exact desired graph
~~~

Each changed enforcement family receives a typed disposition:

~~~text
expand_to_desired     desired is proven no stricter; no contract needed
expand_noop           current already accepts both; tighten in contract
expand_relax          remove or weaken enforcement; restore in contract
expand_bridge         expose two interfaces or synchronized representations
contract_only         operation cannot affect newly deployed SQL acceptance
needs_decision        application behavior or proof is not inferable
unsupported           onward cannot construct a safe one-deploy path
~~~

Every disposition records:

- the current and desired object IDs or correlated enforcement-family IDs;
- whether acceptance is widened, narrowed, unchanged, or unknown;
- the proof used, such as literal-set inclusion or old unique key implying the
  new unique key;
- the temporary guarantee lost during overlap, if any;
- the verification required immediately before contract; and
- whether newly deployed SQL addresses the object by name or relies on its
  behavior, when that requires a developer decision.

Renderers consume the disposition. They do not independently guess phases.

## Climb 1: lock the invariant with failing execution tests

Before changing rendering, add an integration harness that tests application
acceptance, not only final graph convergence.

Each rollout fixture contains:

~~~text
current.sql       current production schema
desired.sql       declarative target schema
legacy.sql        operations an old instance can issue
next.sql          operations the new instance can issue
verify.sql        boolean assertions required before contract
~~~

The harness must:

1. materialize `current.sql`;
2. generate and apply only expand;
3. run both `legacy.sql` and `next.sql` successfully against the same schema;
4. establish the post-drain/backfill conditions in the fixture;
5. run `verify.sql` and require every assertion to be true;
6. apply contract;
7. prove the catalog graph equals `desired.sql`; and
8. where the desired contract is stricter, prove the retired legacy operation
   would now be rejected.

The first red test is the exact tier widening. It must assert:

- DROP old CHECK and ADD desired CHECK are both in expand;
- they share an atomic replacement boundary;
- both old tier values and `slack_new_conversation` are accepted after expand;
- no contract statement exists for that constraint; and
- final inspection has an empty residual diff.

Run these tests on PostgreSQL 15, 16, 17, and 18.

## Climb 2: make CHECK transitions acceptance-aware

### Model the predicate directly

Extend inspected CHECK constraints with the catalog-deparsed expression from:

~~~sql
pg_get_expr(con.conbin, con.conrelid, true)
~~~

Keep the complete `pg_get_constraintdef` representation for faithful DDL, but
do not scrape the CHECK expression out of that presentation string. Both real
and materialized desired schemas pass through the same PostgreSQL inspector, so
the structured expression is catalog-grounded.

### Safe proof ladder

Classify CHECK changes with a deliberately bounded proof ladder:

1. exact semantic equality after PostgreSQL normalization;
2. single-column `IN` / `= ANY(array)` literal-set inclusion, including NULL
   acceptance;
3. finite old literal domain evaluated exhaustively against the desired
   predicate in the disposable PostgreSQL database;
4. simple normalized bound/nullability forms with tested implication rules;
5. otherwise, unknown.

Do not make arbitrary SQL predicate implication a correctness dependency.

### Rendering rules

For a validated current CHECK `P` and desired CHECK `Q`:

- **Q proven at least as permissive as P:** atomically replace P with Q in
  expand. Add Q as `NOT VALID`, then validate it in a separate expand batch so
  the strong catalog lock is brief. Since P proved every existing row satisfies
  Q, this does not invent a data assumption. No contract step remains.
- **Q proven stricter than P:** retain P through expand. After legacy drain,
  add Q under a deterministic temporary name as `NOT VALID` so all subsequent
  writes are fenced by the desired predicate. Backfill and verify existing rows,
  validate Q, then atomically drop P and rename Q to the durable identity.
- **Mixed or unknown:** remove P in expand, explicitly creating a looser
  overlap schema. After legacy drain, add Q as `NOT VALID` to fence subsequent
  writes, capture any product-specific backfill, run a generated `Q IS FALSE`
  assertion, and validate Q in contract.

The generic fallback is removal, not automatic `P OR Q` synthesis. Dropping a
CHECK reliably stops that CHECK from rejecting either writer. Boolean
composition can still throw for partial expressions or user functions because
PostgreSQL does not promise source-order evaluation. A future optimization may
retain `P OR Q` only when total, side-effect-free evaluation is proven.

Preserve comments, validation state, `NO INHERIT`, partition propagation, typed
dependencies, and stable constraint identity. Block or request manual work when
partition inheritance or a non-total function prevents a proven automatic
path.

### Contract proof

Generate the CHECK assertion with PostgreSQL CHECK semantics:

~~~sql
SELECT NOT EXISTS (
  SELECT 1
  FROM "public"."support_escalation_delivery"
  WHERE (<desired predicate>) IS FALSE
);
~~~

`IS FALSE`, rather than `NOT (<predicate>)`, correctly treats NULL as accepted
by a CHECK constraint. Run the assertion only after the desired `NOT VALID`
constraint is installed. PostgreSQL then enforces Q for new or updated rows
while the assertion and backfill cover pre-existing rows, closing the race
between a successful scan and constraint installation.

## Climb 3: handle UNIQUE, exclusion, and index enforcement

Constraint and index changes need the same acceptance classifier, but their
proofs are structural rather than Boolean.

### Proven unique relaxation

When the old unique key implies the new key—for example:

~~~sql
UNIQUE (package_component_id, sequence)
-- becomes
UNIQUE (package_component_id, direction, sequence)
~~~

build the desired backing index concurrently under a reserved temporary name
while the old constraint still proves it can succeed. Swap to the weaker
constraint in expand. The new application can then use duplicate sequence
numbers across directions. Contract is empty.

The proof must account for:

- key order and expressions;
- partial-index predicate implication;
- `NULLS DISTINCT` versus `NULLS NOT DISTINCT`;
- included columns and opclasses;
- collations;
- exclusion operators; and
- dependent foreign keys and replica identity.

Only claim implication for explicitly tested structural forms.

### Tightening or unknown replacement

- Keep compatible old enforcement during expand when it does not reject new
  operations.
- If old enforcement may reject new operations, remove it in expand and mark
  the exact temporary guarantee gap.
- Build/attach the desired unique or exclusion enforcement in contract after a
  duplicate/conflict assertion and any captured cleanup.
- Use concurrent builds and transactional attachment/swap boundaries where
  PostgreSQL permits them.

Generated pre-contract verification must return duplicate keys or exclusion
conflicts in a bounded, reviewable query.

### Application-visible uniqueness

New application SQL may use `ON CONFLICT`, name a constraint, or assume a
database-enforced idempotency key. Schema diff alone cannot infer that.

Ask one focused decision when a new/tightened unique object cannot coexist with
legacy writes:

> Does the newly deployed application address or require this uniqueness before
> contract?

If yes and no acceptance-compatible database shape exists, fail closed with a
two-deployment or application-bridge requirement. Do not emit a plan that lets
new SQL fail. If no, the constraint can be restored in contract.

Replace generic `data_loss` hazards on enforcement drops with accurate hazards
such as `temporary_uniqueness_unenforced`, `constraint_relaxation`, and
`duplicate_rows_possible`.

## Climb 4: correlate cross-name enforcement families

Object identity alone misses transitions such as:

~~~sql
DROP CONSTRAINT checkout_quote_settlement_currency_jpy;
ADD CONSTRAINT checkout_quote_settlement_currency_by_mode CHECK (...);
~~~

Build correlation candidates within a table from typed dependency evidence:

- constraint kind;
- constrained columns;
- referenced table/columns for foreign keys;
- backing index keys and predicates;
- dependent objects; and
- explicit rename decisions already captured by the planner.

Correlation does not silently declare a rename. It groups related enforcement
changes so the classifier can construct one rollout envelope. Ambiguous
many-to-many candidates become a concise decision, with drop/add remaining the
safe semantic fallback.

Retained plan decisions are important here. Six Trip CHECK names appear in
defensive DROP/re-add choreography around a data rewrite even though their
endpoint predicates do not change. That sequence is not visible in endpoint
graphs. When an edited onward plan captures equivalent work, rebasing a
declarative chain must retain it under its stable decision ID until its
preconditions change or the developer removes it.

## Climb 5: audit every enforcement and interface polarity

Replace ad hoc phase choices with a checked-in transition matrix. Every modeled
mutation must be classified or explicitly unsupported.

| Feature | Relaxing direction | Restricting/removing direction | Required audit |
| --- | --- | --- | --- |
| CHECK | wider predicate or drop: expand | narrower predicate: contract | Implemented with bounded proofs and a loose unknown fallback |
| NOT NULL | DROP NOT NULL: expand | SET NOT NULL: backfill/verify/contract | Preserve existing good behavior; move staged proof creation only if it cannot reject legacy writes |
| FOREIGN KEY | drop old enforcement in expand | add or restore desired enforcement in contract | Implemented for mutation; surface temporary orphan/cascade behavior |
| UNIQUE / exclusion | weaker or incomparable old enforcement: expand relaxation | stronger or desired enforcement: contract | Implemented for bounded unique proofs and loose incomparable fallbacks |
| Defaults | adding or changing a same-type default: expand | removing a default: contract | The expand schema has one desired default; old explicit values and omission syntax remain accepted |
| Column add | nullable or compatibility-default form: expand | final required shape: contract | Preserve nullable shadow/backfill flow |
| Column drop | n/a | contract | Confirm no renderer can move it earlier through dependency grouping |
| Column rename | overlap bridge: expand | old-name removal/native identity cleanup: contract | Re-run all existing rename strategies through acceptance probes |
| Column type | old/new interface bridge: expand | old representation removal: contract | Direct ALTER TYPE is allowed only with an explicit proof that both deployed versions accept the one physical type |
| Enum | add accepted label: expand | label removal/reorder/rewrite: bridge then contract | Audit enum replacement paths against old and new value probes |
| Index | additive lookup: expand; uniqueness follows enforcement rules | application-facing removal: contract, enforcement relaxation may be expand | Distinguish performance interface from integrity enforcement |
| Table/view/routine | additive interface: expand | removal/signature contraction: contract | Exercise old and new queries, including dependent views/routines |
| Trigger/generated/identity | case-specific | case-specific | Require an explicit write-path compatibility disposition; do not classify from DDL direction alone |
| RLS/policies/privileges | granting/relaxing may be expand | revoking/tightening contract | Acceptance includes authorization; never use the looser-schema rule to hide an unreviewed security expansion |

Add a test that fails when a modeled create/drop/modify renderer lacks a matrix
entry. Security changes retain their stricter authorization confirmation even
when application acceptance would otherwise suggest expansion.

## Climb 6: make evolving plans preserve compatibility decisions

Compatibility decisions live in the single plan tied to the merge and deploy.
They must survive normal schema edits without becoming stale authority.

Key decisions by an enforcement-family identity plus current/desired
fingerprints. On re-plan:

1. preserve edited backfills, verification, and application-interface answers
   when their exact preconditions are unchanged;
2. re-run compatibility proofs whenever either endpoint predicate or dependent
   column shape changes;
3. invalidate phase/proof decisions when their evidence changes;
4. show the developer why an item moved between expand and contract;
5. never silently discard history-only choreography merely because the final
   endpoint object compares equal; and
6. detect dev/prod/history drift before applying a retained decision.

Add rebase tests where:

- another branch adds one more allowed tier;
- a backfill edit survives an unrelated column addition;
- a predicate edit invalidates a previous widening proof;
- production gains rows that fail the pending desired contract; and
- an unchanged endpoint CHECK still retains an explicitly captured temporary
  drop/backfill/re-add sequence.

## Climb 7: operational rendering and hazards

Rendered plans must make the temporary contract obvious:

~~~sql
-- EXPAND COMPATIBILITY ENVELOPE
-- Both legacy and new application writes are accepted.
-- Temporary guarantee removed: support_escalation_delivery_tier_check.
-- Restore condition: desired predicate has no FALSE rows after legacy drain.
~~~

Use separate batches for:

- brief transactional catalog swaps;
- `CREATE/DROP INDEX CONCURRENTLY` operations;
- lower-lock validation scans;
- operator-edited data movement; and
- final contract attachment/cleanup.

Attach accurate timeouts and retry cleanup. A failed concurrent build must be
detectable as an invalid index and safe to resume, matching the Trip production
runbook's operational lesson.

Hazard vocabulary should distinguish:

- temporary enforcement weakening;
- possible overlap-window invalid rows;
- pre-contract data cleanup;
- new application dependency not yet available;
- strong catalog lock;
- validation scan;
- concurrent-build retry residue; and
- permanent destructive cleanup.

## Climb 8: documentation and proof receipts

Update the README and plan-command guide around the corrected promise:

1. expand is an acceptance-compatible envelope, often intentionally looser;
2. deploy old and new code together against that envelope;
3. drain legacy code;
4. backfill and run generated assertions; and
5. contract to the exact declarative target.

Use the tier widening as the easy example, the unique-key relaxation as the
medium example, the cross-name currency constraint as the hard example, and a
type/derived-object bridge as the nightmare example.

For every published SQL sample, generate the plan from a checked-in fixture and
execute the legacy, next, verify, and convergence receipts in CI. Documentation
must not hand-author phase claims the planner does not produce.

Explain temporary weakening plainly. Do not imply old business invariants are
preserved while new code starts using values those invariants rejected.

## Evidence gates

The work is complete only when all of these are true:

- the exact `slack_new_conversation` widening is expand-only;
- every corpus-derived fixture passes legacy and new probes after expand;
- every contract fixture proves its assertion before desired enforcement is
  attached;
- final graph convergence is empty on PostgreSQL 15–18;
- CHECK, NOT NULL, FK, unique, exclusion, default, index, type, rename, enum,
  generated, identity, trigger, RLS, policy, and privilege transitions have a
  checked-in disposition or a fail-closed blocker;
- no constraint drop is generically labeled `data_loss` without a more precise
  object-specific reason;
- re-planning preserves valid edited work and invalidates stale proofs;
- generated SQL clearly identifies temporary enforcement gaps;
- README examples are generated from executable fixtures; and
- a context-blind PostgreSQL specialist can infer the acceptance-compatibility
  promise from the README, inspect the tests, and find no path where newly
  deployed or legacy SQL is silently rejected before contract.

## Non-goals

- Preserve every old database enforcement guarantee during the overlap window.
- Infer arbitrary application queries or business invariants from DDL.
- Solve implication for arbitrary PostgreSQL expressions.
- Automatically invent product-specific backfills.
- Pretend every schema change fits one rolling deployment; fail closed when no
  acceptance-compatible bridge exists.

## Implementation order

1. Add the legacy/new execution harness and the exact red CHECK fixture.
2. Introduce typed transition dispositions and remove phase authority from
   individual renderers.
3. Implement CHECK expression inspection, bounded proofs, relaxed fallback,
   assertions, and corpus fixtures.
4. Implement unique/exclusion implication, concurrent expand swaps, contract
   verification, and application-visible uniqueness decisions.
5. Correlate cross-name enforcement families and preserve history-only edited
   choreography.
6. Audit FK, default, NOT NULL, index, and destructive-drop polarity.
7. Run type, rename, enum, generated, identity, trigger, view/routine, RLS,
   policy, and privilege paths through the transition matrix.
8. Add plan-rebase invalidation/preservation tests.
9. Regenerate documentation examples from the executable fixture corpus.
10. Run PostgreSQL 15–18 convergence and acceptance CI, then request a fresh
    blind PostgreSQL review.
