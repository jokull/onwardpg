# Phase classification

Every generated operation is placed on one side of exactly one application
deployment. This inventory records why former middle-phase work belongs in
expand, contract, an editable two-sided handoff, or a separate plan.

| Operation family | Classification | Required invariant | Evidence |
| --- | --- | --- | --- |
| New schemas, enums, tables, nullable columns, extensions, routines, triggers, ordinary indexes | Expand | Existing code does not lose an interface; hazards still describe locks, scans, and index builds | `TestCreatePlanConvergesOnPostgreSQL`, `TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL` |
| New constraint on a new table | Expand | The table is itself not yet used by old code | `TestCreatePlanConvergesOnPostgreSQL` |
| New constraint or unique index on an existing table | Contract | Old writers have drained or are known to satisfy the new invariant | `TestMutationPlanConvergesOnPostgreSQL`, `TestNotValidForeignKeyCreateConvergesOnPostgreSQL` |
| Existing `NOT VALID` constraint validation | Expand | PostgreSQL already enforces the constraint for new rows; validation scans historical rows only | `TestNotValidConstraintValidationConvergesOnPostgreSQL`, `TestBuildValidatesNotValidConstraintWithoutRebuild` |
| New required column without a default | Expand nullable column; contract backfill pocket and `SET NOT NULL` | Old writers can omit the column before deployment; new code writes it; old writers drain before enforcement | `TestBuildStagesNewRequiredColumnWithoutDefault` |
| Nullable column becoming `NOT NULL` | Contract | Old writers that may produce nulls have drained; backfill assertions are true | `TestBuildStagesApplicationBackfillBeforeNotNullContract` |
| Drop `NOT NULL` | Expand | Relaxing the invariant does not break old code | `TestMutationPlanConvergesOnPostgreSQL` |
| Same-type column rename | Expand shadow column and trigger; explicit manual, single-transaction, or split-plan backfill strategy; contract assertion and native rename | Both names remain writable; divergent dual writes fail; equality is verified and pre-deployment writers drain before bridge removal | `TestBuildRequiresFingerprintBoundColumnRenameAnswer`, `TestColumnRenameOverlapBridgeConvergesOnPostgreSQL`, `TestColumnRenameWithExistingTriggerIsRejectedOnPostgreSQL` |
| Type or semantic conversion | Editable expand and contract pockets, or explicit split-plan blocker | Product code proves one deployed version works with both representations; onwardpg never invents conversion or precedence | `TestBuildRequiresAndRendersColumnMutationChoices`, `TestSemanticManualSQLHandoffRequiresEditedCloneVerificationOnPostgreSQL` |
| Supported table rename | Expand new-name updatable view; contract atomic view removal and physical rename | Tested view shape supports required old/new reads and writes; old callers drain before removal | `TestTableRenameConvergesOnPostgreSQL` |
| View, routine, trigger, policy, RLS, and privilege behavior changes | Contract after confirmation where required; explicit compatibility blocker for unbridged application identities | Pre-deployment callers no longer require old behavior and the one new release works before and after the change | `TestRoutineReplacementRequiresMaterializedViewRefreshContract`, `TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL`, `TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL` |
| Partition attach/detach or persistence change | Contract | Pre-deployment writers have drained; scans and access-exclusive locks are reviewed | `TestBuildRendersPartitionAttachDetachAndRejectsReconfiguration`, `TestPartitionAttachAndDetachConvergeOnPostgreSQL` |
| Product-specific partition reconfiguration | Contract edit pocket | Agent supplies exact data movement and verifies bounds; planner does not invent detach/attach ordering | `TestBuildRequiresFingerprintBoundManualContractForPartitionReconfiguration` |
| Materialized-view rebuild or refresh | Contract | Stale stored data and blocking refresh/rebuild behavior are explicitly accepted | `TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL`, `TestRoutineReplacementRequiresMaterializedViewRefreshContract` |
| Ordinary index replacement | Expand same-name replacement build; contract old-index cleanup | Replacement is usable before old index is removed; concurrent statements remain outside transactions | `TestBuildContinuouslyReplacesSameNameIndex`, `TestContinuousMaterializedViewAndLocalPartitionIndexReplacementConvergesOnPostgreSQL` |
| Constraint-backed index replacement | Expand replacement index; contract brief constraint swap | New index is ready before the enforced identity changes | `TestContinuousPrimaryAndUniqueConstraintReplacementConvergesOnPostgreSQL` |
| Sequence `OWNED BY`, identity, serial, default, generated-expression, and sequence-parameter changes | Contract unless creating a wholly new object | Old code has drained before generation or cascade behavior changes; destructive identity removal is confirmed | `TestSequenceOwnedByTransitionsConvergeOnPostgreSQL`, `TestIdentityAddAllOptionsAndConfirmedDropConvergeOnPostgreSQL` |
| Drops and approved destructive cleanup | Contract | Destructive intent is fingerprint-bound and old code no longer uses the object | `TestApprovedDestructiveDropsConvergeOnPostgreSQL` |

Manual work is not a phase. A manual statement belongs in expand or contract
according to its timing. Transactional mode is orthogonal: either file may
contain multiple transactional and non-transactional batches.

Where the catalog cannot prove the invariant in this table, onwardpg must ask,
emit an editable pocket, or return unsupported. It must not relabel a direct
cutover as contract and call it a one-deployment strategy.
