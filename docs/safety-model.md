# Safety model

onwardpg is a planner, not a deployment agent. It never executes a plan on a
caller-supplied target. Clone verification is the only execution surface, and
it creates and destroys its own randomly named disposable databases.

The planner's core safety rules are:

- inspect live catalog state in a single `REPEATABLE READ, READ ONLY`
  transaction;
- materialize DDL in a disposable PostgreSQL database rather than parse a
  subset of SQL;
- block unknown or unmodeled catalog state unless a validated narrow ignore
  selector explicitly accounts for it;
- bind ambiguity answers to both source and desired graph fingerprints;
- reject stale, invalid, contradictory, duplicate, and unused answers;
- require an explicit, fingerprint-bound manual-work contract—including its
  transactional/non-transactional execution mode—whenever schema state cannot
  prove the required data work;
- emit no plan when status is `needs_input` or `unsupported`;
- preserve execution constraints through explicit transactional batches; and
- surface destructive, lock, rewrite, validation, and availability concerns as
statement safety/hazard metadata for review.

Manual-work SQL is operator-owned and is never invented from catalog state.
Only a question that explicitly requests manual work may carry that payload;
ordinary answer types reject it. Summaries are one-line metadata. Verification
queries are one-line postconditions that must each return one boolean `true`
row during clone verification. The reviewed statement list is placed in its
own `MANUAL` batch with the declared execution boundary; a failed transactional
postcondition rolls that batch back.

Transactional batches are intended to be atomic execution boundaries. The real
PostgreSQL integration suite includes a failure case that proves an earlier
statement is rolled back when a later statement in the same batch fails.

An ignore selector is acceptance of a blind spot, not a declaration that the
ignored object is equivalent. It is validated against both compared snapshots
and returned in the result's `ignored` field.

The planner cannot prove data validity, safe casts, backfills, lock duration,
or application compatibility. Reviewers own those operational decisions. Test
the generated plan on a clone and require an empty residual diff after it is
applied.
