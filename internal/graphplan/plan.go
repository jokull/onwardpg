// Package graphplan renders forward-only PostgreSQL plans from typed graphs.
package graphplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

type Options struct {
	ConcurrentIndexes bool
	IfNotExists       bool
	IfExists          bool
	CascadeDrops      bool
	// IgnoreExtensionVersions suppresses version-only changes for the named
	// extensions. Names which are not present are intentionally harmless: this
	// option mirrors PostgreSQL schema-diff tooling where one shared allowlist
	// can be used across databases with different extension sets.
	IgnoreExtensionVersions []string
	// SchemaQualifier scopes a plan to one existing schema and controls how
	// schema-qualified names are rendered. A nil value keeps catalog schema
	// names. A pointer to "" emits unqualified names; a non-empty value replaces
	// the one in-scope catalog schema in emitted SQL.
	//
	// Schema creation, deletion, and modification are deliberately forbidden in
	// this mode: a qualified plan is intended for a connection/search_path that
	// already selects its target schema.
	SchemaQualifier *string
	// UnsortedDump preserves UnsortedOrder rather than dependency-sorting
	// changes. It is for preordered schema dumps, never the default migration
	// workflow.
	UnsortedDump  bool
	UnsortedOrder []pgschema.ID
	// DefaultEquivalent is an optional PostgreSQL-backed semantic comparator
	// for non-identical column default expressions.
	DefaultEquivalent func(current, desired string) (bool, error)
	// PreserveSurplus turns the diff into a development-workspace plan. Objects
	// present only in current are retained and reported rather than interpreted
	// as permission to contract the caller-owned database. Exact clone
	// verification and durable history planning keep this false.
	PreserveSurplus bool
	// DirectColumnRenames is set only for caller-owned development-catalog
	// reconciliation. A confirmed rename becomes one immediate ALTER TABLE
	// instead of the rolling-deployment bridge required by durable bundles.
	DirectColumnRenames bool
	// IdentityHints are explicit, validated assertions supplied by the caller
	// before rename candidacy. They are never inferred from a schema diff.
	IdentityHints []protocol.Hint
}

type decisions struct {
	typeUsing       map[pgschema.ID]string
	notNullStrategy map[pgschema.ID]string
	notNullBackfill map[pgschema.ID]protocol.ManualWork
	matViewRebuild  map[pgschema.ID]bool
	matViewRefresh  map[pgschema.ID]protocol.ManualWork
	partitionManual map[pgschema.ID]protocol.ManualWork
	identityDrop    map[pgschema.ID]bool
	enumRewrite     map[pgschema.ID]bool
	authorization   map[pgschema.ID]bool
	renameBridge    map[pgschema.ID]protocol.ManualWork
}

type renameIndex struct {
	from pgschema.Index
	to   pgschema.Index
}

type renameTable struct {
	from                     pgschema.Table
	to                       pgschema.Table
	compatibilityColumns     []pgschema.Column
	compatibilityPrivileges  []pgschema.TablePrivilege
	derivedChildRenames      []tableChildRename
	notNullConstraintRenames []notNullConstraintRename
	manualOnly               bool
}

// tableChildRename is a PostgreSQL-generated child name which needs an
// explicit catalog rename after the physical table rename. PostgreSQL retains
// constraint and index names when a relation is renamed; declarative exporters
// commonly regenerate their conventional names instead.
type tableChildRename struct {
	from pgschema.Object
	to   pgschema.Object
}

type notNullConstraintRename struct {
	from string
	to   string
}

type renameColumn struct {
	from pgschema.Column
	to   pgschema.Column
}

type renameEnum struct {
	from pgschema.Enum
	to   pgschema.Enum
}

type renameConstraint struct {
	from        pgschema.Constraint
	to          pgschema.Constraint
	fromIndexes []pgschema.Index
	toIndexes   []pgschema.Index
}

type renameView struct {
	from          pgschema.View
	to            pgschema.View
	dependents    []viewRewrite
	indexComments []indexCommentRewrite
}

type viewRewrite struct {
	before pgschema.View
	after  pgschema.View
}

type indexCommentRewrite struct {
	before pgschema.Index
	after  pgschema.Index
}

type renameRoutine struct {
	from     pgschema.Routine
	to       pgschema.Routine
	triggers []triggerRewrite
}

type triggerRewrite struct {
	before pgschema.Trigger
	after  pgschema.Trigger
}

type renameTrigger struct {
	from pgschema.Trigger
	to   pgschema.Trigger
}

type moveExtension struct {
	from pgschema.Extension
	to   pgschema.Extension
}

func Build(current, desired *pgschema.Snapshot, answers protocol.Answers, options Options) (protocol.Result, error) {
	if options.UnsortedDump {
		if len(current.IDs()) != 0 {
			return protocol.Result{}, fmt.Errorf("unsorted dump requires an empty current snapshot")
		}
		if err := pgschema.ValidateObjectOrder(desired, options.UnsortedOrder); err != nil {
			return protocol.Result{}, fmt.Errorf("invalid unsorted dump order: %w", err)
		}
	}
	currentFingerprint, err := Fingerprint(current, options)
	if err != nil {
		return protocol.Result{}, err
	}
	desiredFingerprint, err := Fingerprint(desired, options)
	if err != nil {
		return protocol.Result{}, err
	}
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		Ignored: unionStrings(current.Ignored(), desired.Ignored()),
	}
	if answers.ProtocolVersion == "" && answers.CurrentFingerprint == "" && answers.DesiredFingerprint == "" && len(answers.Answers) == 0 {
		answers.ProtocolVersion, answers.CurrentFingerprint, answers.DesiredFingerprint = protocol.Version, currentFingerprint, desiredFingerprint
	}
	resolver, err := answers.Resolver(currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if unsupported := unionStrings(current.Unsupported(), desired.Unsupported()); len(unsupported) > 0 {
		return unsupportedResult(result, resolver, unsupported)
	}

	changes := change.Between(current, desired)
	result.Compatibility = append(result.Compatibility, ignoredExtensionVersionEvidence(changes, options.IgnoreExtensionVersions)...)
	if options.UnsortedDump {
		for _, item := range changes {
			if item.Kind != change.Create {
				return protocol.Result{}, fmt.Errorf("unsorted dump only supports create-only schema snapshots")
			}
		}
	}
	if err := validateSchemaQualifier(current, desired, changes, options.SchemaQualifier); err != nil {
		return protocol.Result{}, err
	}
	changes, err = filterEquivalentDefaults(changes, options.DefaultEquivalent)
	if err != nil {
		return protocol.Result{}, err
	}
	changes, constraintRenames, constraintRenameQuestions, err := resolveConstraintRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(constraintRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, constraintRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, tableRenames, tableRenameQuestions, tableRenameUnsupported, tableRenameAnalysis, err := resolveTableRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint, options.PreserveSurplus, options.IdentityHints)
	if err != nil {
		return protocol.Result{}, err
	}
	result.Analysis = append(result.Analysis, tableRenameAnalysis...)
	if len(tableRenameUnsupported) > 0 {
		return unsupportedResult(result, resolver, tableRenameUnsupported)
	}
	if len(tableRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, tableRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, viewRenames, viewRenameQuestions, err := resolveViewRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(viewRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, viewRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, routineRenames, routineRenameQuestions, err := resolveRoutineRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(routineRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, routineRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, triggerRenames, triggerRenameQuestions, err := resolveTriggerRenames(changes, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(triggerRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, triggerRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, enumRenames, enumRenameQuestions, err := resolveEnumRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(enumRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, enumRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, extensionMoves := resolveExtensionMoves(changes)
	changes, columnRenames, columnRenameQuestions, columnRenameUnsupported, err := resolveColumnRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint, options.DirectColumnRenames, options.PreserveSurplus)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(columnRenameUnsupported) > 0 {
		return unsupportedResult(result, resolver, columnRenameUnsupported)
	}
	if len(columnRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, columnRenameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	changes, indexRenames, renameQuestions, err := resolveIndexRenames(changes, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(renameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, renameQuestions
		scopeQuestions(result.Questions, current, desired)
		return result, nil
	}
	if options.PreserveSurplus {
		changes, result.Preserved = preserveSurplus(changes)
	}
	if unreachable := unreachableColumnPhysicalOrder(current, desired, tableRenames, columnRenames); len(unreachable) > 0 {
		result.Compatibility = append(result.Compatibility, unreachable...)
		sort.Strings(result.Compatibility)
	}
	droppingSchemas, droppingTables := droppingParents(changes)
	questions, approvedDrops, err := destructiveQuestions(changes, droppingSchemas, droppingTables, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	mutationQuestions, choices, err := mutationQuestions(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	questions = append(questions, mutationQuestions...)
	bridgeQuestions, err := resolveRenameBridges(tableRenames, columnRenames, viewRenames, routineRenames, resolver, currentFingerprint, desiredFingerprint, &choices)
	if err != nil {
		return protocol.Result{}, err
	}
	questions = append(questions, bridgeQuestions...)
	if len(questions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, questions
		scopeQuestions(result.Questions, current, desired)
		result.Guidance = partitionReconfigurationGuidance(changes, current, desired, options)
		return result, nil
	}
	result.Guidance = append(splitPlanGuidance(changes, choices), partitionReconfigurationGuidance(changes, current, desired, options)...)
	// These statements must precede same-phase work that refers to the desired
	// routine identity. Trigger definitions are reapplied after a confirmed
	// routine rename, never before the new routine name exists.
	for _, rename := range routineRenames {
		if work, manual := choices.renameBridge[rename.from.ObjectID()]; manual {
			appendStatement(&result, manualWorkStatement(work, "rename_compatibility_bridge", "application_contract"))
			continue
		}
		for _, statement := range renderRoutineRename(rename) {
			appendStatement(&result, statement)
		}
		for _, rewrite := range rename.triggers {
			for _, statement := range renderDependentTriggerRewrite(rewrite) {
				appendStatement(&result, statement)
			}
		}
	}
	for _, rename := range triggerRenames {
		for _, statement := range renderTriggerRename(rename) {
			appendStatement(&result, statement)
		}
	}
	for _, rename := range constraintRenames {
		for _, statement := range renderConstraintRename(rename) {
			appendStatement(&result, statement)
		}
	}

	createdTables := make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind == change.Create && item.ID.Kind == pgschema.KindTable {
			createdTables[item.ID] = true
		}
	}
	detachedIndexes := detachedConstraintIndexes(changes)
	deferredIndexCreates := make([]change.Change, 0)
	deferredPostConstraintStatements := make([]protocol.Statement, 0)
	consumed := make(map[pgschema.ID]bool)
	coordinatedConstraintChanges := coordinatedConstraintChangeIDs(changes, current, desired, options)
	coordinatedDerivedChanges := coordinatedColumnDerivedChangeIDs(changes, current, desired, choices)
	var dynamicUnsupported []string
	scheduledChanges := change.Schedule(current, desired, changes)
	if options.UnsortedDump {
		scheduledChanges = []change.Batch{{Changes: orderUnsortedChanges(changes, options.UnsortedOrder)}}
	}
	for _, scheduled := range scheduledChanges {
		if scheduled.Cyclic {
			return protocol.Result{}, fmt.Errorf("unresolved dependency cycle in scheduled graph changes")
		}
		for _, item := range scheduled.Changes {
			if consumed[item.ID] {
				continue
			}
			if coordinatedConstraintChanges[item.ID] || coordinatedDerivedChanges[item.ID] {
				continue
			}
			if implicitConstraintIndexDrop(item, changes) || propagatedPartitionChildDrop(item, changes) || propagatedPartitionChildModify(item, changes) || propagatedPartitionChildRebuild(item, changes) {
				continue
			}
			if item.Kind == change.Drop {
				if coveredByParent(item.ID, droppingSchemas, droppingTables) && !foreignKeyMustDropBeforeReferencedTable(item, droppingTables) {
					continue
				}
				if !approvedDrops[item.ID] {
					continue
				}
			}
			if isIndexNamespaceCollisionCreate(item, changes) {
				deferredIndexCreates = append(deferredIndexCreates, item)
				continue
			}
			statements, consumedIDs, unsupported, err := renderChange(item, current, desired, createdTables, options, choices)
			if err != nil {
				return protocol.Result{}, err
			}
			for _, id := range consumedIDs {
				consumed[id] = true
			}
			dynamicUnsupported = append(dynamicUnsupported, unsupported...)
			for _, statement := range statements {
				if stringIn(statement.Hazards, "after_primary_key_drop") {
					deferredPostConstraintStatements = append(deferredPostConstraintStatements, statement)
				} else {
					appendStatement(&result, statement)
				}
			}
		}
	}
	if len(dynamicUnsupported) > 0 {
		return unsupportedResult(result, resolver, dynamicUnsupported)
	}
	for _, statement := range deferredPostConstraintStatements {
		appendStatement(&result, statement)
	}
	// PostgreSQL indexes share a schema-level relation namespace. A same-named
	// index moved to another table must be dropped before its replacement is
	// created; defer the create into the contract phase after scheduled drops.
	for _, item := range deferredIndexCreates {
		statements, _, unsupported, err := renderChange(item, current, desired, createdTables, options, choices)
		if err != nil {
			return protocol.Result{}, err
		}
		if len(unsupported) > 0 {
			return unsupportedResult(result, resolver, unsupported)
		}
		for _, statement := range withPhase(statements, "contract", "review", "index_name_collision") {
			appendStatement(&result, statement)
		}
	}
	// Dropping a primary/unique constraint drops its backing index. Recreate a
	// desired standalone index only after that contract step has run.
	for _, index := range detachedIndexes {
		statements, _, unsupported, err := renderCreate(index, desired, nil, options)
		if err != nil {
			return protocol.Result{}, err
		}
		if len(unsupported) > 0 {
			return unsupportedResult(result, resolver, unsupported)
		}
		for _, statement := range withPhase(statements, "contract", "review", "constraint_index_detach") {
			appendStatement(&result, statement)
		}
	}
	// A CREATE OR REPLACE VIEW can leave stored rows in a dependent
	// materialized view stale even though the materialized-view definition has
	// not changed. The graph proves the dependency, but it cannot tell us
	// whether REFRESH CONCURRENTLY is available, what lock window is acceptable,
	// or which data postcondition the operator requires. Emit only the reviewed,
	// fingerprint-bound contract supplied for that work.
	for _, id := range sortedManualWorkIDs(choices.matViewRefresh) {
		appendStatement(&result, manualWorkStatement(choices.matViewRefresh[id], "materialized_view_refresh", "stale_materialized_view_data", "data_movement", "blocking_lock_possible"))
	}
	if err := resolver.ValidateAllUsed(); err != nil {
		return protocol.Result{}, err
	}
	for _, rename := range indexRenames {
		for _, statement := range renderIndexRename(rename) {
			appendStatement(&result, statement)
		}
	}
	for _, rename := range tableRenames {
		if rename.manualOnly {
			appendStatement(&result, manualWorkStatement(choices.renameBridge[rename.from.ObjectID()], "rename_compatibility_bridge", "application_contract"))
			continue
		}
		statements, err := renderTableRename(rename)
		if err != nil {
			return protocol.Result{}, err
		}
		for _, statement := range statements {
			appendStatement(&result, statement)
		}
	}
	for _, rename := range viewRenames {
		if work, manual := choices.renameBridge[rename.from.ObjectID()]; manual {
			appendStatement(&result, manualWorkStatement(work, "rename_compatibility_bridge", "application_contract"))
			continue
		}
		for _, statement := range renderViewRename(rename) {
			appendStatement(&result, statement)
		}
		for _, rewrite := range rename.dependents {
			for _, statement := range renderDependentViewRewrite(rewrite) {
				appendStatement(&result, statement)
			}
		}
		for _, rewrite := range rename.indexComments {
			for _, statement := range renderIndexCommentRewrite(rewrite) {
				appendStatement(&result, statement)
			}
		}
	}
	for _, rename := range enumRenames {
		for _, statement := range renderEnumRename(rename) {
			appendStatement(&result, statement)
		}
	}
	for _, move := range extensionMoves {
		for _, statement := range renderExtensionMove(move) {
			appendStatement(&result, statement)
		}
	}
	for _, rename := range columnRenames {
		if work, manual := choices.renameBridge[rename.from.ObjectID()]; manual {
			appendStatement(&result, manualWorkStatement(work, "rename_compatibility_bridge", "application_contract"))
			continue
		}
		if options.DirectColumnRenames {
			for _, statement := range renderDirectColumnRename(rename) {
				appendStatement(&result, statement)
			}
			continue
		}
		for _, statement := range renderColumnRename(rename) {
			appendStatement(&result, statement)
		}
	}
	if options.SchemaQualifier != nil {
		applySchemaQualifier(&result, schemaNamesForChanges(current, desired, changes), *options.SchemaQualifier)
	}
	if options.UnsortedDump {
		markUnsortedDump(&result)
		assignStatementIDs(result.Statements)
		if err := rebuildUnsortedBatches(&result); err != nil {
			return protocol.Result{}, err
		}
	} else {
		assignStatementIDs(result.Statements)
		if err := rebuildBatches(&result); err != nil {
			return protocol.Result{}, err
		}
	}
	if unsupported := expandContractViolations(result.Statements); len(unsupported) > 0 {
		result.Status = protocol.Unsupported
		result.Unsupported = unsupported
		result.Statements = nil
		result.Batches = nil
		return result, nil
	}
	result.Status = protocol.Planned
	for _, item := range result.Statements {
		if strings.Contains(item.SQL, "ONWARDPG TODO") {
			result.Status = protocol.NeedsSQLEdits
			break
		}
	}
	return result, nil
}

// Equivalent reports whether two snapshots differ only in semantics explicitly
// excluded by planner options. It is intentionally narrower than a generic
// ignore mechanism: extension existence, schema, comments, and dependencies
// remain part of convergence; only the Version field of a same-name extension
// present on both sides can be normalized.
func Equivalent(current, desired *pgschema.Snapshot, options Options) (bool, error) {
	leftFingerprint, err := Fingerprint(current, options)
	if err != nil {
		return false, err
	}
	rightFingerprint, err := Fingerprint(desired, options)
	if err != nil {
		return false, err
	}
	return leftFingerprint == rightFingerprint, nil
}

// Fingerprint returns the semantic fingerprint used by plans and receipts.
// Selected extension versions are normalized, while every other extension
// attribute and graph edge remains identity-bearing.
func Fingerprint(snapshot *pgschema.Snapshot, options Options) (string, error) {
	selected := make(map[string]bool, len(options.IgnoreExtensionVersions))
	for _, name := range options.IgnoreExtensionVersions {
		selected[name] = true
	}
	normalized, err := snapshotWithNormalizedExtensionVersions(snapshot, selected)
	if err != nil {
		return "", err
	}
	return normalized.Fingerprint()
}

func snapshotWithNormalizedExtensionVersions(source *pgschema.Snapshot, names map[string]bool) (*pgschema.Snapshot, error) {
	result := pgschema.New()
	for _, object := range source.Objects() {
		if extension, ok := object.(pgschema.Extension); ok && names[extension.Name] {
			extension.Version = ""
			object = extension
		}
		if err := result.Add(object); err != nil {
			return nil, err
		}
	}
	for _, id := range source.IDs() {
		for _, dependency := range source.Dependencies(id) {
			if err := result.AddDependency(id, dependency); err != nil {
				return nil, err
			}
		}
	}
	for _, selector := range source.Unsupported() {
		if err := result.AddUnsupported(selector); err != nil {
			return nil, err
		}
	}
	for _, selector := range source.Ignored() {
		if err := result.AddIgnored(selector); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func ignoredExtensionVersionEvidence(changes []change.Change, names []string) []string {
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	var evidence []string
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindExtension {
			continue
		}
		before, beforeOK := item.Before.(pgschema.Extension)
		after, afterOK := item.After.(pgschema.Extension)
		if beforeOK && afterOK && before.Version != after.Version && selected[after.Name] {
			evidence = append(evidence, "extension_version_ignored:"+after.Name)
		}
	}
	sort.Strings(evidence)
	return evidence
}

func preserveSurplus(changes []change.Change) ([]change.Change, []string) {
	kept := make([]change.Change, 0, len(changes))
	preserved := make([]string, 0)
	for _, item := range changes {
		if item.Kind == change.Drop {
			preserved = append(preserved, item.ID.String())
			continue
		}
		kept = append(kept, item)
	}
	sort.Strings(preserved)
	return kept, preserved
}

// unreachableColumnPhysicalOrder identifies declarative column positions that
// PostgreSQL cannot reach with ALTER TABLE. The planner keeps these as visible
// compatibility evidence, while semantic equality and convergence ignore the
// ordinal: replacing a table merely to match source-file layout would be a
// disproportionate and dangerous migration.
func unreachableColumnPhysicalOrder(current, desired *pgschema.Snapshot, tableRenames []renameTable, columnRenames []renameColumn) []string {
	tableTargets := make(map[pgschema.ID]pgschema.ID)
	for _, object := range current.Objects() {
		table, ok := object.(pgschema.Table)
		if !ok {
			continue
		}
		if _, exists := desired.Object(table.ObjectID()); exists {
			tableTargets[table.ObjectID()] = table.ObjectID()
		}
	}
	for _, rename := range tableRenames {
		tableTargets[rename.from.ObjectID()] = rename.to.ObjectID()
	}

	columnTargets := make(map[pgschema.ID]pgschema.ID)
	for _, rename := range columnRenames {
		columnTargets[rename.from.ObjectID()] = rename.to.ObjectID()
	}

	var unsupported []string
	for currentTable, desiredTable := range tableTargets {
		currentColumns := tableColumns(current, currentTable)
		desiredColumns := tableColumns(desired, desiredTable)
		if len(currentColumns) == 0 || len(desiredColumns) == 0 {
			continue
		}
		positionsKnown := true
		for _, column := range append(append([]pgschema.Column(nil), currentColumns...), desiredColumns...) {
			if column.Position <= 0 {
				positionsKnown = false
				break
			}
		}
		if !positionsKnown {
			continue
		}
		desiredByID := make(map[pgschema.ID]pgschema.Column, len(desiredColumns))
		for _, column := range desiredColumns {
			desiredByID[column.ObjectID()] = column
		}
		matchedDesired := make(map[pgschema.ID]bool, len(currentColumns))
		var currentRetained []pgschema.ID
		for _, before := range currentColumns {
			targetID, renamed := columnTargets[before.ObjectID()]
			if !renamed {
				targetID = pgschema.ID{Kind: pgschema.KindColumn, Schema: desiredTable.Schema, Name: desiredTable.Name, Part: before.Name}
			}
			if _, exists := desiredByID[targetID]; !exists {
				continue
			}
			matchedDesired[targetID] = true
			currentRetained = append(currentRetained, targetID)
		}
		if len(currentRetained) == 0 {
			continue
		}
		var desiredRetained []pgschema.ID
		lastRetainedPosition := 0
		for _, after := range desiredColumns {
			if matchedDesired[after.ObjectID()] {
				desiredRetained = append(desiredRetained, after.ObjectID())
				lastRetainedPosition = after.Position
			}
		}
		desiredOrdinal := make(map[pgschema.ID]int, len(desiredRetained))
		for index, id := range desiredRetained {
			desiredOrdinal[id] = index + 1
		}
		for index, id := range currentRetained {
			if index < len(desiredRetained) && desiredRetained[index] == id {
				continue
			}
			after := desiredByID[id]
			unsupported = append(unsupported, fmt.Sprintf(
				"column_physical_order:%s.%s:retained_column:%s:current_order=%d:desired_order=%d",
				desiredTable.Schema, desiredTable.Name, after.Name, index+1, desiredOrdinal[id],
			))
		}
		for _, after := range desiredColumns {
			if matchedDesired[after.ObjectID()] || after.Position > lastRetainedPosition {
				continue
			}
			unsupported = append(unsupported, fmt.Sprintf(
				"column_physical_order:%s.%s:inserted_column:%s:desired=%d:last_retained=%d",
				desiredTable.Schema, desiredTable.Name, after.Name, after.Position, lastRetainedPosition,
			))
		}
	}
	return unionStrings(unsupported, nil)
}

func unsupportedResult(result protocol.Result, resolver *protocol.Resolver, unsupported []string) (protocol.Result, error) {
	if err := resolver.ValidateAllUsed(); err != nil {
		return protocol.Result{}, err
	}
	result.Status = protocol.Unsupported
	result.Unsupported = unionStrings(unsupported, nil)
	result.Statements, result.Batches = nil, nil
	return result, nil
}

type scopedQuestionObject struct {
	ID           string          `json:"id"`
	Definition   json.RawMessage `json:"definition"`
	Dependencies []string        `json:"dependencies,omitempty"`
}

func scopeQuestions(questions []protocol.Question, current, desired *pgschema.Snapshot) {
	for index := range questions {
		questions[index].ScopeFingerprint = questionScopeFingerprint(questions[index], current, desired)
	}
}

func questionScopeFingerprint(question protocol.Question, current, desired *pgschema.Snapshot) string {
	currentExcluded, desiredExcluded := renameTableScopeExclusions(question, current, desired)
	document := struct {
		Kind           string                 `json:"kind"`
		Key            string                 `json:"key"`
		Choices        []string               `json:"choices"`
		AllowsFreeform bool                   `json:"allows_freeform"`
		Current        []scopedQuestionObject `json:"current"`
		Desired        []scopedQuestionObject `json:"desired"`
	}{
		Kind: question.Kind, Key: question.Key, Choices: append([]string(nil), question.Choices...),
		AllowsFreeform: question.AllowsFreeform,
		Current:        questionScopeObjects(current, question, currentExcluded),
		Desired:        questionScopeObjects(desired, question, desiredExcluded),
	}
	if len(document.Current) == 0 && len(document.Desired) == 0 {
		return ""
	}
	data, err := json.Marshal(document)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func questionScopeObjects(snapshot *pgschema.Snapshot, question protocol.Question, excluded map[pgschema.ID]bool) []scopedQuestionObject {
	selectedNames := map[string]bool{question.Key: true}
	for _, choice := range question.Choices {
		selectedNames[choice] = true
	}
	selected := make(map[pgschema.ID]bool)
	ids := snapshot.IDs()
	for _, id := range ids {
		if selectedNames[id.String()] && !excluded[id] {
			selected[id] = true
		}
	}

	// First collect the complete dependent closure of the participating
	// objects. Then include its dependency closure without walking back out
	// through shared parents such as schemas; an unrelated table in the same
	// schema must not invalidate an otherwise identical decision.
	queue := make([]pgschema.ID, 0, len(selected))
	for id := range selected {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		dependency := queue[0]
		queue = queue[1:]
		for _, candidate := range ids {
			if selected[candidate] || excluded[candidate] || !containsID(snapshot.Dependencies(candidate), dependency) {
				continue
			}
			selected[candidate] = true
			queue = append(queue, candidate)
		}
	}
	queue = queue[:0]
	for id := range selected {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dependency := range snapshot.Dependencies(id) {
			if selected[dependency] || excluded[dependency] {
				continue
			}
			selected[dependency] = true
			queue = append(queue, dependency)
		}
	}

	objects := make([]scopedQuestionObject, 0, len(selected))
	for _, id := range ids {
		if !selected[id] {
			continue
		}
		object, _ := snapshot.Object(id)
		definition, err := json.Marshal(object)
		if err != nil {
			definition = []byte("null")
		}
		dependencies := snapshot.Dependencies(id)
		dependencyNames := make([]string, 0, len(dependencies))
		for _, dependency := range dependencies {
			if selected[dependency] {
				dependencyNames = append(dependencyNames, dependency.String())
			}
		}
		objects = append(objects, scopedQuestionObject{ID: id.String(), Definition: definition, Dependencies: dependencyNames})
	}
	return objects
}

// A new child on the desired side is an independent structural edit, not
// evidence that an otherwise credible table identity changed. Excluding only
// unpaired direct children keeps a rename receipt stable while a feature adds
// a column; modifications to children present on both sides still invalidate
// the receipt.
func renameTableScopeExclusions(question protocol.Question, current, desired *pgschema.Snapshot) (map[pgschema.ID]bool, map[pgschema.ID]bool) {
	currentExcluded, desiredExcluded := make(map[pgschema.ID]bool), make(map[pgschema.ID]bool)
	// With one candidate plus "create", new-only columns can be excluded from
	// the rename receipt. Multiple candidates must retain their full closures
	// because each candidate can differ independently.
	if question.Kind != "rename_table" || len(question.Choices) != 2 {
		return currentExcluded, desiredExcluded
	}
	fromObject, fromExists := objectByString(current, question.Key)
	toObject, toExists := objectByString(desired, question.Choices[0])
	from, fromOK := fromObject.(pgschema.Table)
	to, toOK := toObject.(pgschema.Table)
	if !fromExists || !toExists || !fromOK || !toOK {
		return currentExcluded, desiredExcluded
	}
	fromChildren, toChildren := tableChildren(current, from.ObjectID()), tableChildren(desired, to.ObjectID())
	for key, child := range fromChildren {
		if _, exists := toChildren[key]; !exists {
			currentExcluded[child.ObjectID()] = true
		}
	}
	for key, child := range toChildren {
		if _, exists := fromChildren[key]; !exists {
			desiredExcluded[child.ObjectID()] = true
		}
	}
	return currentExcluded, desiredExcluded
}

func objectByString(snapshot *pgschema.Snapshot, value string) (pgschema.Object, bool) {
	for _, id := range snapshot.IDs() {
		if id.String() == value {
			return snapshot.Object(id)
		}
	}
	return nil, false
}

func containsID(ids []pgschema.ID, wanted pgschema.ID) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

func assignStatementIDs(statements []protocol.Statement) {
	seen := make(map[string]int, len(statements))
	for i := range statements {
		base := protocol.StableStatementID(statements[i])
		seen[base]++
		statements[i].ID = base
		if seen[base] > 1 {
			statements[i].ID = fmt.Sprintf("%s-%d", base, seen[base])
		}
	}
}

// markUnsortedDump makes the non-executable nature of caller-selected order
// explicit in every statement. A dependency-invalid source order can be
// useful for a reviewable schema dump, but must never masquerade as a normal
// forward migration plan.
func markUnsortedDump(result *protocol.Result) {
	for i := range result.Statements {
		result.Statements[i].Safety = "manual"
		result.Statements[i].Hazards = unionStrings(result.Statements[i].Hazards, []string{"unsorted_dump_order"})
	}
}

func orderUnsortedChanges(changes []change.Change, order []pgschema.ID) []change.Change {
	byID := make(map[pgschema.ID]change.Change, len(changes))
	for _, item := range changes {
		byID[item.ID] = item
	}
	ordered := make([]change.Change, 0, len(changes))
	seen := make(map[pgschema.ID]bool, len(changes))
	for _, id := range order {
		if item, exists := byID[id]; exists {
			ordered, seen[id] = append(ordered, item), true
		}
	}
	for _, item := range changes {
		if !seen[item.ID] {
			ordered = append(ordered, item)
		}
	}
	return ordered
}

// validateSchemaQualifier implements the bounded equivalent of Atlas's
// schema-scoped planning guard. Rendering every relation into another schema
// is only sound when the mutation and all of its typed dependencies are
// confined to one schema. Schema DDL itself cannot be made safe by a name
// qualifier and is rejected.
func validateSchemaQualifier(current, desired *pgschema.Snapshot, changes []change.Change, qualifier *string) error {
	if qualifier == nil {
		return nil
	}
	for _, item := range changes {
		if item.ID.Kind == pgschema.KindSchema {
			return fmt.Errorf("schema-scoped planning does not allow schema changes (%s)", item.ID.String())
		}
	}
	names := schemaNamesForChanges(current, desired, changes)
	if len(names) > 1 {
		ordered := make([]string, 0, len(names))
		for name := range names {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		return fmt.Errorf("found %d schemas when migration plan is scoped to one: %q", len(ordered), ordered)
	}
	return nil
}

// schemaNamesForChanges returns the schema names of changed graph nodes and
// their transitive typed dependencies. Following graph edges (rather than
// parsing SQL definitions) catches cross-schema enums, referenced tables, and
// sequences before a qualifier could accidentally redirect them.
func schemaNamesForChanges(current, desired *pgschema.Snapshot, changes []change.Change) map[string]bool {
	names := make(map[string]bool)
	seen := make(map[pgschema.ID]bool)
	var visit func(*pgschema.Snapshot, pgschema.ID)
	visit = func(snapshot *pgschema.Snapshot, id pgschema.ID) {
		key := id
		// A node can occur in both snapshots. Include its dependencies from both;
		// marking before recursion is only an optimization, not a semantic choice.
		if seen[key] {
			return
		}
		seen[key] = true
		if id.Schema != "" {
			names[id.Schema] = true
		}
		for _, dependency := range snapshot.Dependencies(id) {
			visit(snapshot, dependency)
		}
	}
	for _, item := range changes {
		// Do not use a shared seen map across snapshots here: an identical ID can
		// have a desired-only dependency (for example, a newly referenced type).
		visit(current, item.ID)
		seen = make(map[pgschema.ID]bool)
		visit(desired, item.ID)
		seen = make(map[pgschema.ID]bool)
	}
	return names
}

// applySchemaQualifier rewrites only SQL identifier tokens, never arbitrary
// comment/default string literals. renderers intentionally quote object names,
// which lets this small lexer replace qualified identifiers without trying to
// parse PostgreSQL expressions. Bare schema prefixes are handled as well for
// catalog type names such as app.status.
func applySchemaQualifier(result *protocol.Result, schemas map[string]bool, qualifier string) {
	for i := range result.Statements {
		result.Statements[i].SQL = qualifySQL(result.Statements[i].SQL, schemas, qualifier)
	}
}

func qualifySQL(sql string, schemas map[string]bool, qualifier string) string {
	var out strings.Builder
	for i := 0; i < len(sql); {
		if sql[i] == '\'' {
			start := i
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					i++
					if i < len(sql) && sql[i] == '\'' { // escaped quote
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(sql[start:i])
			continue
		}
		if sql[i] == '"' {
			name, end, ok := readQuotedIdentifier(sql, i)
			if ok && end < len(sql) && sql[end] == '.' {
				if _, next, nextOK := readQuotedIdentifier(sql, end+1); nextOK && schemas[name] {
					if qualifier != "" {
						out.WriteString(quote(qualifier))
						out.WriteByte('.')
					}
					out.WriteString(sql[end+1 : next])
					i = next
					continue
				}
			}
		}
		// Catalog formatted types commonly use an unquoted schema prefix. This
		// branch is intentionally restricted to identifier boundaries.
		if isIdentStart(sql[i]) {
			start := i
			i++
			for i < len(sql) && isIdentPart(sql[i]) {
				i++
			}
			name := sql[start:i]
			if schemas[name] && i < len(sql) && sql[i] == '.' && i+1 < len(sql) && (isIdentStart(sql[i+1]) || sql[i+1] == '"') {
				if qualifier != "" {
					out.WriteString(quote(qualifier))
					out.WriteByte('.')
				}
				// Do not consume the object identifier; it may be quoted.
				i++
				continue
			}
			out.WriteString(sql[start:i])
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

func readQuotedIdentifier(sql string, start int) (string, int, bool) {
	if start >= len(sql) || sql[start] != '"' {
		return "", start, false
	}
	var name strings.Builder
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != '"' {
			name.WriteByte(sql[i])
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '"' {
			name.WriteByte('"')
			i++
			continue
		}
		return name.String(), i + 1, true
	}
	return "", start, false
}

func isIdentStart(b byte) bool { return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }
func isIdentPart(b byte) bool  { return isIdentStart(b) || b >= '0' && b <= '9' || b == '$' }

func isIndexNamespaceCollisionCreate(item change.Change, changes []change.Change) bool {
	if item.Kind != change.Create || item.ID.Kind != pgschema.KindIndex {
		return false
	}
	created, ok := item.After.(pgschema.Index)
	if !ok || created.Constraint != "" {
		return false
	}
	for _, other := range changes {
		if other.Kind != change.Drop || other.ID.Kind != pgschema.KindIndex {
			continue
		}
		dropped, ok := other.Before.(pgschema.Index)
		if ok && dropped.Constraint == "" && dropped.Table.Schema == created.Table.Schema && dropped.Name == created.Name && dropped.Table != created.Table {
			return true
		}
	}
	return false
}

func filterEquivalentDefaults(changes []change.Change, equivalent func(current, desired string) (bool, error)) ([]change.Change, error) {
	if equivalent == nil {
		return changes, nil
	}
	filtered := make([]change.Change, 0, len(changes))
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindColumn {
			filtered = append(filtered, item)
			continue
		}
		before, after := item.Before.(pgschema.Column), item.After.(pgschema.Column)
		if before.Default == nil || after.Default == nil || *before.Default == *after.Default {
			filtered = append(filtered, item)
			continue
		}
		withoutDefault := before
		withoutDefault.Default = after.Default
		if !reflect.DeepEqual(withoutDefault, after) {
			filtered = append(filtered, item)
			continue
		}
		same, err := equivalent(*before.Default, *after.Default)
		if err != nil {
			return nil, fmt.Errorf("compare defaults for %s: %w", item.ID, err)
		}
		if !same {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func resolveConstraintRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameConstraint, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindConstraint {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		} else if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameConstraint
	var questions []protocol.Question
	for _, drop := range drops {
		before := drop.Before.(pgschema.Constraint)
		if before.Parent != nil || constraintTableIsPartitioned(current, before.Table) {
			continue
		}
		var candidates []change.Change
		for _, create := range creates {
			after := create.After.(pgschema.Constraint)
			if after.Parent == nil && !constraintTableIsPartitioned(desired, after.Table) && equivalentConstraintForRename(before, after) {
				if _, _, ok := constraintRenameIndexes(current, desired, before, after); ok {
					candidates = append(candidates, create)
				}
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		after := candidate.After.(pgschema.Constraint)
		question := protocol.Question{
			ID: "rename_constraint:" + drop.ID.String(), Kind: "rename_constraint", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer != candidate.ID.String() {
			continue
		}
		fromIndexes, toIndexes, ok := constraintRenameIndexes(current, desired, before, after)
		if !ok {
			continue
		}
		consumed[drop.ID], consumed[candidate.ID] = true, true
		for i := range fromIndexes {
			consumed[fromIndexes[i].ObjectID()] = true
			consumed[toIndexes[i].ObjectID()] = true
		}
		renames = append(renames, renameConstraint{from: before, to: after, fromIndexes: fromIndexes, toIndexes: toIndexes})
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

func equivalentConstraintForRename(before, after pgschema.Constraint) bool {
	if before.Table != after.Table {
		return false
	}
	if before.UsingIndex == before.Name && after.UsingIndex == after.Name {
		before.UsingIndex, after.UsingIndex = "", ""
	}
	before.Name, after.Name = "", ""
	before.Comment, after.Comment = nil, nil
	return reflect.DeepEqual(before, after)
}

func constraintTableIsPartitioned(snapshot *pgschema.Snapshot, id pgschema.ID) bool {
	object, exists := snapshot.Object(id)
	table, ok := object.(pgschema.Table)
	return exists && ok && table.Partition != nil
}

func constraintRenameIndexes(current, desired *pgschema.Snapshot, before, after pgschema.Constraint) ([]pgschema.Index, []pgschema.Index, bool) {
	var fromIndexes, toIndexes []pgschema.Index
	for _, object := range current.Objects() {
		index, ok := object.(pgschema.Index)
		if ok && index.Table == before.Table && index.Constraint == before.Name {
			fromIndexes = append(fromIndexes, index)
		}
	}
	for _, object := range desired.Objects() {
		index, ok := object.(pgschema.Index)
		if ok && index.Table == after.Table && index.Constraint == after.Name {
			toIndexes = append(toIndexes, index)
		}
	}
	if len(fromIndexes) != len(toIndexes) || len(fromIndexes) > 1 {
		return nil, nil, false
	}
	if len(fromIndexes) == 0 {
		return fromIndexes, toIndexes, true
	}
	from, to := fromIndexes[0], toIndexes[0]
	from.Name, to.Name = "", ""
	from.Constraint, to.Constraint = "", ""
	from.Comment, to.Comment = nil, nil
	from.Definition, to.Definition = "", ""
	return fromIndexes, toIndexes, reflect.DeepEqual(from, to)
}

func renderConstraintRename(rename renameConstraint) []protocol.Statement {
	identifier := qualified(rename.from.Table.Schema, rename.from.Table.Name)
	statements := []protocol.Statement{statement("ALTER TABLE "+identifier+" RENAME CONSTRAINT "+quote(rename.from.Name)+" TO "+quote(rename.to.Name)+";", "contract", "review", true, "constraint_identity", "brief_lock")}
	commentIdentifier := quote(rename.to.Name) + " ON " + identifier
	statements = append(statements, commentModificationPhase("CONSTRAINT", commentIdentifier, rename.from.Comment, rename.to.Comment, "contract")...)
	for i := range rename.fromIndexes {
		to := rename.toIndexes[i]
		statements = append(statements, commentModificationPhase("INDEX", qualified(to.Table.Schema, to.Name), rename.fromIndexes[i].Comment, to.Comment, "contract")...)
	}
	return statements
}

func detachedConstraintIndexes(changes []change.Change) []pgschema.Index {
	var indexes []pgschema.Index
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindIndex {
			continue
		}
		before, after := item.Before.(pgschema.Index), item.After.(pgschema.Index)
		if before.Constraint != "" && after.Constraint == "" {
			indexes = append(indexes, after)
		}
	}
	return indexes
}

func resolveEnumRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameEnum, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindEnum {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameEnum
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Enum)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Enum)
			if from.Schema == to.Schema && reflect.DeepEqual(from.Labels, to.Labels) && enumDependentsEquivalent(current, desired, from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		to := candidate.After.(pgschema.Enum)
		question := protocol.Question{
			ID: "rename_enum:" + drop.ID.String(), Kind: "rename_enum", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			for _, item := range changes {
				if item.Kind != change.Modify || item.ID.Kind != pgschema.KindColumn {
					continue
				}
				before, after := item.Before.(pgschema.Column), item.After.(pgschema.Column)
				if equivalentColumnForEnumRename(before, after, from, to) {
					consumed[item.ID] = true
				}
			}
			renames = append(renames, renameEnum{from: from, to: to})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

func enumDependentsEquivalent(current, desired *pgschema.Snapshot, from, to pgschema.Enum) bool {
	for _, object := range current.Objects() {
		column, ok := object.(pgschema.Column)
		if !ok || !containsEnumReference(column, from) {
			continue
		}
		nextObject, exists := desired.Object(column.ObjectID())
		if !exists {
			return false
		}
		next, ok := nextObject.(pgschema.Column)
		if !ok || !equivalentColumnForEnumRename(column, next, from, to) {
			return false
		}
	}
	return true
}

func containsEnumReference(column pgschema.Column, enum pgschema.Enum) bool {
	needle := enum.Schema + "." + enum.Name
	quoted := qualified(enum.Schema, enum.Name)
	return strings.Contains(column.Type, needle) || strings.Contains(column.Type, quoted) || (column.Default != nil && (strings.Contains(*column.Default, needle) || strings.Contains(*column.Default, quoted)))
}

func equivalentColumnForEnumRename(before, after pgschema.Column, from, to pgschema.Enum) bool {
	before.Type = normalizeEnumReference(before.Type, from, to)
	if before.Default != nil {
		value := normalizeEnumReference(*before.Default, from, to)
		before.Default = &value
	}
	return reflect.DeepEqual(before, after)
}

func normalizeEnumReference(value string, from, to pgschema.Enum) string {
	for _, pair := range [][2]string{
		{qualified(from.Schema, from.Name), qualified(to.Schema, to.Name)},
		{from.Schema + "." + from.Name, to.Schema + "." + to.Name},
	} {
		value = strings.ReplaceAll(value, pair[0], pair[1])
	}
	return value
}

func renderEnumRename(rename renameEnum) []protocol.Statement {
	sql := "ALTER TYPE " + qualified(rename.from.Schema, rename.from.Name) + " RENAME TO " + quote(rename.to.Name) + ";"
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "application_contract")}
	return append(statements, commentModificationPhase("TYPE", qualified(rename.to.Schema, rename.to.Name), rename.from.Comment, rename.to.Comment, "contract")...)
}

// resolveExtensionMoves is deterministic: PostgreSQL extension names are
// database-global, so a same-named extension cannot independently exist in
// two schemas. A same-version drop/create pair is therefore a schema move,
// not destructive intent.
func resolveExtensionMoves(changes []change.Change) ([]change.Change, []moveExtension) {
	creates := make(map[string]change.Change)
	for _, item := range changes {
		if item.Kind != change.Create || item.ID.Kind != pgschema.KindExtension {
			continue
		}
		extension := item.After.(pgschema.Extension)
		if _, exists := creates[extension.Name]; exists {
			creates[extension.Name] = change.Change{}
			continue
		}
		creates[extension.Name] = item
	}
	consumed := make(map[pgschema.ID]bool)
	var moves []moveExtension
	for _, item := range changes {
		if item.Kind != change.Drop || item.ID.Kind != pgschema.KindExtension {
			continue
		}
		from := item.Before.(pgschema.Extension)
		candidate, exists := creates[from.Name]
		if !exists || candidate.ID.Kind == "" {
			continue
		}
		to := candidate.After.(pgschema.Extension)
		if from.Schema == to.Schema || from.Version != to.Version {
			continue
		}
		consumed[item.ID], consumed[candidate.ID] = true, true
		moves = append(moves, moveExtension{from: from, to: to})
	}
	if len(consumed) == 0 {
		return changes, moves
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, moves
}

func renderExtensionMove(move moveExtension) []protocol.Statement {
	sql := "ALTER EXTENSION " + quote(move.from.Name) + " SET SCHEMA " + quote(move.to.Schema) + ";"
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "extension_schema_move")}
	return append(statements, commentModificationPhase("EXTENSION", quote(move.to.Name), move.from.Comment, move.to.Comment, "contract")...)
}

func resolveColumnRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string, direct, preserveSurplus bool) ([]change.Change, []renameColumn, []protocol.Question, []string, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindColumn {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameColumn
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Column)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Column)
			if from.Table == to.Table && equivalentColumnForRename(from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID.String() < candidates[j].ID.String() })
		choices := make([]string, 0, len(candidates)+1)
		for _, candidate := range candidates {
			choices = append(choices, candidate.ID.String())
		}
		fallback := "create"
		if preserveSurplus {
			fallback = "preserve"
		}
		choices = append(choices, fallback)
		question := protocol.Question{
			ID: "rename_column:" + drop.ID.String(), Kind: "rename_column", Key: drop.ID.String(),
			Message:            "Which desired column, if any, is the new identity of " + drop.ID.String() + "?",
			Choices:            choices,
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer != "create" && answer != "preserve" {
			var candidate change.Change
			for _, possible := range candidates {
				if possible.ID.String() == answer {
					candidate = possible
					break
				}
			}
			if candidate.ID.Kind == "" {
				return nil, nil, nil, nil, fmt.Errorf("resolved column rename candidate %q is unavailable", answer)
			}
			if consumed[candidate.ID] {
				return nil, nil, nil, nil, fmt.Errorf("desired column %s was selected as more than one rename target", candidate.ID)
			}
			to := candidate.After.(pgschema.Column)
			if reason := columnTriggerBridgeUnsupported(current, desired, from, to); !direct && reason != "" {
				return nil, nil, nil, []string{"single_deployment_column_bridge_required:" + drop.ID.String() + ":" + reason}, nil
			}
			consumed[drop.ID], consumed[candidate.ID] = true, true
			// PostgreSQL rewrites catalog references to a renamed column.  Keep
			// only modifications which are exactly that automatic rewrite out of
			// the plan; a real change to a dependent object must still be planned.
			// The dependency edge is the authority for deciding which objects are
			// eligible.  In particular, never infer dependency from a substring in
			// an expression or generated SQL.
			for _, object := range current.Objects() {
				if !dependsOn(current, object.ObjectID(), from.ObjectID()) {
					continue
				}
				afterObject, exists := desired.Object(object.ObjectID())
				if view, isView := object.(pgschema.View); isView {
					if exists && equivalentAutomaticColumnReferenceAfterRename(view, afterObject, from, to) {
						consumed[view.ObjectID()] = true
						continue
					}
					// The database may have rewritten this dependent view, but a
					// non-trivial desired definition could also be an explicit API
					// change. Do not render CREATE OR REPLACE before the column
					// rename or guess a destructive rebuild sequence.
					return nil, nil, nil, []string{"column_rename_dependent_view:" + view.ObjectID().String()}, nil
				}
				if !exists || !equivalentAutomaticColumnReferenceAfterRename(object, afterObject, from, to) {
					continue
				}
				consumed[object.ObjectID()] = true
			}
			renames = append(renames, renameColumn{from: from, to: to})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil, nil
}

func equivalentColumnForRename(from, to pgschema.Column) bool {
	from.Name, to.Name = "", ""
	// ADD COLUMN appends physically, so a column first introduced earlier in a
	// feature may have a different catalog ordinal from its final declarative
	// placement. Position is evidence, not column identity.
	from.Position, to.Position = 0, 0
	from.NotNullConstraintName, to.NotNullConstraintName = "", ""
	from.Comment, to.Comment = nil, nil
	return reflect.DeepEqual(from, to)
}

// columnTriggerBridgeUnsupported bounds the automatic one-deployment column
// rename strategy. Expand temporarily exposes both names and a BEFORE trigger
// keeps their values identical. Contract drops the shadow and performs the
// native rename on the original column, preserving its position and all
// OID-backed dependencies. Anything that could change trigger ordering,
// privilege semantics, or value generation stays explicit unsupported state.
func columnTriggerBridgeUnsupported(current, desired *pgschema.Snapshot, from, to pgschema.Column) string {
	if from.Table != to.Table || from.Type != to.Type || from.Collation != to.Collation {
		return "column_shape_changed"
	}
	if from.Default != nil || to.Default != nil {
		return "column_default_requires_write_precedence"
	}
	if from.Identity != nil || to.Identity != nil || from.Serial != nil || to.Serial != nil || from.Generated != nil || to.Generated != nil {
		return "generated_value_semantics"
	}
	if from.NotNull && !reflect.DeepEqual(notNullConstraintBlockers(current), notNullConstraintBlockers(desired)) {
		// PostgreSQL 18 represents NOT NULL as a named catalog constraint. A
		// native column rename retains that physical constraint name, while a
		// freshly materialized desired schema derives a new name. Until those
		// constraints are typed graph nodes, do not report a convergent bridge.
		return "not_null_constraint_identity"
	}
	tableObject, exists := current.Object(from.Table)
	table, ok := tableObject.(pgschema.Table)
	if !exists || !ok {
		return "missing_table"
	}
	if table.Partition != nil || table.PartitionOf != nil {
		return "partition_trigger_propagation"
	}
	functionName, triggerName := columnBridgeNames(from, to)
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range snapshot.Objects() {
			switch object := object.(type) {
			case pgschema.Trigger:
				if object.Table == from.Table {
					if object.Name == triggerName {
						return "generated_trigger_name_collision"
					}
					return "existing_trigger_order"
				}
			case pgschema.Policy:
				if object.Table == from.Table {
					return "row_security_policy"
				}
			case pgschema.ReplicaIdentity:
				if object.Table == from.Table && object.Mode != pgschema.ReplicaIdentityDefault {
					return "replica_identity_state"
				}
			case pgschema.RowSecurity:
				if object.Table == from.Table && (object.Enabled || object.Forced) {
					return "row_security_state"
				}
			case pgschema.Routine:
				if object.Schema == from.Table.Schema && object.Name == functionName && object.Signature == "" {
					return "generated_function_name_collision"
				}
			}
		}
	}
	return ""
}

func notNullConstraintBlockers(snapshot *pgschema.Snapshot) []string {
	var blockers []string
	for _, selector := range snapshot.Unsupported() {
		if strings.HasPrefix(selector, "not_null_constraint:") {
			blockers = append(blockers, selector)
		}
	}
	return blockers
}

func columnBridgeNames(from, to pgschema.Column) (function, trigger string) {
	digest := sha256.Sum256([]byte(from.ObjectID().String() + "->" + to.ObjectID().String()))
	suffix := hex.EncodeToString(digest[:8])
	return "onwardpg_sync_column_" + suffix, "onwardpg_sync_column_" + suffix
}

func dependsOn(snapshot *pgschema.Snapshot, object, dependency pgschema.ID) bool {
	for _, candidate := range snapshot.Dependencies(object) {
		if candidate == dependency {
			return true
		}
	}
	return false
}

func equivalentAutomaticColumnReferenceAfterRename(before, after pgschema.Object, from, to pgschema.Column) bool {
	switch before := before.(type) {
	case pgschema.Constraint:
		next, ok := after.(pgschema.Constraint)
		if !ok {
			return false
		}
		before.Definition = renameIdentifierTokens(before.Definition, from.Name, to.Name)
		return reflect.DeepEqual(before, next)
	case pgschema.Index:
		next, ok := after.(pgschema.Index)
		if !ok {
			return false
		}
		for i := range before.Parts {
			if before.Parts[i].Column == from.Name {
				before.Parts[i].Column = to.Name
			}
		}
		// Index definitions are diagnostic-only and are deliberately not part
		// of semantic graph equality.
		before.Definition, next.Definition = "", ""
		return reflect.DeepEqual(before, next)
	case pgschema.Trigger:
		next, ok := after.(pgschema.Trigger)
		if !ok {
			return false
		}
		before.Definition = normalizeTriggerColumnReference(before.Definition, from.Name, to.Name)
		return reflect.DeepEqual(before, next)
	case pgschema.View:
		next, ok := after.(pgschema.View)
		if !ok || before.Materialized != next.Materialized || !isSimpleUnquotedIdentifier(from.Name) || !isSimpleUnquotedIdentifier(to.Name) {
			return false
		}
		expected, ok := automaticSimpleViewColumnRenameDefinition(before.Definition, from.Name, to.Name)
		if !ok {
			return false
		}
		before.Definition = expected
		return reflect.DeepEqual(before, next)
	default:
		return false
	}
}

func isSimpleUnquotedIdentifier(value string) bool {
	if value == "" || !isIdentifierStart(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !isIdentifierPart(value[i]) {
			return false
		}
	}
	return true
}

// automaticSimpleViewColumnRenameDefinition recognizes PostgreSQL's native
// rewrite for the deliberately narrow shape "SELECT old_column FROM ...".
// PostgreSQL preserves the view output name, rendering it as
// "SELECT new_column AS old_column FROM ...". More complex select lists,
// aliases, expressions, and quoted identifiers stay unsupported rather than
// being parsed or rewritten heuristically.
func automaticSimpleViewColumnRenameDefinition(definition, from, to string) (string, bool) {
	upper := strings.ToUpper(definition)
	selectAt := strings.Index(upper, "SELECT ")
	if selectAt < 0 {
		return "", false
	}
	columnStart := selectAt + len("SELECT ")
	fromAt := -1
	for at := columnStart; at < len(definition); at++ {
		if !strings.HasPrefix(upper[at:], "FROM") {
			continue
		}
		if at > columnStart && !strings.ContainsRune(" \t\n", rune(definition[at-1])) {
			continue
		}
		if at+len("FROM") < len(definition) && !strings.ContainsRune(" \t\n", rune(definition[at+len("FROM")])) {
			continue
		}
		fromAt = at
		break
	}
	if fromAt < 0 {
		return "", false
	}
	column := strings.TrimSpace(definition[columnStart:fromAt])
	qualifiedPrefix := ""
	if column != from {
		prefix, found := strings.CutSuffix(column, "."+from)
		if !found || !isSimpleUnquotedIdentifier(prefix) {
			return "", false
		}
		qualifiedPrefix = prefix + "."
	}
	columnEnd := fromAt
	for columnEnd > columnStart && strings.ContainsRune(" \t\n", rune(definition[columnEnd-1])) {
		columnEnd--
	}
	return definition[:columnStart] + qualifiedPrefix + to + " AS " + from + definition[columnEnd:], true
}

// normalizeTriggerColumnReference models only the two trigger-definition
// regions PostgreSQL changes for ALTER TABLE ... RENAME COLUMN: UPDATE OF
// column lists and the WHEN predicate. Trigger names, relation targets,
// routines, and other definition text deliberately remain untouched.
func normalizeTriggerColumnReference(definition, from, to string) string {
	result := definition
	if start := strings.Index(strings.ToUpper(result), " UPDATE OF "); start >= 0 {
		start += len(" UPDATE OF ")
		if end := strings.Index(strings.ToUpper(result[start:]), " ON "); end >= 0 {
			end += start
			result = result[:start] + renameIdentifierTokens(result[start:end], from, to) + result[end:]
		}
	}
	when := strings.Index(strings.ToUpper(result), " WHEN (")
	if when < 0 {
		return result
	}
	start := when + len(" WHEN (")
	end := triggerWhenExpressionEnd(result, start)
	if end < start {
		return result
	}
	return result[:start] + renameIdentifierTokens(result[start:end], from, to) + result[end:]
}

func triggerWhenExpressionEnd(value string, start int) int {
	depth := 1
	for at := start; at < len(value); {
		if end := skipSQLProtectedText(value, at); end > at {
			at = end
			continue
		}
		switch value[at] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return at
			}
		}
		at++
	}
	return -1
}

// renameIdentifierTokens makes the narrow catalog transformation PostgreSQL
// performs for a column rename. It changes identifier tokens only; quoted
// strings (for example CHECK values) are preserved verbatim.
func renameIdentifierTokens(value, from, to string) string {
	var result strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '\'' {
			start := i
			i++
			for i < len(value) {
				if value[i] == '\'' {
					i++
					if i < len(value) && value[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			result.WriteString(value[start:i])
			continue
		}
		if value[i] == '"' {
			start := i
			i++
			var identifier strings.Builder
			for i < len(value) {
				if value[i] == '"' {
					i++
					if i < len(value) && value[i] == '"' {
						identifier.WriteByte('"')
						i++
						continue
					}
					break
				}
				identifier.WriteByte(value[i])
				i++
			}
			if identifier.String() == from {
				result.WriteString(quote(to))
			} else {
				result.WriteString(value[start:i])
			}
			continue
		}
		if isIdentifierStart(value[i]) {
			start := i
			i++
			for i < len(value) && isIdentifierPart(value[i]) {
				i++
			}
			if value[start:i] == from {
				result.WriteString(to)
			} else {
				result.WriteString(value[start:i])
			}
			continue
		}
		result.WriteByte(value[i])
		i++
	}
	return result.String()
}

func isIdentifierStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
func isIdentifierPart(value byte) bool {
	return isIdentifierStart(value) || value >= '0' && value <= '9' || value == '$'
}

func renderColumnRename(rename renameColumn) []protocol.Statement {
	table := qualified(rename.from.Table.Schema, rename.from.Table.Name)
	oldColumn, newColumn := quote(rename.from.Name), quote(rename.to.Name)
	functionName, triggerName := columnBridgeNames(rename.from, rename.to)
	function := qualified(rename.from.Table.Schema, functionName)
	conflict := literal("onwardpg column bridge conflict on " + rename.from.Table.Schema + "." + rename.from.Table.Name + ": " + rename.from.Name + " and " + rename.to.Name + " differ")
	shadow := rename.to
	shadow.NotNull = false
	shadow.Default, shadow.Identity, shadow.Serial, shadow.Generated, shadow.Comment = nil, nil, nil, nil, nil
	functionSQL := "CREATE FUNCTION " + function + "() RETURNS trigger LANGUAGE plpgsql AS $onwardpg$\n" +
		"BEGIN\n" +
		"  IF TG_OP = 'INSERT' THEN\n" +
		"    IF NEW." + oldColumn + " IS NULL THEN\n" +
		"      NEW." + oldColumn + " := NEW." + newColumn + ";\n" +
		"    ELSIF NEW." + newColumn + " IS NULL THEN\n" +
		"      NEW." + newColumn + " := NEW." + oldColumn + ";\n" +
		"    ELSIF NEW." + oldColumn + " IS DISTINCT FROM NEW." + newColumn + " THEN\n" +
		"      RAISE EXCEPTION " + conflict + " USING ERRCODE = '23514';\n" +
		"    END IF;\n" +
		"  ELSIF NEW." + oldColumn + " IS DISTINCT FROM OLD." + oldColumn + " AND NEW." + newColumn + " IS NOT DISTINCT FROM OLD." + newColumn + " THEN\n" +
		"    NEW." + newColumn + " := NEW." + oldColumn + ";\n" +
		"  ELSIF NEW." + newColumn + " IS DISTINCT FROM OLD." + newColumn + " AND NEW." + oldColumn + " IS NOT DISTINCT FROM OLD." + oldColumn + " THEN\n" +
		"    NEW." + oldColumn + " := NEW." + newColumn + ";\n" +
		"  ELSIF NEW." + oldColumn + " IS DISTINCT FROM OLD." + oldColumn + " AND NEW." + newColumn + " IS DISTINCT FROM OLD." + newColumn + " AND NEW." + oldColumn + " IS DISTINCT FROM NEW." + newColumn + " THEN\n" +
		"    RAISE EXCEPTION " + conflict + " USING ERRCODE = '23514';\n" +
		"  END IF;\n" +
		"  RETURN NEW;\n" +
		"END\n" +
		"$onwardpg$;"
	statements := []protocol.Statement{
		statement("ALTER TABLE "+table+" ADD COLUMN "+renderColumn(shadow)+";", "expand", "review", true, "table_lock", "column_overlap_bridge"),
		statement(functionSQL, "expand", "review", true, "column_overlap_bridge", "trigger_function"),
		statement("CREATE TRIGGER "+quote(triggerName)+" BEFORE INSERT OR UPDATE OF "+oldColumn+", "+newColumn+" ON "+table+" FOR EACH ROW EXECUTE FUNCTION "+function+"();", "expand", "review", true, "column_overlap_bridge", "trigger_behavior_change", "table_lock"),
		statement("UPDATE "+table+" SET "+newColumn+" = "+oldColumn+" WHERE "+newColumn+" IS DISTINCT FROM "+oldColumn+";", "expand", "review", true, "column_overlap_bridge", "data_movement", "table_scan"),
		statement("UPDATE "+table+" SET "+newColumn+" = "+oldColumn+" WHERE "+newColumn+" IS DISTINCT FROM "+oldColumn+";", "contract", "review", true, "final_overlap_catchup", "data_movement", "table_scan"),
		statement("DROP TRIGGER "+quote(triggerName)+" ON "+table+";", "contract", "review", true, "compatibility_removal", "table_lock"),
		statement("DROP FUNCTION "+function+"();", "contract", "review", true, "compatibility_removal"),
		statement("ALTER TABLE "+table+" DROP COLUMN "+newColumn+";", "contract", "dangerous", true, "compatibility_removal", "data_loss", "access_exclusive_lock"),
		statement("ALTER TABLE "+table+" RENAME COLUMN "+oldColumn+" TO "+newColumn+";", "contract", "review", true, "compatibility_removal", "access_exclusive_lock"),
	}
	statements = appendNotNullConstraintRename(statements, table, rename, "contract")
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON COLUMN "+table+"."+quote(rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func renderDirectColumnRename(rename renameColumn) []protocol.Statement {
	table := qualified(rename.from.Table.Schema, rename.from.Table.Name)
	statements := []protocol.Statement{
		statement("ALTER TABLE "+table+" RENAME COLUMN "+quote(rename.from.Name)+" TO "+quote(rename.to.Name)+";", "expand", "review", true, "access_exclusive_lock"),
	}
	statements = appendNotNullConstraintRename(statements, table, rename, "expand")
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON COLUMN "+table+"."+quote(rename.to.Name)+" IS "+value+";", "expand", "safe", true))
	}
	return statements
}

func appendNotNullConstraintRename(statements []protocol.Statement, table string, rename renameColumn, phase string) []protocol.Statement {
	from, to := rename.from.NotNullConstraintName, rename.to.NotNullConstraintName
	if from == "" || to == "" || from == to {
		return statements
	}
	return append(statements, statement(
		"ALTER TABLE "+table+" RENAME CONSTRAINT "+quote(from)+" TO "+quote(to)+";",
		phase, "review", true, "access_exclusive_lock", "not_null_constraint_identity",
	))
}

func assertedTableIdentity(hints []protocol.Hint, from, to pgschema.Table) bool {
	for _, hint := range hints {
		if hint.Kind != "identity" || hint.Object != "table" {
			continue
		}
		if reflect.DeepEqual(hint.From, []string{from.Schema, from.Name}) && reflect.DeepEqual(hint.To, []string{to.Schema, to.Name}) {
			return true
		}
	}
	return false
}

func consumeAssertedTableIdentityChanges(changes []change.Change, from, to pgschema.Table, consumed map[pgschema.ID]bool) {
	for _, item := range changes {
		if tableIdentityChangeBelongsTo(item.Before, from, to) || tableIdentityChangeBelongsTo(item.After, from, to) {
			consumed[item.ID] = true
		}
	}
}

func tableIdentityChangeBelongsTo(value any, from, to pgschema.Table) bool {
	object, ok := value.(pgschema.Object)
	if !ok || object == nil {
		return false
	}
	id := object.ObjectID()
	if id == from.ObjectID() || id == to.ObjectID() || childTable(object) == from.ObjectID() || childTable(object) == to.ObjectID() {
		return true
	}
	constraint, ok := object.(pgschema.Constraint)
	return ok && constraint.Reference != nil && (*constraint.Reference == from.ObjectID() || *constraint.Reference == to.ObjectID())
}

// resolveRenameBridges turns a confirmed identity into an explicit handoff
// when PostgreSQL has no generic, online-compatible rename sequence. The
// planner has already bounded the old/new objects and any typed dependents;
// only the product's compatibility window and rollout SQL remain unknown.
func resolveRenameBridges(tables []renameTable, _ []renameColumn, views []renameView, routines []renameRoutine, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string, choices *decisions) ([]protocol.Question, error) {
	type bridge struct {
		id      pgschema.ID
		kind    string
		message string
	}
	bridges := make([]bridge, 0, len(tables)+len(views)+len(routines))
	for _, rename := range tables {
		if !rename.manualOnly {
			continue
		}
		bridges = append(bridges, bridge{rename.from.ObjectID(), "table", "Table " + rename.from.ObjectID().String() + " is explicitly asserted to be " + rename.to.ObjectID().String() + ", but its child catalog state does not match an automatic table-rename strategy. Supply the complete reviewed expand/contract transition around one application deployment; onwardpg will verify the resulting desired catalog but will not guess child identity, data movement, or application timing."})
	}
	for _, rename := range views {
		kind := "view"
		display := "View"
		if rename.from.Materialized {
			kind = "materialized view"
			display = "Materialized view"
		}
		bridges = append(bridges, bridge{rename.from.ObjectID(), kind, display + " " + rename.from.ObjectID().String() + " is confirmed as " + rename.to.ObjectID().String() + ". Supply the reviewed compatibility wrapper and dependent cutover SQL; onwardpg will not guess callers, refresh behavior, or availability requirements."})
	}
	for _, rename := range routines {
		bridges = append(bridges, bridge{rename.from.ObjectID(), "routine", "Routine " + rename.from.ObjectID().String() + " is confirmed as " + rename.to.ObjectID().String() + ". Supply the reviewed old-name wrapper, caller migration, and final removal SQL; onwardpg will not guess application call timing."})
	}
	sort.Slice(bridges, func(i, j int) bool { return bridges[i].id.String() < bridges[j].id.String() })
	questions := make([]protocol.Question, 0, len(bridges))
	for _, bridge := range bridges {
		question := protocol.Question{
			ID: "rename_compatibility_bridge:" + bridge.id.String(), Kind: "rename_compatibility_bridge", Key: bridge.id.String(),
			Message: bridge.message, Choices: []string{"provided"}, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		work, found, err := resolver.ResolveManual(question)
		if err != nil {
			return nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		choices.renameBridge[bridge.id] = work
	}
	return questions, nil
}

func resolveTableRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string, preserveSurplus bool, identityHints []protocol.Hint) ([]change.Change, []renameTable, []protocol.Question, []string, []protocol.DecisionAnalysis, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindTable {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameTable
	var compoundColumnChanges []change.Change
	var questions []protocol.Question
	var analysis []protocol.DecisionAnalysis
	for _, drop := range drops {
		from := drop.Before.(pgschema.Table)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Table)
			if equivalentTableForRename(current, desired, from, to) || assertedTableIdentity(identityHints, from, to) {
				candidates = append(candidates, create)
				continue
			}
			if reason, credible := tableRenameRejection(current, desired, from, to); credible {
				analysis = append(analysis, protocol.DecisionAnalysis{Kind: "rename_table", From: drop.ID.String(), To: create.ID.String(), Outcome: "rejected", Reason: reason})
			}
		}
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID.String() < candidates[j].ID.String() })
		choices := make([]string, 0, len(candidates)+1)
		for _, candidate := range candidates {
			choices = append(choices, candidate.ID.String())
		}
		noRenameChoice := "create"
		if preserveSurplus {
			noRenameChoice = "preserve"
		}
		choices = append(choices, noRenameChoice)
		question := protocol.Question{
			ID: "rename_table:" + drop.ID.String(), Kind: "rename_table", Key: drop.ID.String(),
			Message:            "Which desired table, if any, is the new identity of " + drop.ID.String() + "?",
			Choices:            choices,
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, nil, analysis, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer != noRenameChoice {
			var candidate change.Change
			for _, possible := range candidates {
				if possible.ID.String() == answer {
					candidate = possible
					break
				}
			}
			if candidate.ID.Kind == "" {
				return nil, nil, nil, nil, analysis, fmt.Errorf("resolved table rename candidate %q is unavailable", answer)
			}
			if consumed[candidate.ID] {
				return nil, nil, nil, nil, analysis, fmt.Errorf("desired table %s was selected as more than one rename target", candidate.ID)
			}
			to := candidate.After.(pgschema.Table)
			consumed[drop.ID], consumed[candidate.ID] = true, true
			if !equivalentTableForRename(current, desired, from, to) {
				// An explicit identity assertion may reach this branch even though
				// the automatic compatibility strategy cannot account for all table
				// children. Consume the bounded transition and request ordinary
				// editable SQL; never fall through to destructive drop/create.
				consumeAssertedTableIdentityChanges(changes, from, to, consumed)
				renames = append(renames, renameTable{from: from, to: to, manualOnly: true})
				continue
			}
			fromChildren, toChildren := tableChildren(current, from.ObjectID()), tableChildren(desired, to.ObjectID())
			var derivedChildRenames []tableChildRename
			var notNullConstraintRenames []notNullConstraintRename
			for key, before := range fromChildren {
				after := toChildren[key]
				consumed[before.ObjectID()] = true
				consumed[after.ObjectID()] = true
				if beforeColumn, ok := before.(pgschema.Column); ok {
					afterColumn := after.(pgschema.Column)
					if beforeColumn.NotNullConstraintName != "" && afterColumn.NotNullConstraintName != "" && beforeColumn.NotNullConstraintName != afterColumn.NotNullConstraintName {
						notNullConstraintRenames = append(notNullConstraintRenames, notNullConstraintRename{
							from: beforeColumn.NotNullConstraintName,
							to:   afterColumn.NotNullConstraintName,
						})
					}
				}
				if !equivalentChildForTableRename(before, after, from, to) {
					beforeColumn := before.(pgschema.Column)
					afterColumn := after.(pgschema.Column)
					afterColumn.Table = from.ObjectID()
					compoundColumnChanges = append(compoundColumnChanges, change.Change{
						Kind: change.Modify, ID: beforeColumn.ObjectID(), Before: beforeColumn, After: afterColumn,
					})
				}
				if derivedTableChildNameChanged(before, after, from, to) {
					derivedChildRenames = append(derivedChildRenames, tableChildRename{from: before, to: after})
				}
			}
			for key, after := range toChildren {
				if _, exists := fromChildren[key]; exists {
					continue
				}
				column := after.(pgschema.Column)
				column.Table = from.ObjectID()
				for index := range changes {
					if changes[index].Kind == change.Create && changes[index].ID == after.ObjectID() {
						changes[index].After = column
					}
				}
			}
			sort.Slice(derivedChildRenames, func(i, j int) bool {
				return derivedChildRenames[i].from.ObjectID().String() < derivedChildRenames[j].from.ObjectID().String()
			})
			sort.Slice(notNullConstraintRenames, func(i, j int) bool {
				return notNullConstraintRenames[i].from < notNullConstraintRenames[j].from
			})
			retainedViews := retainedTableRenameViews(current, desired, from, to)
			retainedViewSet := make(map[pgschema.ID]bool, len(retainedViews))
			for _, id := range retainedViews {
				retainedViewSet[id] = true
				consumed[id] = true
			}
			for _, object := range current.Objects() {
				view, ok := object.(pgschema.View)
				if !ok || retainedViewSet[view.ObjectID()] || !viewDependsOnTable(current, view.ObjectID(), from) {
					continue
				}
				if _, exists := desired.Object(view.ObjectID()); exists {
					return nil, nil, nil, []string{"table_rename_dependent_view:" + view.ObjectID().String()}, analysis, nil
				}
			}
			// PostgreSQL updates foreign keys in other tables when their
			// referenced table is renamed. Suppress only the FK modifications
			// that are exactly this automatic reference rewrite; genuine FK
			// changes remain scheduled normally.
			for _, object := range current.Objects() {
				before, ok := object.(pgschema.Constraint)
				if !ok || before.Reference == nil || *before.Reference != from.ObjectID() {
					continue
				}
				afterObject, exists := desired.Object(before.ObjectID())
				after, ok := afterObject.(pgschema.Constraint)
				if !exists || !ok || !equivalentExternalReferenceAfterTableRename(before, after, to) {
					continue
				}
				consumed[before.ObjectID()] = true
			}
			columns, privileges, compatibilityUnsupported := tableRenameCompatibility(current, desired, from, to)
			if len(compatibilityUnsupported) > 0 {
				return nil, nil, nil, compatibilityUnsupported, analysis, nil
			}
			renames = append(renames, renameTable{
				from: from, to: to, compatibilityColumns: columns, compatibilityPrivileges: privileges, derivedChildRenames: derivedChildRenames,
				notNullConstraintRenames: notNullConstraintRenames,
			})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil, analysis, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	filtered = append(filtered, compoundColumnChanges...)
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].ID.String() == filtered[j].ID.String() {
			return filtered[i].Kind < filtered[j].Kind
		}
		return filtered[i].ID.String() < filtered[j].ID.String()
	})
	return filtered, renames, questions, nil, analysis, nil
}

// retainedTableRenameViews captures PostgreSQL's native pg_rewrite update for
// direct ordinary/materialized view dependencies on a renamed table. Their
// relation OID and materialized rows remain valid; consume only an exact
// catalog-definition rewrite instead of scheduling a view change before the
// table rename or rebuilding a materialized view.
func retainedTableRenameViews(current, desired *pgschema.Snapshot, from, to pgschema.Table) []pgschema.ID {
	var retained []pgschema.ID
	fromReference := pgschema.View{Schema: from.Schema, Name: from.Name}
	toReference := pgschema.View{Schema: to.Schema, Name: to.Name}
	for _, object := range current.Objects() {
		before, ok := object.(pgschema.View)
		if !ok || !viewDependsOnTable(current, before.ObjectID(), from) {
			continue
		}
		afterObject, exists := desired.Object(before.ObjectID())
		after, ok := afterObject.(pgschema.View)
		if !exists || !ok || after.Materialized != before.Materialized || !viewDependsOnTable(desired, after.ObjectID(), to) {
			continue
		}
		normalized, changed := normalizeViewRelationReference(before.Definition, fromReference, toReference)
		if !changed {
			continue
		}
		before.Definition = normalized
		if reflect.DeepEqual(before, after) {
			retained = append(retained, before.ObjectID())
		}
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].String() < retained[j].String() })
	return retained
}

func viewDependsOnTable(snapshot *pgschema.Snapshot, view pgschema.ID, table pgschema.Table) bool {
	for _, dependency := range snapshot.Dependencies(view) {
		if dependency == table.ObjectID() {
			return true
		}
		if dependency.Kind == pgschema.KindColumn && dependency.Schema == table.Schema && dependency.Name == table.Name {
			return true
		}
	}
	return false
}

func resolveViewRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameView, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindView && item.ID.Kind != pgschema.KindMatView {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameView
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.View)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.View)
			if equivalentViewForRename(from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		indexChanges, indexedRenameSafe := materializedViewRenameIndexes(changes, from.ObjectID(), candidate.After.(pgschema.View).ObjectID())
		if from.Materialized && !indexedRenameSafe {
			continue
		}
		dependentRewrites, retainedDependents, safe := dependentViewRewrites(changes, current, desired, from, candidate.After.(pgschema.View))
		if !safe {
			continue
		}
		question := protocol.Question{
			ID: "rename_view:" + drop.ID.String(), Kind: "rename_view", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			for _, rewrite := range indexChanges {
				consumed[rewrite.before.ObjectID()], consumed[rewrite.after.ObjectID()] = true, true
			}
			for _, rewrite := range dependentRewrites {
				consumed[rewrite.before.ObjectID()] = true
			}
			for _, id := range retainedDependents {
				consumed[id] = true
			}
			renames = append(renames, renameView{from: from, to: candidate.After.(pgschema.View), dependents: dependentRewrites, indexComments: indexChanges})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

func materializedViewRenameIndexes(changes []change.Change, from, to pgschema.ID) ([]indexCommentRewrite, bool) {
	var dropped, created []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindIndex {
			continue
		}
		switch item.Kind {
		case change.Drop:
			if index, ok := item.Before.(pgschema.Index); ok && index.Table == from {
				dropped = append(dropped, item)
			}
		case change.Create:
			if index, ok := item.After.(pgschema.Index); ok && index.Table == to {
				created = append(created, item)
			}
		case change.Modify:
			if index, ok := item.Before.(pgschema.Index); ok && (index.Table == from || index.Table == to) {
				return nil, false
			}
		}
	}
	if len(dropped) != len(created) {
		return nil, false
	}
	rewrites := make([]indexCommentRewrite, 0, len(dropped))
	used := make(map[pgschema.ID]bool, len(created))
	for _, drop := range dropped {
		before := drop.Before.(pgschema.Index)
		matched := false
		for _, create := range created {
			if used[create.ID] {
				continue
			}
			after := create.After.(pgschema.Index)
			if equivalentIndexAcrossRelationRename(before, after) {
				used[create.ID], matched = true, true
				rewrites = append(rewrites, indexCommentRewrite{before: before, after: after})
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	return rewrites, true
}

func equivalentIndexAcrossRelationRename(before, after pgschema.Index) bool {
	before.Table = after.Table
	before.Comment, after.Comment = nil, nil
	before.Definition, after.Definition = "", ""
	return reflect.DeepEqual(before, after)
}

// dependentViewRewrites returns the transitive ordinary-view dependents that
// PostgreSQL can safely retarget after a view rename. Their desired definition
// is reapplied only after ALTER VIEW, so no statement references the new name
// before it exists. Any drop/create or materialized-view participant leaves
// the rename on the conservative path.
func dependentViewRewrites(changes []change.Change, current, desired *pgschema.Snapshot, from, to pgschema.View) ([]viewRewrite, []pgschema.ID, bool) {
	changeByID := make(map[pgschema.ID]change.Change, len(changes))
	for _, item := range changes {
		changeByID[item.ID] = item
	}
	seen := map[pgschema.ID]bool{from.ObjectID(): true}
	queue := []pgschema.ID{from.ObjectID()}
	var rewrites []viewRewrite
	var retained []pgschema.ID
	for len(queue) > 0 {
		dependency := queue[0]
		queue = queue[1:]
		for _, object := range current.Objects() {
			view, ok := object.(pgschema.View)
			if !ok || seen[view.ObjectID()] || !dependsOn(current, view.ObjectID(), dependency) {
				continue
			}
			if view.Materialized {
				afterObject, exists := desired.Object(view.ObjectID())
				after, afterIsView := afterObject.(pgschema.View)
				if !exists || !afterIsView || !after.Materialized || !dependsOn(desired, after.ObjectID(), to.ObjectID()) || !equivalentMaterializedDependentAfterViewRename(view, after, from, to) {
					// A materialized view cannot be CREATE OR REPLACEd. Permit only
					// PostgreSQL's native catalog rewrite; any actual definition
					// change still needs its own reviewed rebuild transition.
					return nil, nil, false
				}
				seen[view.ObjectID()] = true
				retained = append(retained, view.ObjectID())
				continue
			}
			seen[view.ObjectID()] = true
			queue = append(queue, view.ObjectID())
			afterObject, exists := desired.Object(view.ObjectID())
			after, afterIsView := afterObject.(pgschema.View)
			if !exists || !afterIsView || after.Materialized {
				return nil, nil, false
			}
			if _, unsupported := renderViewCreate(after, true); unsupported != "" {
				return nil, nil, false
			}
			item, changed := changeByID[view.ObjectID()]
			if !changed || item.Kind != change.Modify {
				// A dependent whose catalog state is already unchanged needs no
				// rewrite, but it remains part of the traversal.
				continue
			}
			rewrites = append(rewrites, viewRewrite{before: view, after: after})
		}
	}
	sort.Slice(rewrites, func(i, j int) bool {
		return rewrites[i].after.ObjectID().String() < rewrites[j].after.ObjectID().String()
	})
	sort.Slice(retained, func(i, j int) bool { return retained[i].String() < retained[j].String() })
	return rewrites, retained, true
}

// equivalentMaterializedDependentAfterViewRename accepts only PostgreSQL's
// native rewrite of a materialized dependent during an ALTER ... RENAME. It
// compares the full typed view after replacing relation tokens in the deparsed
// definition; a literal, option, population, or other definition change keeps
// the transition conservative rather than hiding a rebuild.
func equivalentMaterializedDependentAfterViewRename(before, after, from, to pgschema.View) bool {
	normalized, changed := normalizeViewRelationReference(before.Definition, from, to)
	if !changed {
		return false
	}
	before.Definition = normalized
	return reflect.DeepEqual(before, after)
}

// normalizeViewRelationReference changes only a relation position immediately
// following deparsed FROM or JOIN. It deliberately skips SQL strings, dollar
// strings, quoted identifiers, and comments, so a literal mentioning an old
// view cannot be mistaken for PostgreSQL's native catalog rewrite.
func normalizeViewRelationReference(definition string, from, to pgschema.View) (string, bool) {
	oldReferences := []string{qualified(from.Schema, from.Name), from.Schema + "." + from.Name, from.Name}
	newReferences := []string{qualified(to.Schema, to.Name), to.Schema + "." + to.Name, to.Name}
	var result strings.Builder
	result.Grow(len(definition))
	changed := false
	for i := 0; i < len(definition); {
		if replacement, length, ok := viewRelationReferenceAt(definition, i, oldReferences, newReferences); ok {
			result.WriteString(replacement)
			i += length
			changed = true
			continue
		}
		if end := skipSQLProtectedText(definition, i); end > i {
			result.WriteString(definition[i:end])
			i = end
			continue
		}
		if !isIdentifierStart(definition[i]) {
			result.WriteByte(definition[i])
			i++
			continue
		}
		start := i
		for i < len(definition) && isIdentifierPart(definition[i]) {
			i++
		}
		word := definition[start:i]
		result.WriteString(word)
		if !strings.EqualFold(word, "from") && !strings.EqualFold(word, "join") {
			continue
		}
		spaceStart := i
		for i < len(definition) && (definition[i] == ' ' || definition[i] == '\t' || definition[i] == '\n') {
			i++
		}
		result.WriteString(definition[spaceStart:i])
		if replacement, length, ok := viewRelationReferenceAt(definition, i, oldReferences, newReferences); ok {
			result.WriteString(replacement)
			i += length
			changed = true
		}
	}
	return result.String(), changed
}

func viewRelationReferenceAt(value string, at int, oldReferences, newReferences []string) (string, int, bool) {
	if at > 0 && (isIdentifierPart(value[at-1]) || value[at-1] == '.') {
		return "", 0, false
	}
	for index, oldReference := range oldReferences {
		if !strings.HasPrefix(value[at:], oldReference) {
			continue
		}
		end := at + len(oldReference)
		if !relationReferenceBoundary(value, end) {
			continue
		}
		// A schema-less reference is accepted only as a relation qualifier
		// (old_view.column). PostgreSQL 14/15 use this form in a deparsed
		// target list while retaining the schema-qualified FROM relation. Do
		// not treat a bare function or other identifier as a renamed relation.
		if index == len(oldReferences)-1 && (end == len(value) || value[end] != '.') {
			continue
		}
		return newReferences[index], len(oldReference), true
	}
	return "", 0, false
}

func relationReferenceBoundary(value string, at int) bool {
	if at == len(value) {
		return true
	}
	switch value[at] {
	case ' ', '\t', '\n', ',', ')', ';', '.':
		return true
	default:
		return false
	}
}

func skipSQLProtectedText(value string, start int) int {
	if start >= len(value) {
		return start
	}
	switch value[start] {
	case '\'':
		for at := start + 1; at < len(value); at++ {
			if value[at] != '\'' {
				continue
			}
			if at+1 < len(value) && value[at+1] == '\'' {
				at++
				continue
			}
			return at + 1
		}
		return len(value)
	case '"':
		for at := start + 1; at < len(value); at++ {
			if value[at] != '"' {
				continue
			}
			if at+1 < len(value) && value[at+1] == '"' {
				at++
				continue
			}
			return at + 1
		}
		return len(value)
	case '-':
		if start+1 < len(value) && value[start+1] == '-' {
			if end := strings.IndexByte(value[start+2:], '\n'); end >= 0 {
				return start + 2 + end + 1
			}
			return len(value)
		}
	case '/':
		if start+1 < len(value) && value[start+1] == '*' {
			if end := strings.Index(value[start+2:], "*/"); end >= 0 {
				return start + 2 + end + 2
			}
			return len(value)
		}
	case '$':
		at := start + 1
		for at < len(value) && (isIdentifierPart(value[at])) {
			at++
		}
		if at < len(value) && value[at] == '$' {
			delimiter := value[start : at+1]
			if end := strings.Index(value[at+1:], delimiter); end >= 0 {
				return at + 1 + end + len(delimiter)
			}
			return len(value)
		}
	}
	return start
}

func equivalentViewForRename(from, to pgschema.View) bool {
	from.Schema, to.Schema = "", ""
	from.Name, to.Name = "", ""
	from.Comment, to.Comment = nil, nil
	return reflect.DeepEqual(from, to)
}

func renderViewRename(rename renameView) []protocol.Statement {
	kind := "VIEW"
	if rename.from.Materialized {
		kind = "MATERIALIZED VIEW"
	}
	from := qualified(rename.from.Schema, rename.from.Name)
	current := from
	statements := make([]protocol.Statement, 0, 3)
	if rename.from.Schema != rename.to.Schema {
		statements = append(statements, statement("ALTER "+kind+" "+from+" SET SCHEMA "+quote(rename.to.Schema)+";", "contract", "review", true, "application_contract"))
		current = qualified(rename.to.Schema, rename.from.Name)
	}
	if rename.from.Name != rename.to.Name {
		statements = append(statements, statement("ALTER "+kind+" "+current+" RENAME TO "+quote(rename.to.Name)+";", "contract", "review", true, "application_contract"))
	}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON "+kind+" "+qualified(rename.to.Schema, rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func renderDependentViewRewrite(rewrite viewRewrite) []protocol.Statement {
	sql, unsupported := renderViewCreate(rewrite.after, true)
	if unsupported != "" {
		return nil
	}
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "dependent_view_rewrite", "application_contract")}
	if !reflect.DeepEqual(rewrite.before.Comment, rewrite.after.Comment) {
		value := "NULL"
		if rewrite.after.Comment != nil {
			value = literal(*rewrite.after.Comment)
		}
		statements = append(statements, statement("COMMENT ON VIEW "+qualified(rewrite.after.Schema, rewrite.after.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func renderIndexCommentRewrite(rewrite indexCommentRewrite) []protocol.Statement {
	if reflect.DeepEqual(rewrite.before.Comment, rewrite.after.Comment) {
		return nil
	}
	value := "NULL"
	if rewrite.after.Comment != nil {
		value = literal(*rewrite.after.Comment)
	}
	return []protocol.Statement{statement("COMMENT ON INDEX "+qualified(rewrite.after.Table.Schema, rewrite.after.Name)+" IS "+value+";", "contract", "safe", true)}
}

func resolveRoutineRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameRoutine, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindRoutine {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameRoutine
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Routine)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Routine)
			if to.Kind == from.Kind && to.Signature == from.Signature {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		triggerRewrites, safe := dependentTriggerRewrites(changes, current, desired, from.ObjectID(), candidate.After.(pgschema.Routine).ObjectID())
		if !safe {
			continue
		}
		question := protocol.Question{
			ID: "rename_routine:" + drop.ID.String(), Kind: "rename_routine", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "? Routine names are application contracts; confirm explicitly.",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			for _, rewrite := range triggerRewrites {
				consumed[rewrite.before.ObjectID()] = true
			}
			for _, id := range retainedMaterializedRoutineDependents(current, desired, from, candidate.After.(pgschema.Routine)) {
				consumed[id] = true
			}
			renames = append(renames, renameRoutine{from: from, to: candidate.After.(pgschema.Routine), triggers: triggerRewrites})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

// retainedMaterializedRoutineDependents identifies materialized views whose
// only desired change is PostgreSQL's catalog rewrite of a qualified user
// routine call after ALTER FUNCTION/PROCEDURE ... RENAME. Their stored rows
// remain valid because the routine OID is retained, so a destructive rebuild
// would be spurious. Ordinary views are intentionally left to the existing
// post-rename replacement path.
func retainedMaterializedRoutineDependents(current, desired *pgschema.Snapshot, from, to pgschema.Routine) []pgschema.ID {
	var retained []pgschema.ID
	for _, object := range current.Objects() {
		before, ok := object.(pgschema.View)
		if !ok || !before.Materialized || !dependsOn(current, before.ObjectID(), from.ObjectID()) {
			continue
		}
		afterObject, exists := desired.Object(before.ObjectID())
		after, ok := afterObject.(pgschema.View)
		if !exists || !ok || !after.Materialized || !dependsOn(desired, after.ObjectID(), to.ObjectID()) || !equivalentMaterializedRoutineDependentAfterRename(before, after, from, to) {
			continue
		}
		retained = append(retained, before.ObjectID())
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].String() < retained[j].String() })
	return retained
}

func equivalentMaterializedRoutineDependentAfterRename(before, after pgschema.View, from, to pgschema.Routine) bool {
	normalized, changed := normalizeRoutineCallReferences(before.Definition, from, to)
	if !changed {
		return false
	}
	before.Definition = normalized
	return reflect.DeepEqual(before, after)
}

// normalizeRoutineCallReferences changes only schema-qualified routine call
// tokens immediately followed by "(". It skips literals, quoted strings, and
// comments, and deliberately does not accept bare names or parse arbitrary
// expressions. That keeps a routine-rename proof distinct from a source-code
// rewrite.
func normalizeRoutineCallReferences(definition string, from, to pgschema.Routine) (string, bool) {
	oldReferences := []string{qualified(from.Schema, from.Name), from.Schema + "." + from.Name}
	newReferences := []string{qualified(to.Schema, to.Name), to.Schema + "." + to.Name}
	var result strings.Builder
	result.Grow(len(definition))
	changed := false
	for at := 0; at < len(definition); {
		if definition[at] == '\'' || definition[at] == '$' || (definition[at] == '-' && at+1 < len(definition) && definition[at+1] == '-') || (definition[at] == '/' && at+1 < len(definition) && definition[at+1] == '*') {
			if end := skipSQLProtectedText(definition, at); end > at {
				result.WriteString(definition[at:end])
				at = end
				continue
			}
		}
		matched := false
		for index, oldReference := range oldReferences {
			end := at + len(oldReference)
			if !strings.HasPrefix(definition[at:], oldReference) || (at > 0 && (isIdentifierPart(definition[at-1]) || definition[at-1] == '.')) || end >= len(definition) || definition[end] != '(' {
				continue
			}
			result.WriteString(newReferences[index])
			at = end
			changed, matched = true, true
			break
		}
		if matched {
			continue
		}
		result.WriteByte(definition[at])
		at++
	}
	return result.String(), changed
}

func dependentTriggerRewrites(changes []change.Change, current, desired *pgschema.Snapshot, from, to pgschema.ID) ([]triggerRewrite, bool) {
	changeByID := make(map[pgschema.ID]change.Change, len(changes))
	for _, item := range changes {
		changeByID[item.ID] = item
	}
	var rewrites []triggerRewrite
	for _, object := range current.Objects() {
		trigger, ok := object.(pgschema.Trigger)
		if !ok || trigger.Routine != from {
			continue
		}
		afterObject, exists := desired.Object(trigger.ObjectID())
		after, afterIsTrigger := afterObject.(pgschema.Trigger)
		if !exists || !afterIsTrigger || after.Routine != to {
			return nil, false
		}
		if _, unsupported := renderTriggerDefinition(after); unsupported != "" {
			return nil, false
		}
		item, changed := changeByID[trigger.ObjectID()]
		if !changed || item.Kind != change.Modify {
			continue
		}
		rewrites = append(rewrites, triggerRewrite{before: trigger, after: after})
	}
	sort.Slice(rewrites, func(i, j int) bool {
		return rewrites[i].after.ObjectID().String() < rewrites[j].after.ObjectID().String()
	})
	return rewrites, true
}

func renderRoutineRename(rename renameRoutine) []protocol.Statement {
	kind := strings.ToUpper(rename.from.Kind)
	from := qualified(rename.from.Schema, rename.from.Name) + "(" + rename.from.Signature + ")"
	current := from
	statements := make([]protocol.Statement, 0, 3)
	if rename.from.Schema != rename.to.Schema {
		statements = append(statements, statement("ALTER "+kind+" "+from+" SET SCHEMA "+quote(rename.to.Schema)+";", "contract", "review", true, "application_contract"))
		current = qualified(rename.to.Schema, rename.from.Name) + "(" + rename.from.Signature + ")"
	}
	if rename.from.Name != rename.to.Name {
		statements = append(statements, statement("ALTER "+kind+" "+current+" RENAME TO "+quote(rename.to.Name)+";", "contract", "review", true, "application_contract"))
	}
	// pg_get_functiondef remains the desired authoritative definition after an
	// ALTER rename, including a body that may intentionally mention the new
	// name. Reapply it rather than attempting a source rewrite.
	if definition, unsupported := renderRoutineDefinition(rename.to); unsupported == "" {
		statements = append(statements, statement(definition, "contract", "review", true, "routine_replace", "application_contract"))
	}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON "+strings.ToUpper(rename.to.Kind)+" "+qualified(rename.to.Schema, rename.to.Name)+"("+rename.to.Signature+") IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func renderDependentTriggerRewrite(rewrite triggerRewrite) []protocol.Statement {
	sql, unsupported := renderTriggerDefinition(rewrite.after)
	if unsupported != "" {
		return nil
	}
	// PostgreSQL 14+ supports CREATE OR REPLACE TRIGGER. Reapply the desired
	// definition after the routine rename without a destructive trigger gap.
	sql = strings.Replace(sql, "CREATE TRIGGER", "CREATE OR REPLACE TRIGGER", 1)
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "dependent_trigger_rewrite", "routine_behavior_change", "table_lock")}
	if rewrite.after.Enabled != rewrite.before.Enabled {
		enabled, unsupported := renderTriggerEnabled(rewrite.after)
		if unsupported != "" {
			return nil
		}
		statements = append(statements, statement(enabled, "contract", "review", true, "trigger_enable_state", "routine_behavior_change", "table_lock"))
	}
	return statements
}

func resolveTriggerRenames(changes []change.Change, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameTrigger, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindTrigger {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameTrigger
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Trigger)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Trigger)
			if equivalentTriggerForRename(from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		question := protocol.Question{
			ID: "rename_trigger:" + drop.ID.String(), Kind: "rename_trigger", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			renames = append(renames, renameTrigger{from: from, to: candidate.After.(pgschema.Trigger)})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

func equivalentTriggerForRename(from, to pgschema.Trigger) bool {
	return from.Table == to.Table && from.Routine == to.Routine && from.Enabled == to.Enabled && triggerDefinitionTail(from.Definition) == triggerDefinitionTail(to.Definition)
}

// triggerDefinitionTail removes only PostgreSQL's canonical CREATE TRIGGER
// name token. It is not a general SQL parser: the rest of the catalog-derived
// definition must be byte-for-byte equal before a rename question is offered.
func triggerDefinitionTail(definition string) string {
	value := strings.TrimSpace(definition)
	prefix := "CREATE TRIGGER "
	if strings.HasPrefix(strings.ToUpper(value), "CREATE CONSTRAINT TRIGGER ") {
		prefix = "CREATE CONSTRAINT TRIGGER "
	} else if !strings.HasPrefix(strings.ToUpper(value), prefix) {
		return ""
	}
	value = strings.TrimSpace(value[len(prefix):])
	if value == "" {
		return ""
	}
	if value[0] == '"' {
		for i := 1; i < len(value); i++ {
			if value[i] != '"' {
				continue
			}
			if i+1 < len(value) && value[i+1] == '"' {
				i++
				continue
			}
			return strings.TrimSpace(value[i+1:])
		}
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] == ' ' || value[i] == '\t' || value[i] == '\n' {
			return strings.TrimSpace(value[i:])
		}
	}
	return ""
}

func renderTriggerRename(rename renameTrigger) []protocol.Statement {
	sql := "ALTER TRIGGER " + quote(rename.from.Name) + " ON " + qualified(rename.from.Table.Schema, rename.from.Table.Name) + " RENAME TO " + quote(rename.to.Name) + ";"
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "trigger_identity", "brief_lock")}
	identifier := quote(rename.to.Name) + " ON " + qualified(rename.to.Table.Schema, rename.to.Table.Name)
	return append(statements, commentModificationPhase("TRIGGER", identifier, rename.from.Comment, rename.to.Comment, "contract")...)
}

func equivalentTableForRename(current, desired *pgschema.Snapshot, from, to pgschema.Table) bool {
	fromTable, toTable := from, to
	fromTable.Schema, toTable.Schema = "", ""
	fromTable.Name, toTable.Name = "", ""
	fromTable.Comment, toTable.Comment = nil, nil
	if !reflect.DeepEqual(fromTable, toTable) {
		return false
	}
	fromChildren := tableChildren(current, from.ObjectID())
	toChildren := tableChildren(desired, to.ObjectID())
	for key, before := range fromChildren {
		after, exists := toChildren[key]
		if !exists {
			return false
		}
		if !equivalentChildForTableRename(before, after, from, to) && !plannableColumnChangeDuringTableRename(before, after) {
			return false
		}
	}
	for key, after := range toChildren {
		if _, exists := fromChildren[key]; exists {
			continue
		}
		if _, ok := after.(pgschema.Column); !ok {
			return false
		}
	}
	return true
}

// tableRenameRejection reports only pairs whose relation-level shape matches:
// those are credible near-misses an agent may reasonably inspect or assert.
// It deliberately does not claim that arbitrary similarly named tables are
// rename candidates.
func tableRenameRejection(current, desired *pgschema.Snapshot, from, to pgschema.Table) (string, bool) {
	fromTable, toTable := from, to
	fromTable.Schema, toTable.Schema = "", ""
	fromTable.Name, toTable.Name = "", ""
	fromTable.Comment, toTable.Comment = nil, nil
	if !reflect.DeepEqual(fromTable, toTable) {
		return "table_shape_changed", false
	}
	fromChildren := tableChildren(current, from.ObjectID())
	toChildren := tableChildren(desired, to.ObjectID())
	for key, before := range fromChildren {
		after, exists := toChildren[key]
		if !exists {
			return "child_identity_mismatch:" + before.ObjectID().String(), true
		}
		if !equivalentChildForTableRename(before, after, from, to) && !plannableColumnChangeDuringTableRename(before, after) {
			return "child_semantics_changed:" + before.ObjectID().String(), true
		}
	}
	for key, after := range toChildren {
		if _, exists := fromChildren[key]; !exists {
			if _, column := after.(pgschema.Column); !column {
				return "new_noncolumn_child:" + after.ObjectID().String(), true
			}
		}
	}
	return "not_a_rename_candidate", true
}

func plannableColumnChangeDuringTableRename(before, after pgschema.Object) bool {
	beforeColumn, beforeOK := before.(pgschema.Column)
	afterColumn, afterOK := after.(pgschema.Column)
	return beforeOK && afterOK && beforeColumn.Name == afterColumn.Name && beforeColumn.Position == afterColumn.Position
}

func tableChildren(snapshot *pgschema.Snapshot, table pgschema.ID) map[string]pgschema.Object {
	children := make(map[string]pgschema.Object)
	for _, object := range snapshot.Objects() {
		if childTable(object) == table {
			children[tableChildKey(object, table)] = object
		}
	}
	return children
}

// tableChildKey makes a table rename candidate robust to the precise subset of
// names PostgreSQL itself derives from the relation and its key columns. It is
// intentionally narrower than "looks table-prefixed": a custom name remains a
// material catalog change and cannot disappear inside a table rename.
func tableChildKey(object pgschema.Object, table pgschema.ID) string {
	normalized, derived := normalizeDerivedTableChildName(object, table.Name)
	if !derived {
		id := object.ObjectID()
		return string(id.Kind) + ":" + id.Part + ":" + id.Signature
	}
	switch child := normalized.(type) {
	case pgschema.Constraint:
		child.Table = pgschema.ID{}
		if child.Reference != nil && *child.Reference == table {
			child.Reference = nil
		}
		return "derived_constraint:" + stableChildKey(child)
	case pgschema.Index:
		child.Table = pgschema.ID{}
		child.Definition = ""
		return "derived_index:" + stableChildKey(child)
	default:
		id := object.ObjectID()
		return string(id.Kind) + ":" + id.Part + ":" + id.Signature
	}
}

func stableChildKey(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("unencodable:%T:%v", value, err)
	}
	return string(encoded)
}

func derivedTableChildNameChanged(before, after pgschema.Object, from, to pgschema.Table) bool {
	_, leftDerived := normalizeDerivedTableChildName(before, from.Name)
	_, rightDerived := normalizeDerivedTableChildName(after, to.Name)
	if !leftDerived || !rightDerived || before.ObjectID().Part == after.ObjectID().Part {
		return false
	}
	switch before.(type) {
	case pgschema.Constraint, pgschema.Index:
		return true
	default:
		return false
	}
}

func normalizeDerivedTableChildName(object pgschema.Object, table string) (pgschema.Object, bool) {
	switch child := object.(type) {
	case pgschema.Constraint:
		expected, ok := derivedConstraintName(child, table)
		if !ok || child.Name != expected {
			return object, false
		}
		child.Name = ""
		if child.UsingIndex == expected {
			child.UsingIndex = ""
		}
		return child, true
	case pgschema.Index:
		expected, ok := derivedIndexName(child, table)
		if !ok || child.Name != expected {
			return object, false
		}
		child.Name = ""
		if child.Constraint == expected {
			child.Constraint = ""
		}
		return child, true
	default:
		return object, false
	}
}

func derivedConstraintName(constraint pgschema.Constraint, table string) (string, bool) {
	if constraint.Type == pgschema.ConstraintPrimary {
		return table + "_pkey", true
	}
	var suffix string
	switch constraint.Type {
	case pgschema.ConstraintUnique:
		suffix = "_key"
	case pgschema.ConstraintForeign:
		suffix = "_fkey"
	default:
		return "", false
	}
	columns, ok := constraintKeyColumns(constraint.Definition)
	if !ok {
		return "", false
	}
	return table + "_" + strings.Join(columns, "_") + suffix, true
}

func derivedIndexName(index pgschema.Index, table string) (string, bool) {
	if index.Primary {
		return table + "_pkey", true
	}
	columns := make([]string, 0, len(index.Parts))
	for _, part := range index.Parts {
		if part.Column == "" || part.Expression != "" {
			return "", false
		}
		columns = append(columns, part.Column)
	}
	if len(columns) == 0 {
		return "", false
	}
	suffix := "_idx"
	if index.Constraint != "" {
		suffix = "_key"
	}
	return table + "_" + strings.Join(columns, "_") + suffix, true
}

// constraintKeyColumns recognizes the catalog-rendered leading key list used
// by PRIMARY/UNIQUE/FOREIGN KEY constraints. Complex or truncated generated
// names are deliberately not normalized: they continue to require an explicit
// change rather than being guessed as conventional names.
func constraintKeyColumns(definition string) ([]string, bool) {
	start := strings.Index(definition, "(")
	if start < 0 {
		return nil, false
	}
	end := strings.Index(definition[start+1:], ")")
	if end < 0 {
		return nil, false
	}
	fields := strings.Split(definition[start+1:start+1+end], ",")
	columns := make([]string, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field)
		if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
			name = strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
		}
		if name == "" || strings.ContainsAny(name, " ()") {
			return nil, false
		}
		columns = append(columns, name)
	}
	return columns, len(columns) > 0
}

func childTable(object pgschema.Object) pgschema.ID {
	switch object := object.(type) {
	case pgschema.Column:
		return object.Table
	case pgschema.Constraint:
		return object.Table
	case pgschema.Index:
		return object.Table
	case pgschema.Trigger:
		return object.Table
	case pgschema.Policy:
		return object.Table
	case pgschema.ReplicaIdentity:
		return object.Table
	case pgschema.RowSecurity:
		return object.Table
	case pgschema.TablePrivilege:
		return object.Table
	default:
		return pgschema.ID{}
	}
}

func equivalentExternalReferenceAfterTableRename(before, after pgschema.Constraint, to pgschema.Table) bool {
	reference := to.ObjectID()
	before.Reference = &reference
	return reflect.DeepEqual(before, after)
}

func equivalentChildForTableRename(before, after pgschema.Object, from, to pgschema.Table) bool {
	switch before := before.(type) {
	case pgschema.Column:
		next, ok := after.(pgschema.Column)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		before.NotNullConstraintName, next.NotNullConstraintName = "", ""
		return reflect.DeepEqual(before, next)
	case pgschema.Constraint:
		next, ok := after.(pgschema.Constraint)
		if !ok {
			return false
		}
		beforeObject, beforeDerived := normalizeDerivedTableChildName(before, from.Name)
		nextObject, nextDerived := normalizeDerivedTableChildName(next, to.Name)
		if beforeDerived != nextDerived {
			return false
		}
		before = beforeObject.(pgschema.Constraint)
		next = nextObject.(pgschema.Constraint)
		before.Table = to.ObjectID()
		if before.Reference != nil && *before.Reference == from.ObjectID() {
			reference := to.ObjectID()
			before.Reference = &reference
		}
		return reflect.DeepEqual(before, next)
	case pgschema.Index:
		next, ok := after.(pgschema.Index)
		if !ok {
			return false
		}
		beforeObject, beforeDerived := normalizeDerivedTableChildName(before, from.Name)
		nextObject, nextDerived := normalizeDerivedTableChildName(next, to.Name)
		if beforeDerived != nextDerived {
			return false
		}
		before = beforeObject.(pgschema.Index)
		next = nextObject.(pgschema.Index)
		before.Table = to.ObjectID()
		before.Definition = normalizeTableReference(before.Definition, from, to)
		return reflect.DeepEqual(before, next)
	case pgschema.Trigger:
		next, ok := after.(pgschema.Trigger)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		// pg_get_triggerdef's ON relation is the only trigger-definition text
		// PostgreSQL rewrites during ALTER TABLE ... RENAME. Normalize that one
		// catalog clause, never an arbitrary identifier inside the invoked
		// routine or a trigger WHEN expression.
		before.Definition = normalizeTriggerTableReference(before.Definition, from, to)
		return reflect.DeepEqual(before, next)
	case pgschema.Policy:
		next, ok := after.(pgschema.Policy)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		return reflect.DeepEqual(before, next)
	case pgschema.ReplicaIdentity:
		next, ok := after.(pgschema.ReplicaIdentity)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		if before.Index != nil {
			index := *before.Index
			index.Schema, index.Name = to.Schema, to.Name
			before.Index = &index
		}
		return reflect.DeepEqual(before, next)
	case pgschema.RowSecurity:
		next, ok := after.(pgschema.RowSecurity)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		return reflect.DeepEqual(before, next)
	case pgschema.TablePrivilege:
		next, ok := after.(pgschema.TablePrivilege)
		if !ok {
			return false
		}
		before.Table = to.ObjectID()
		return reflect.DeepEqual(before, next)
	default:
		return false
	}
}

func normalizeTriggerTableReference(definition string, from, to pgschema.Table) string {
	for _, pair := range [][2]string{
		{" ON " + qualified(from.Schema, from.Name) + " ", " ON " + qualified(to.Schema, to.Name) + " "},
		{" ON " + from.Schema + "." + from.Name + " ", " ON " + to.Schema + "." + to.Name + " "},
	} {
		if strings.Contains(definition, pair[0]) {
			return strings.Replace(definition, pair[0], pair[1], 1)
		}
	}
	return definition
}

func normalizeTableReference(definition string, from, to pgschema.Table) string {
	for _, pair := range [][2]string{
		{qualified(from.Schema, from.Name), qualified(to.Schema, to.Name)},
		{from.Schema + "." + from.Name, to.Schema + "." + to.Name},
		{quote(from.Name), quote(to.Name)},
	} {
		definition = strings.ReplaceAll(definition, pair[0], pair[1])
	}
	return definition
}

func tableRenameCompatibility(current, desired *pgschema.Snapshot, from, to pgschema.Table) ([]pgschema.Column, []pgschema.TablePrivilege, []string) {
	fromChildren := tableChildren(current, from.ObjectID())
	toChildren := tableChildren(desired, to.ObjectID())
	var columns []pgschema.Column
	var privileges []pgschema.TablePrivilege
	var unsupported []string
	sourceColumnCount := 0
	for key, object := range fromChildren {
		after, exists := toChildren[key]
		if !exists {
			unsupported = append(unsupported, "table_rename_compatibility_missing:"+object.ObjectID().String())
			continue
		}
		switch before := object.(type) {
		case pgschema.Column:
			sourceColumnCount++
			if !equivalentChildForTableRename(before, after, from, to) {
				unsupported = append(unsupported, "table_rename_compatibility_column_change:"+before.ObjectID().String())
				continue
			}
			columns = append(columns, before)
		case pgschema.TablePrivilege:
			if !equivalentChildForTableRename(before, after, from, to) {
				unsupported = append(unsupported, "table_rename_compatibility_privilege_change:"+before.ObjectID().String())
				continue
			}
			switch before.Privilege {
			case "SELECT", "INSERT", "UPDATE", "DELETE":
				privileges = append(privileges, before)
			default:
				unsupported = append(unsupported, "table_rename_compatibility_table_only_privilege:"+before.ObjectID().String())
			}
		}
	}
	if sourceColumnCount == 0 {
		unsupported = append(unsupported, "table_rename_compatibility_zero_columns:"+from.ObjectID().String())
	}
	sort.Slice(columns, func(i, j int) bool {
		if columns[i].Position != columns[j].Position {
			return columns[i].Position < columns[j].Position
		}
		return columns[i].Name < columns[j].Name
	})
	sort.Slice(privileges, func(i, j int) bool { return privileges[i].ObjectID().String() < privileges[j].ObjectID().String() })
	sort.Strings(unsupported)
	return columns, privileges, unsupported
}

func renderTableRename(rename renameTable) ([]protocol.Statement, error) {
	from := qualified(rename.from.Schema, rename.from.Name)
	to := qualified(rename.to.Schema, rename.to.Name)
	var statements []protocol.Statement
	columns := make([]string, len(rename.compatibilityColumns))
	for index, column := range rename.compatibilityColumns {
		columns[index] = quote(column.Name)
	}
	// Preserve the old physical table throughout the compatibility window. Old
	// application instances are the least able to accommodate a view: they may
	// use ON CONFLICT, table-only privileges, or table-shaped catalog metadata.
	// New code is introduced with knowledge of the temporary view contract.
	compatibilitySQL := "CREATE VIEW " + to + " WITH (security_invoker = true) AS\nSELECT " + strings.Join(columns, ", ") + " FROM " + from + ";"
	statements = append(statements, statement(compatibilitySQL, "expand", "review", true, "application_compatibility", "temporary_compatibility_view"))
	for _, privilege := range rename.compatibilityPrivileges {
		privilege.Table = rename.to.ObjectID()
		sql, unsupported := renderTablePrivilege(privilege, "GRANT")
		if unsupported != "" {
			return nil, fmt.Errorf("validated table-rename compatibility privilege became unrenderable: %s", unsupported)
		}
		statements = append(statements, statement(sql, "expand", "safe", true, "application_compatibility"))
	}
	statements = append(statements, statement("DROP VIEW "+to+";", "contract", "review", true, "compatibility_removal"))
	current := from
	if rename.from.Schema != rename.to.Schema {
		statements = append(statements, statement("ALTER TABLE "+from+" SET SCHEMA "+quote(rename.to.Schema)+";", "contract", "review", true, "application_compatibility", "compatibility_removal", "brief_lock"))
		current = qualified(rename.to.Schema, rename.from.Name)
	}
	if rename.from.Name != rename.to.Name {
		statements = append(statements, statement("ALTER TABLE "+current+" RENAME TO "+quote(rename.to.Name)+";", "contract", "review", true, "application_compatibility", "compatibility_removal", "brief_lock"))
	}
	for _, constraintRename := range rename.notNullConstraintRenames {
		statements = append(statements, statement(
			"ALTER TABLE "+to+" RENAME CONSTRAINT "+quote(constraintRename.from)+" TO "+quote(constraintRename.to)+";",
			"contract", "review", true, "derived_catalog_name", "brief_lock",
		))
	}
	for _, childRename := range rename.derivedChildRenames {
		childStatements, err := renderDerivedTableChildRename(childRename, rename.to)
		if err != nil {
			return nil, err
		}
		statements = append(statements, childStatements...)
	}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON TABLE "+to+" IS "+value+";", "contract", "safe", true))
	}
	return statements, nil
}

func renderDerivedTableChildRename(rename tableChildRename, table pgschema.Table) ([]protocol.Statement, error) {
	switch before := rename.from.(type) {
	case pgschema.Constraint:
		after, ok := rename.to.(pgschema.Constraint)
		if !ok {
			return nil, fmt.Errorf("derived table child changed kind from constraint")
		}
		return []protocol.Statement{statement("ALTER TABLE "+qualified(table.Schema, table.Name)+" RENAME CONSTRAINT "+quote(before.Name)+" TO "+quote(after.Name)+";", "contract", "review", true, "derived_catalog_name", "brief_lock")}, nil
	case pgschema.Index:
		after, ok := rename.to.(pgschema.Index)
		if !ok {
			return nil, fmt.Errorf("derived table child changed kind from index")
		}
		// A constraint-backed index is renamed by ALTER TABLE .. RENAME
		// CONSTRAINT above. Rendering it again would fail; the constraint is the
		// source of the relation's public name in that case.
		if before.Constraint != "" {
			return nil, nil
		}
		return []protocol.Statement{statement("ALTER INDEX "+qualified(table.Schema, before.Name)+" RENAME TO "+quote(after.Name)+";", "contract", "review", true, "derived_catalog_name", "brief_lock")}, nil
	default:
		return nil, fmt.Errorf("unsupported derived table child kind %T", rename.from)
	}
}

func resolveIndexRenames(changes []change.Change, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameIndex, []protocol.Question, error) {
	var drops, creates []change.Change
	for _, item := range changes {
		if item.ID.Kind != pgschema.KindIndex {
			continue
		}
		if item.Kind == change.Drop {
			drops = append(drops, item)
		}
		if item.Kind == change.Create {
			creates = append(creates, item)
		}
	}
	consumed := make(map[pgschema.ID]bool)
	var renames []renameIndex
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Index)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Index)
			if from.Table == to.Table && equivalentIndexForRename(from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		to := candidate.After.(pgschema.Index)
		question := protocol.Question{
			ID: "rename_index:" + drop.ID.String(), Kind: "rename_index", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		answer, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			renames = append(renames, renameIndex{from: from, to: to})
		}
	}
	if len(consumed) == 0 {
		return changes, renames, questions, nil
	}
	filtered := make([]change.Change, 0, len(changes)-len(consumed))
	for _, item := range changes {
		if !consumed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, renames, questions, nil
}

func equivalentIndexForRename(from, to pgschema.Index) bool {
	from.Name, to.Name = "", ""
	from.Definition, to.Definition = "", ""
	from.Comment, to.Comment = nil, nil
	return reflect.DeepEqual(from, to)
}

func renderIndexRename(rename renameIndex) []protocol.Statement {
	sql := "ALTER INDEX " + qualified(rename.from.Table.Schema, rename.from.Name) + " RENAME TO " + quote(rename.to.Name) + ";"
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "index_identity", "brief_lock")}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON INDEX "+qualified(rename.to.Table.Schema, rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func expandContractViolations(statements []protocol.Statement) []string {
	var unsupported []string
	for _, item := range statements {
		if item.Manual == nil && stringIn(item.Hazards, "application_contract") && !stringIn(item.Hazards, "compatibility_removal") {
			unsupported = append(unsupported, "expand_contract_bridge_required:"+item.ID)
			continue
		}
		upper := strings.ToUpper(item.SQL)
		if item.Manual == nil && strings.Contains(upper, " ALTER COLUMN ") && strings.Contains(upper, " TYPE ") && !stringIn(item.Hazards, "enum_rewrite") && !stringIn(item.Hazards, "column_collation_change") && !stringIn(item.Hazards, "generated_type_change") {
			unsupported = append(unsupported, "expand_contract_type_bridge_required:"+item.ID)
			continue
		}
	}
	sort.Strings(unsupported)
	return unsupported
}

func stringIn(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func renderChange(item change.Change, current, desired *pgschema.Snapshot, createdTables map[pgschema.ID]bool, options Options, choices decisions) ([]protocol.Statement, []pgschema.ID, []string, error) {
	switch item.Kind {
	case change.Create:
		if constraint, ok := item.After.(pgschema.Constraint); ok {
			statements, consumed, unsupported, err := renderConstraintCreateUsingExistingIndex(constraint, current, desired)
			if !createdTables[constraint.Table] {
				statements = withPhase(statements, protocol.PhaseContract, "review", "compatible_writers_required")
			}
			return statements, consumed, unsupported, err
		}
		return renderCreate(item.After, desired, createdTables, options)
	case change.Drop:
		if routine, ok := item.Before.(pgschema.Routine); ok && routineDropCircular(routine, current, desired) {
			return nil, nil, []string{"routine_drop_circular_dependency:" + routine.ObjectID().String()}, nil
		}
		return renderDropWithOptions(item.Before, options), nil, nil, nil
	case change.Modify:
		return renderModify(item.Before, item.After, choices, options, current, desired)
	default:
		return nil, nil, nil, fmt.Errorf("unknown change kind %q", item.Kind)
	}
}

func routineDropCircular(routine pgschema.Routine, current, desired *pgschema.Snapshot) bool {
	if current == nil || desired == nil {
		return false
	}
	droppedRelationDependency := false
	for _, dependency := range current.Dependencies(routine.ObjectID()) {
		if dependency.Kind != pgschema.KindTable && dependency.Kind != pgschema.KindView && dependency.Kind != pgschema.KindMatView {
			continue
		}
		if _, retained := desired.Object(dependency); !retained {
			droppedRelationDependency = true
			break
		}
	}
	if !droppedRelationDependency {
		return false
	}
	for _, id := range current.IDs() {
		if id.Kind != pgschema.KindTable || !containsID(current.Dependencies(id), routine.ObjectID()) {
			continue
		}
		if _, retained := desired.Object(id); !retained {
			return true
		}
	}
	return false
}

func renderCreate(object pgschema.Object, desired *pgschema.Snapshot, createdTables map[pgschema.ID]bool, options Options) ([]protocol.Statement, []pgschema.ID, []string, error) {
	switch object := object.(type) {
	case pgschema.Schema:
		prefix := "CREATE SCHEMA "
		if options.IfNotExists || object.Name == "public" {
			prefix += "IF NOT EXISTS "
		}
		statements := []protocol.Statement{statement(prefix+quote(object.Name)+";", "expand", "safe", true)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON SCHEMA "+quote(object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Enum:
		labels := make([]string, len(object.Labels))
		for i, label := range object.Labels {
			labels[i] = literal(label)
		}
		statements := []protocol.Statement{statement("CREATE TYPE "+qualified(object.Schema, object.Name)+" AS ENUM ("+strings.Join(labels, ", ")+");", "expand", "safe", true)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON TYPE "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Domain:
		if object.Schema == "" || object.Name == "" || strings.TrimSpace(object.BaseType) == "" {
			return nil, nil, []string{"domain_create:" + object.ObjectID().String()}, nil
		}
		sql := "CREATE DOMAIN " + qualified(object.Schema, object.Name) + " AS " + object.BaseType
		if object.Collation != "" {
			sql += " COLLATE " + object.Collation
		}
		if object.Default != nil {
			sql += " DEFAULT " + *object.Default
		}
		if object.NotNull {
			sql += " NOT NULL"
		}
		statements := []protocol.Statement{statement(sql+";", "expand", "safe", true)}
		for _, constraint := range object.Constraints {
			created, unsupported := renderDomainConstraintAdd(object, constraint)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements, statement(created, "expand", "review", true, "domain_constraint_added"))
		}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON DOMAIN "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Composite:
		attributes := make([]string, 0, len(object.Attributes))
		for _, attribute := range object.Attributes {
			if attribute.Name == "" || strings.TrimSpace(attribute.Type) == "" {
				return nil, nil, []string{"composite_attribute:" + object.ObjectID().String()}, nil
			}
			definition := quote(attribute.Name) + " " + attribute.Type
			if attribute.Collation != "" {
				definition += " COLLATE " + attribute.Collation
			}
			attributes = append(attributes, definition)
		}
		statements := []protocol.Statement{statement("CREATE TYPE "+qualified(object.Schema, object.Name)+" AS ("+strings.Join(attributes, ", ")+");", "expand", "safe", true)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON TYPE "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Range:
		sql, unsupported := renderRangeCreate(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "expand", "safe", true)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON TYPE "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Sequence:
		parts := []string{
			"AS " + object.Type,
			"START WITH " + fmt.Sprint(object.Start),
			"INCREMENT BY " + fmt.Sprint(object.Increment),
			"MINVALUE " + fmt.Sprint(object.Min),
			"MAXVALUE " + fmt.Sprint(object.Max),
			"CACHE " + fmt.Sprint(object.Cache),
		}
		if object.Cycle {
			parts = append(parts, "CYCLE")
		} else {
			parts = append(parts, "NO CYCLE")
		}
		if object.OwnedBy != nil {
			ownedBy, unsupported := renderSequenceOwnedBy(object)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			parts = append(parts, ownedBy)
		}
		prefix := "CREATE "
		if object.Unlogged {
			prefix += "UNLOGGED "
		}
		statements := []protocol.Statement{statement(prefix+"SEQUENCE "+qualified(object.Schema, object.Name)+" "+strings.Join(parts, " ")+";", "expand", "safe", true)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON SEQUENCE "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Table:
		columns := tableColumns(desired, object.ObjectID())
		definitions := make([]string, 0, len(columns))
		consumed := make([]pgschema.ID, 0, len(columns))
		for _, column := range columns {
			if !serialColumnSupported(column) {
				return nil, nil, []string{"serial_sequence_name:" + column.ObjectID().String()}, nil
			}
			definitions = append(definitions, renderColumn(column))
			consumed = append(consumed, column.ObjectID())
		}
		prefix := "CREATE "
		if object.Unlogged {
			prefix += "UNLOGGED "
		}
		prefix += "TABLE "
		if options.IfNotExists {
			prefix += "IF NOT EXISTS "
		}
		sql := prefix + qualified(object.Schema, object.Name) + " (" + strings.Join(definitions, ", ") + ")"
		if object.PartitionOf != nil {
			if object.PartitionOf.Bound == "" {
				return nil, nil, nil, fmt.Errorf("partition child %s has no canonical bound", object.ObjectID())
			}
			sql = prefix + qualified(object.Schema, object.Name) + " PARTITION OF " + qualified(object.PartitionOf.Parent.Schema, object.PartitionOf.Parent.Name) + " " + object.PartitionOf.Bound
			if object.Partition != nil {
				if object.Partition.Raw == "" {
					return nil, nil, nil, fmt.Errorf("partitioned table %s has no canonical partition key", object.ObjectID())
				}
				sql += " PARTITION BY " + object.Partition.Raw
			}
		} else if object.Partition != nil {
			if object.Partition.Raw == "" {
				return nil, nil, nil, fmt.Errorf("partitioned table %s has no canonical partition key", object.ObjectID())
			}
			sql += " PARTITION BY " + object.Partition.Raw
		}
		statements := []protocol.Statement{statement(sql+";", "expand", "review", true)}
		if object.Owner != "" {
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(object.Schema, object.Name)+" OWNER TO "+quote(object.Owner)+";", "expand", "dangerous", "table_owner_change", "authorization_change"))
		}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON TABLE "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		for _, column := range columns {
			if column.Comment != nil {
				statements = append(statements, statement("COMMENT ON COLUMN "+qualified(object.Schema, object.Name)+"."+quote(column.Name)+" IS "+literal(*column.Comment)+";", "expand", "safe", true))
			}
		}
		return statements, consumed, nil, nil
	case pgschema.Column:
		if createdTables[object.Table] {
			return nil, nil, nil, nil
		}
		if !serialColumnSupported(object) {
			return nil, nil, []string{"serial_sequence_name:" + object.ObjectID().String()}, nil
		}
		if object.NotNull && object.Default == nil && object.Identity == nil && object.Serial == nil && object.Generated == nil {
			expandColumn := object
			expandColumn.NotNull = false
			table := qualified(object.Table.Schema, object.Table.Name)
			column := quote(object.Name)
			statements := []protocol.Statement{statement(
				"ALTER TABLE "+table+" ADD COLUMN "+renderColumn(expandColumn)+";",
				"expand", "review", true, "table_lock", "application_compatibility",
			)}
			if object.Comment != nil {
				statements = append(statements, statement("COMMENT ON COLUMN "+table+"."+column+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
			}
			statements = append(statements,
				statement(
					"-- ONWARDPG TODO: deploy code that writes "+object.ObjectID().String()+", then replace this comment with a reviewed backfill for existing rows.\n"+
						"-- Expected effect: every row has a product-correct value before the NOT NULL contract runs.\n"+
						"-- Add a boolean assertion to verify.sql proving no NULL values remain.",
					"contract", "manual", true, "manual_sql", "data_movement", "post_drain_backfill_required",
				),
				statement(
					"ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;",
					"contract", "review", true, "table_scan", "access_exclusive_lock", "compatibility_removal",
				),
			)
			return statements, nil, nil, nil
		}
		sql := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD COLUMN " + renderColumn(object) + ";"
		hazards := []string{"table_lock"}
		if object.Default != nil || object.Identity != nil || object.Serial != nil || object.Generated != nil {
			hazards = append(hazards, "table_rewrite_possible")
		}
		if object.NotNull {
			hazards = append(hazards, "validation_scan_possible")
		}
		statements := []protocol.Statement{statement(sql, "expand", "review", true, hazards...)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON COLUMN "+qualified(object.Table.Schema, object.Table.Name)+"."+quote(object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Constraint:
		if object.Parent != nil || constraintPropagatedByPartitionParent(object, desired) {
			// PostgreSQL creates/drops this child as part of the partitioned
			// parent constraint operation. Never emit a duplicate child DDL.
			return nil, nil, nil, nil
		}
		if constraintUsesMatchPartial(object) {
			return nil, nil, []string{"foreign_key_match_partial:" + object.ObjectID().String()}, nil
		}
		sql := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD CONSTRAINT " + quote(object.Name) + " " + object.Definition + ";"
		phase := "contract"
		hazards := []string{"table_lock", "validation_scan_possible", "compatible_writers_required"}
		if createdTables[object.Table] {
			phase = "expand"
			hazards = []string{"table_lock"}
		}
		return []protocol.Statement{authorizationStatement(sql, phase, "review", hazards...)}, nil, nil, nil
	case pgschema.Index:
		if object.Parent != nil {
			// PostgreSQL propagates this index from the partitioned parent.
			return nil, nil, nil, nil
		}
		if object.Constraint != "" {
			// PostgreSQL creates the backing index as part of ADD CONSTRAINT.
			return nil, nil, nil, nil
		}
		sql, unsupported := renderIndex(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		transactional := true
		if options.ConcurrentIndexes && !object.Primary && object.Constraint == "" {
			sql = strings.Replace(sql, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
			sql = strings.Replace(sql, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1)
			transactional = false
		}
		phase := "expand"
		hazards := []string{"index_build", "table_lock_possible"}
		if object.Unique && !createdTables[object.Table] {
			phase = "contract"
			hazards = append(hazards, "compatible_writers_required")
		}
		statements := []protocol.Statement{statement(strings.TrimSuffix(sql, ";")+";", phase, "review", transactional, hazards...)}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON INDEX "+qualified(object.Table.Schema, object.Name)+" IS "+literal(*object.Comment)+";", phase, "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Extension:
		if object.Name == "" || object.Schema == "" || object.Version == "" {
			return nil, nil, []string{"extension_create:" + object.ObjectID().String()}, nil
		}
		sql := "CREATE EXTENSION " + quote(object.Name) + " WITH SCHEMA " + quote(object.Schema) + " VERSION " + literal(object.Version) + ";"
		statements := []protocol.Statement{statement(sql, "expand", "review", true, "extension_install")}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON EXTENSION "+quote(object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.View:
		sql, unsupported := renderViewCreate(object, false)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "expand", "review", true)}
		if object.Comment != nil {
			kind := "VIEW"
			if object.Materialized {
				kind = "MATERIALIZED VIEW"
			}
			statements = append(statements, statement("COMMENT ON "+kind+" "+qualified(object.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		if object.Owner != "" {
			owner, ownerUnsupported := renderRole(object.Owner, object.ObjectID())
			if ownerUnsupported != "" {
				return nil, nil, []string{ownerUnsupported}, nil
			}
			kind := "VIEW"
			if object.Materialized {
				kind = "MATERIALIZED VIEW"
			}
			statements = append(statements, authorizationStatement("ALTER "+kind+" "+qualified(object.Schema, object.Name)+" OWNER TO "+owner+";", "expand", "review", "relation_owner_preserved", "authorization_change"))
		}
		return statements, nil, nil, nil
	case pgschema.Routine:
		sql, unsupported := renderRoutineDefinition(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "expand", "review", true, "routine_definition")}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON "+strings.ToUpper(object.Kind)+" "+qualified(object.Schema, object.Name)+"("+object.Signature+") IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Trigger:
		sql, unsupported := renderTriggerDefinition(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "expand", "review", true, "trigger_create", "table_lock")}
		if object.Enabled != "O" {
			enabled, unsupported := renderTriggerEnabled(object)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements, statement(enabled, "expand", "review", true, "trigger_enable_state", "table_lock"))
		}
		if object.Comment != nil {
			identifier := quote(object.Name) + " ON " + qualified(object.Table.Schema, object.Table.Name)
			statements = append(statements, statement("COMMENT ON TRIGGER "+identifier+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Policy:
		sql, unsupported := renderPolicyCreate(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		hazards := []string{"row_security_policy_change", "authorization_change"}
		if object.Permissive {
			hazards = append(hazards, "permissive_policy_added")
		} else {
			hazards = append(hazards, "restrictive_policy_added", "availability_risk")
		}
		return []protocol.Statement{statement(sql, "expand", "review", true, hazards...)}, nil, nil, nil
	case pgschema.ReplicaIdentity:
		if object.Mode == pgschema.ReplicaIdentityDefault && object.Index == nil {
			return nil, nil, nil, nil
		}
		sql, unsupported := renderReplicaIdentity(object)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		if sql == "" {
			return nil, nil, nil, nil
		}
		return []protocol.Statement{statement(sql, "contract", "review", true, "replica_identity_change", "table_lock")}, nil, nil, nil
	case pgschema.RowSecurity:
		if object.Table.Kind != pgschema.KindTable || object.Table.Schema == "" || object.Table.Name == "" || (!object.Enabled && !object.Forced) {
			return nil, nil, []string{"row_security_render:" + object.ObjectID().String()}, nil
		}
		var statements []protocol.Statement
		if object.Enabled {
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" ENABLE ROW LEVEL SECURITY;", "expand", "review", "row_security_enabled", "authorization_change", "availability_risk", "table_lock"))
		}
		if object.Forced {
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" FORCE ROW LEVEL SECURITY;", "expand", "review", "row_security_forced", "authorization_change", "availability_risk", "table_lock"))
		}
		return statements, nil, nil, nil
	case pgschema.TablePrivilege:
		sql, unsupported := renderTablePrivilege(object, "GRANT")
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		if grantor := object.Grantor; grantor != "@owner" {
			renderedGrantor, grantorUnsupported := renderRole(grantor, object.ObjectID())
			if grantorUnsupported != "" {
				return nil, nil, []string{grantorUnsupported}, nil
			}
			sql = "SET LOCAL ROLE " + renderedGrantor + ";\n" + sql + "\nRESET ROLE;"
		}
		return []protocol.Statement{authorizationStatement(sql, "expand", "review", "privilege_granted", "authorization_change")}, nil, nil, nil
	default:
		return nil, nil, []string{"create:" + object.ObjectID().String()}, nil
	}
}

// constraintPropagatedByPartitionParent covers the catalog shape produced
// when a partition is created after its parent's primary/unique constraint.
// PostgreSQL may expose that child constraint without a conparentid, but the
// typed table parent plus an identical parent constraint proves that it is not
// independently creatable. This is a catalog relationship, not a name-based
// heuristic.
func constraintPropagatedByPartitionParent(object pgschema.Constraint, desired *pgschema.Snapshot) bool {
	if desired == nil {
		return false
	}
	tableObject, exists := desired.Object(object.Table)
	if !exists {
		return false
	}
	table, ok := tableObject.(pgschema.Table)
	if !ok || table.PartitionOf == nil {
		return false
	}
	if object.Parent != nil {
		parentObject, exists := desired.Object(*object.Parent)
		parent, ok := parentObject.(pgschema.Constraint)
		return exists && ok && parent.Table == table.PartitionOf.Parent &&
			parent.Type == object.Type && parent.Definition == object.Definition
	}
	if object.Type != pgschema.ConstraintPrimary && object.Type != pgschema.ConstraintUnique {
		return false
	}
	for _, candidate := range desired.Objects() {
		parent, ok := candidate.(pgschema.Constraint)
		if !ok || parent.Table != table.PartitionOf.Parent || parent.Type != object.Type || parent.Definition != object.Definition {
			continue
		}
		return true
	}
	return false
}

// renderIndex deliberately uses the typed index payload rather than the
// pg_get_indexdef compatibility field. This keeps constructed snapshots portable
// and makes an unrepresented attribute an explicit unsupported result instead
// of silently emitting a different index.
func renderIndex(index pgschema.Index) (string, string) {
	if index.Name == "" || (index.Table.Kind != pgschema.KindTable && index.Table.Kind != pgschema.KindMatView) || index.Table.Schema == "" || index.Table.Name == "" || index.Method == "" || len(index.Parts) == 0 {
		return "", "index_render:" + index.ObjectID().String()
	}
	parts := make([]string, 0, len(index.Parts))
	for _, part := range index.Parts {
		if part.Column != "" && part.Expression != "" {
			return "", "index_part_ambiguous:" + index.ObjectID().String()
		}
		var rendered string
		switch {
		case part.Column != "":
			rendered = quote(part.Column)
		case part.Expression != "":
			// Expressions are catalog/declarative SQL, not identifiers. Keeping
			// them parenthesized prevents precedence changes around operators.
			rendered = "(" + part.Expression + ")"
		default:
			return "", "index_part_missing:" + index.ObjectID().String()
		}
		if part.Collation != "" {
			rendered += " COLLATE " + part.Collation
		}
		if part.OpClass != nil && (!part.OpClass.IsDefault || len(part.OpClass.Parameters) > 0) {
			if part.OpClass.Name == "" {
				return "", "index_opclass_missing:" + index.ObjectID().String()
			}
			rendered += " " + quoteQualifiedName(part.OpClass.Name)
			if len(part.OpClass.Parameters) > 0 {
				parameters, unsupported := renderIndexOptions(part.OpClass.Parameters, index.ObjectID())
				if unsupported != "" {
					return "", unsupported
				}
				rendered += " (" + parameters + ")"
			}
		}
		if part.Descending {
			rendered += " DESC"
		}
		if part.NullsFirst && part.NullsLast {
			return "", "index_null_order_invalid:" + index.ObjectID().String()
		}
		if part.NullsFirst {
			rendered += " NULLS FIRST"
		} else if part.NullsLast {
			rendered += " NULLS LAST"
		}
		parts = append(parts, rendered)
	}
	sql := "CREATE"
	if index.Unique {
		sql += " UNIQUE"
	}
	sql += " INDEX " + quote(index.Name) + " ON " + qualified(index.Table.Schema, index.Table.Name) + " USING " + quote(index.Method) + " (" + strings.Join(parts, ", ") + ")"
	if len(index.Include) > 0 {
		included := make([]string, len(index.Include))
		for i, column := range index.Include {
			if column == "" {
				return "", "index_include_missing:" + index.ObjectID().String()
			}
			included[i] = quote(column)
		}
		sql += " INCLUDE (" + strings.Join(included, ", ") + ")"
	}
	if index.NullsNotDistinct {
		if !index.Unique {
			return "", "index_nulls_not_distinct_non_unique:" + index.ObjectID().String()
		}
		sql += " NULLS NOT DISTINCT"
	}
	if len(index.Storage.Options) > 0 {
		options, unsupported := renderIndexOptions(index.Storage.Options, index.ObjectID())
		if unsupported != "" {
			return "", unsupported
		}
		sql += " WITH (" + options + ")"
	}
	if index.Predicate != "" {
		sql += " WHERE " + index.Predicate
	}
	return sql, ""
}

// replacementIndexName is stable for the complete old/new typed index pair.
// The hash-only suffix avoids byte-truncation bugs for quoted Unicode names
// while staying comfortably below PostgreSQL's 63-byte identifier limit.
func replacementIndexName(before, after pgschema.Index) (string, error) {
	before.Definition, after.Definition = "", ""
	encoded, err := json.Marshal(struct {
		Before pgschema.Index `json:"before"`
		After  pgschema.Index `json:"after"`
	}{Before: before, After: after})
	if err != nil {
		return "", fmt.Errorf("fingerprint replacement index %s: %w", after.ObjectID(), err)
	}
	digest := sha256.Sum256(encoded)
	return "onwardpg_tmpidx_" + hex.EncodeToString(digest[:8]), nil
}

// PostgreSQL indexes share the pg_class relation-name namespace with tables,
// sequences, views, and materialized views. Checking only typed index IDs
// would permit a temporary name that CREATE/ALTER INDEX cannot actually use.
func relationNameExists(snapshot *pgschema.Snapshot, schema, name string) bool {
	for _, object := range snapshot.Objects() {
		switch object := object.(type) {
		case pgschema.Table:
			if object.Schema == schema && object.Name == name {
				return true
			}
		case pgschema.Sequence:
			if object.Schema == schema && object.Name == name {
				return true
			}
		case pgschema.View:
			if object.Schema == schema && object.Name == name {
				return true
			}
		case pgschema.Index:
			if object.Table.Schema == schema && object.Name == name {
				return true
			}
		}
	}
	return false
}

func renderSequenceOwnedBy(sequence pgschema.Sequence) (string, string) {
	if sequence.OwnedBy == nil {
		return "", ""
	}
	ownedBy := *sequence.OwnedBy
	if ownedBy.Kind != pgschema.KindColumn || ownedBy.Schema == "" || ownedBy.Name == "" || ownedBy.Part == "" {
		return "", "sequence_owned_by_invalid:" + sequence.ObjectID().String()
	}
	return "OWNED BY " + qualified(ownedBy.Schema, ownedBy.Name) + "." + quote(ownedBy.Part), ""
}

func renderViewCreate(view pgschema.View, replace bool) (string, string) {
	if view.Schema == "" || view.Name == "" || strings.TrimSpace(view.Definition) == "" {
		return "", "view_render:" + view.ObjectID().String()
	}
	if replace && view.Materialized {
		return "", "materialized_view_rebuild:" + view.ObjectID().String()
	}
	options, unsupported := renderViewOptions(view.Options, view.ObjectID())
	if unsupported != "" {
		return "", unsupported
	}
	prefix := "CREATE "
	if replace {
		prefix += "OR REPLACE "
	}
	if view.Materialized {
		prefix += "MATERIALIZED "
	}
	prefix += "VIEW " + qualified(view.Schema, view.Name)
	if options != "" {
		prefix += " WITH (" + options + ")"
	}
	definition := strings.TrimSuffix(strings.TrimSpace(view.Definition), ";")
	sql := prefix + " AS " + definition
	if view.Materialized {
		if view.Populated {
			sql += " WITH DATA"
		} else {
			sql += " WITH NO DATA"
		}
	}
	return sql + ";", ""
}

func renderRoutineDefinition(routine pgschema.Routine) (string, string) {
	if routine.Schema == "" || routine.Name == "" || (routine.Kind != "function" && routine.Kind != "procedure") {
		return "", "routine_render:" + routine.ObjectID().String()
	}
	definition := strings.TrimSpace(routine.Definition)
	upper := strings.ToUpper(definition)
	if !strings.HasPrefix(upper, "CREATE OR REPLACE FUNCTION ") && !strings.HasPrefix(upper, "CREATE OR REPLACE PROCEDURE ") {
		return "", "routine_definition_unrecognized:" + routine.ObjectID().String()
	}
	return strings.TrimSuffix(definition, ";") + ";", ""
}

func renderRoutineReturnTypeChange(before, after pgschema.Routine, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if before.Kind != "function" || after.Kind != "function" || before.ReturnType == "" || after.ReturnType == "" {
		return nil, nil, []string{"routine_return_type_change:" + after.ObjectID().String()}, nil
	}
	created, unsupported := renderRoutineDefinition(after)
	if unsupported != "" {
		return nil, nil, []string{unsupported}, nil
	}
	type dependent struct {
		id     pgschema.ID
		before pgschema.Object
		after  pgschema.Object
	}
	var dependents []dependent
	seen := make(map[pgschema.ID]bool)
	for _, id := range current.IDs() {
		if id == before.ObjectID() || !containsID(current.Dependencies(id), before.ObjectID()) {
			continue
		}
		object, _ := current.Object(id)
		switch object.(type) {
		case pgschema.Column, pgschema.Constraint, pgschema.Index:
		case pgschema.Table:
			// Tables carry their column-default routine dependencies so a
			// parent DROP TABLE is scheduled before DROP FUNCTION.
			continue
		default:
			return nil, nil, []string{"routine_return_type_dependent:" + id.String()}, nil
		}
		next, exists := desired.Object(id)
		if !exists || !routineDependentEquivalent(object, next) || !containsID(desired.Dependencies(id), after.ObjectID()) {
			return nil, nil, []string{"routine_return_type_changed_dependent:" + id.String()}, nil
		}
		dependents = append(dependents, dependent{id: id, before: object, after: next})
		seen[id] = true
	}
	for _, id := range desired.IDs() {
		if id == after.ObjectID() || !containsID(desired.Dependencies(id), after.ObjectID()) || seen[id] {
			continue
		}
		if object, _ := desired.Object(id); object != nil {
			if _, ok := object.(pgschema.Table); ok {
				continue
			}
		}
		return nil, nil, []string{"routine_return_type_new_dependent:" + id.String()}, nil
	}
	sort.Slice(dependents, func(i, j int) bool { return dependents[i].id.String() < dependents[j].id.String() })
	var dropDefaults, dropConstraints, dropIndexes, addConstraints, addIndexes, addDefaults []protocol.Statement
	for _, item := range dependents {
		switch object := item.after.(type) {
		case pgschema.Column:
			if object.Default == nil {
				return nil, nil, []string{"routine_return_type_default_missing:" + item.id.String()}, nil
			}
			prefix := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ALTER COLUMN " + quote(object.Name)
			dropDefaults = append(dropDefaults, statement(prefix+" DROP DEFAULT;", "contract", "review", true, "routine_return_type_change", "table_lock"))
			addDefaults = append(addDefaults, statement(prefix+" SET DEFAULT "+*object.Default+";", "contract", "review", true, "routine_return_type_change", "table_lock"))
		case pgschema.Constraint:
			if object.Parent != nil {
				return nil, nil, []string{"routine_return_type_partition_constraint:" + item.id.String()}, nil
			}
			dropConstraints = append(dropConstraints, statement("ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" DROP CONSTRAINT "+quote(object.Name)+";", "contract", "review", true, "routine_return_type_change", "table_lock"))
			created := withPhase(renderConstraintCreate(object), "contract", "review", "routine_return_type_change")
			addConstraints = append(addConstraints, created...)
			if object.Comment != nil {
				addConstraints = append(addConstraints, statement("COMMENT ON CONSTRAINT "+quote(object.Name)+" ON "+qualified(object.Table.Schema, object.Table.Name)+" IS "+literal(*object.Comment)+";", "contract", "safe", true, "routine_return_type_change"))
			}
		case pgschema.Index:
			if object.Parent != nil || object.Constraint != "" {
				return nil, nil, []string{"routine_return_type_index_shape:" + item.id.String()}, nil
			}
			dropIndexes = append(dropIndexes, statement("DROP INDEX "+qualified(object.Table.Schema, object.Name)+";", "contract", "review", true, "routine_return_type_change", "table_lock"))
			sql, unsupported := renderIndex(object)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			addIndexes = append(addIndexes, statement(sql, "contract", "review", true, "routine_return_type_change", "index_build", "table_lock_possible"))
			if object.Comment != nil {
				addIndexes = append(addIndexes, statement("COMMENT ON INDEX "+qualified(object.Table.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "contract", "safe", true, "routine_return_type_change"))
			}
		}
	}
	statements := append(dropDefaults, dropConstraints...)
	statements = append(statements, dropIndexes...)
	statements = append(statements,
		statement("DROP FUNCTION "+qualified(before.Schema, before.Name)+"("+before.Signature+");", "contract", "review", true, "routine_return_type_change", "application_compatibility"),
		statement(created, "contract", "review", true, "routine_return_type_change", "routine_behavior_change", "application_compatibility"),
	)
	if after.Comment != nil {
		statements = append(statements, statement("COMMENT ON FUNCTION "+qualified(after.Schema, after.Name)+"("+after.Signature+") IS "+literal(*after.Comment)+";", "contract", "safe", true, "routine_return_type_change"))
	}
	statements = append(statements, addConstraints...)
	statements = append(statements, addIndexes...)
	statements = append(statements, addDefaults...)
	return statements, nil, nil, nil
}

func routineDependentEquivalent(before, after pgschema.Object) bool {
	beforeIndex, beforeOK := before.(pgschema.Index)
	afterIndex, afterOK := after.(pgschema.Index)
	if beforeOK || afterOK {
		if !beforeOK || !afterOK {
			return false
		}
		beforeIndex.Definition, afterIndex.Definition = "", ""
		for index := range beforeIndex.Parts {
			if beforeIndex.Parts[index].OpClass != nil && beforeIndex.Parts[index].OpClass.IsDefault && len(beforeIndex.Parts[index].OpClass.Parameters) == 0 {
				beforeIndex.Parts[index].OpClass = nil
			}
		}
		for index := range afterIndex.Parts {
			if afterIndex.Parts[index].OpClass != nil && afterIndex.Parts[index].OpClass.IsDefault && len(afterIndex.Parts[index].OpClass.Parameters) == 0 {
				afterIndex.Parts[index].OpClass = nil
			}
		}
		return reflect.DeepEqual(beforeIndex, afterIndex)
	}
	return reflect.DeepEqual(before, after)
}

func renderTriggerDefinition(trigger pgschema.Trigger) (string, string) {
	if (trigger.Table.Kind != pgschema.KindTable && trigger.Table.Kind != pgschema.KindView) || trigger.Table.Schema == "" || trigger.Table.Name == "" || trigger.Name == "" {
		return "", "trigger_render:" + trigger.ObjectID().String()
	}
	definition := strings.TrimSpace(trigger.Definition)
	upper := strings.ToUpper(definition)
	if !strings.HasPrefix(upper, "CREATE TRIGGER ") && !strings.HasPrefix(upper, "CREATE CONSTRAINT TRIGGER ") {
		return "", "trigger_definition_unrecognized:" + trigger.ObjectID().String()
	}
	return strings.TrimSuffix(definition, ";") + ";", ""
}

func renderTriggerEnabled(trigger pgschema.Trigger) (string, string) {
	mode := ""
	switch trigger.Enabled {
	case "O":
		mode = "ENABLE"
	case "D":
		mode = "DISABLE"
	case "R":
		mode = "ENABLE REPLICA"
	case "A":
		mode = "ENABLE ALWAYS"
	default:
		return "", "trigger_enabled_invalid:" + trigger.ObjectID().String()
	}
	return "ALTER TABLE " + qualified(trigger.Table.Schema, trigger.Table.Name) + " " + mode + " TRIGGER " + quote(trigger.Name) + ";", ""
}

func renderReplicaIdentity(identity pgschema.ReplicaIdentity) (string, string) {
	if identity.Table.Kind != pgschema.KindTable || identity.Table.Schema == "" || identity.Table.Name == "" {
		return "", "replica_identity_table:" + identity.ObjectID().String()
	}
	clause := string(identity.Mode)
	switch identity.Mode {
	case pgschema.ReplicaIdentityDefault, pgschema.ReplicaIdentityFull, pgschema.ReplicaIdentityNothing:
		if identity.Index != nil {
			return "", "replica_identity_unexpected_index:" + identity.ObjectID().String()
		}
	case pgschema.ReplicaIdentityIndex:
		if identity.Index == nil || identity.Index.Kind != pgschema.KindIndex || identity.Index.Schema != identity.Table.Schema || identity.Index.Name != identity.Table.Name || identity.Index.Part == "" {
			return "", "replica_identity_index:" + identity.ObjectID().String()
		}
		clause = "USING INDEX " + quote(identity.Index.Part)
	default:
		return "", "replica_identity_mode:" + identity.ObjectID().String()
	}
	return "ALTER TABLE " + qualified(identity.Table.Schema, identity.Table.Name) + " REPLICA IDENTITY " + clause + ";", ""
}

func renderPolicyCreate(policy pgschema.Policy) (string, string) {
	if policy.Table.Kind != pgschema.KindTable || policy.Table.Schema == "" || policy.Table.Name == "" || policy.Name == "" || len(policy.Roles) == 0 {
		return "", "policy_render:" + policy.ObjectID().String()
	}
	command, unsupported := renderPolicyCommand(policy.Command, policy.ObjectID())
	if unsupported != "" {
		return "", unsupported
	}
	if unsupported := validatePolicyExpressions(policy); unsupported != "" {
		return "", unsupported
	}
	roles, unsupported := renderRoles(policy.Roles, policy.ObjectID())
	if unsupported != "" {
		return "", unsupported
	}
	mode := "RESTRICTIVE"
	if policy.Permissive {
		mode = "PERMISSIVE"
	}
	sql := "CREATE POLICY " + quote(policy.Name) + " ON " + qualified(policy.Table.Schema, policy.Table.Name) + " AS " + mode + " FOR " + command + " TO " + roles
	if policy.Using != nil {
		if strings.TrimSpace(*policy.Using) == "" {
			return "", "policy_using_empty:" + policy.ObjectID().String()
		}
		sql += " USING (" + *policy.Using + ")"
	}
	if policy.Check != nil {
		if strings.TrimSpace(*policy.Check) == "" {
			return "", "policy_check_empty:" + policy.ObjectID().String()
		}
		sql += " WITH CHECK (" + *policy.Check + ")"
	}
	return sql + ";", ""
}

func validatePolicyExpressions(policy pgschema.Policy) string {
	switch policy.Command {
	case "SELECT", "DELETE":
		if policy.Check != nil {
			return "policy_check_not_allowed:" + policy.ObjectID().String()
		}
	case "INSERT":
		if policy.Using != nil {
			return "policy_using_not_allowed:" + policy.ObjectID().String()
		}
	case "ALL", "UPDATE":
		// Both expressions are legal.
	default:
		return "policy_command_invalid:" + policy.ObjectID().String()
	}
	return ""
}

func renderPolicyAlter(before, after pgschema.Policy) (string, string) {
	if policyRequiresReplacement(before, after) {
		return "", "policy_replacement_required:" + after.ObjectID().String()
	}
	if unsupported := validatePolicyExpressions(after); unsupported != "" {
		return "", unsupported
	}
	var parts []string
	if !reflect.DeepEqual(before.Roles, after.Roles) {
		roles, unsupported := renderRoles(after.Roles, after.ObjectID())
		if unsupported != "" {
			return "", unsupported
		}
		parts = append(parts, "TO "+roles)
	}
	if !reflect.DeepEqual(before.Using, after.Using) {
		if after.Using == nil || strings.TrimSpace(*after.Using) == "" {
			return "", "policy_using_reset:" + after.ObjectID().String()
		}
		parts = append(parts, "USING ("+*after.Using+")")
	}
	if !reflect.DeepEqual(before.Check, after.Check) {
		if after.Check == nil || strings.TrimSpace(*after.Check) == "" {
			return "", "policy_check_reset:" + after.ObjectID().String()
		}
		parts = append(parts, "WITH CHECK ("+*after.Check+")")
	}
	if len(parts) == 0 {
		return "", ""
	}
	return "ALTER POLICY " + quote(after.Name) + " ON " + qualified(after.Table.Schema, after.Table.Name) + " " + strings.Join(parts, " ") + ";", ""
}

func policyRequiresReplacement(before, after pgschema.Policy) bool {
	return before.Table != after.Table || before.Name != after.Name || before.Permissive != after.Permissive || before.Command != after.Command || before.Using != nil && after.Using == nil || before.Check != nil && after.Check == nil
}

func renderPolicyCommand(command string, id pgschema.ID) (string, string) {
	switch command {
	case "ALL", "SELECT", "INSERT", "UPDATE", "DELETE":
		return command, ""
	default:
		return "", "policy_command_invalid:" + id.String()
	}
}

func renderRoles(roles []string, id pgschema.ID) (string, string) {
	if len(roles) == 0 {
		return "", "role_list_empty:" + id.String()
	}
	rendered := make([]string, len(roles))
	for i, role := range roles {
		var unsupported string
		rendered[i], unsupported = renderRole(role, id)
		if unsupported != "" {
			return "", unsupported
		}
	}
	return strings.Join(rendered, ", "), ""
}

func renderRole(role string, id pgschema.ID) (string, string) {
	if role == "" || strings.IndexByte(role, 0) >= 0 {
		return "", "role_invalid:" + id.String()
	}
	if role == "PUBLIC" {
		return "PUBLIC", ""
	}
	return quote(role), ""
}

func renderPrivilege(privilege string, id pgschema.ID) (string, string) {
	switch privilege {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN":
		return privilege, ""
	default:
		return "", "table_privilege_invalid:" + id.String()
	}
}

func renderTablePrivilege(privilege pgschema.TablePrivilege, operation string) (string, string) {
	if (privilege.Table.Kind != pgschema.KindTable && privilege.Table.Kind != pgschema.KindView && privilege.Table.Kind != pgschema.KindMatView) || privilege.Table.Schema == "" || privilege.Table.Name == "" || privilege.Grantor == "" {
		return "", "table_privilege_render:" + privilege.ObjectID().String()
	}
	renderedPrivilege, unsupported := renderPrivilege(privilege.Privilege, privilege.ObjectID())
	if unsupported != "" {
		return "", unsupported
	}
	role, unsupported := renderRole(privilege.Grantee, privilege.ObjectID())
	if unsupported != "" {
		return "", unsupported
	}
	switch operation {
	case "GRANT":
		sql := "GRANT " + renderedPrivilege + " ON TABLE " + qualified(privilege.Table.Schema, privilege.Table.Name) + " TO " + role
		if privilege.Grantable {
			sql += " WITH GRANT OPTION"
		}
		return sql + ";", ""
	case "REVOKE":
		return "REVOKE " + renderedPrivilege + " ON TABLE " + qualified(privilege.Table.Schema, privilege.Table.Name) + " FROM " + role + ";", ""
	default:
		return "", "table_privilege_operation:" + privilege.ObjectID().String()
	}
}

func renderViewOptions(options []pgschema.Option, id pgschema.ID) (string, string) {
	if len(options) == 0 {
		return "", ""
	}
	rendered := make([]string, 0, len(options))
	for _, option := range options {
		if option.Name == "" || option.Value == "" || strings.ContainsAny(option.Name, ";\x00") || !safeViewOptionValue(option.Value) {
			return "", "view_option_render:" + id.String()
		}
		rendered = append(rendered, quote(option.Name)+" = "+option.Value)
	}
	return strings.Join(rendered, ", "), ""
}

func safeViewOptionValue(value string) bool {
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return value != ""
}

func quoteQualifiedName(name string) string {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		parts[i] = quote(part)
	}
	return strings.Join(parts, ".")
}

func renderIndexOptions(options []pgschema.Option, id pgschema.ID) (string, string) {
	rendered := make([]string, 0, len(options))
	for _, option := range options {
		if option.Name == "" || option.Value == "" {
			return "", "index_option_invalid:" + id.String()
		}
		if strings.ContainsAny(option.Name, ";\x00") || !safeIndexOptionValue(option.Value) {
			return "", "index_option_invalid:" + id.String()
		}
		rendered = append(rendered, quote(option.Name)+" = "+option.Value)
	}
	return strings.Join(rendered, ", "), ""
}

func safeIndexOptionValue(value string) bool {
	if value == "true" || value == "false" || value == "on" || value == "off" {
		return true
	}
	if value == "" {
		return false
	}
	for i, r := range value {
		if r >= '0' && r <= '9' {
			continue
		}
		if (r == '+' || r == '-') && i == 0 {
			continue
		}
		if r == '.' {
			continue
		}
		return false
	}
	return true
}

func renderDrop(object pgschema.Object) []protocol.Statement {
	return renderDropWithOptions(object, Options{})
}

func renderDropWithOptions(object pgschema.Object, options Options) []protocol.Statement {
	var sql string
	transactional := true
	hazards := []string{"data_loss", "blocking_lock"}
	switch object := object.(type) {
	case pgschema.Schema:
		prefix := "DROP SCHEMA "
		if options.IfExists {
			prefix += "IF EXISTS "
		}
		// A schema drop necessarily consumes the descendant graph objects
		// that were suppressed from this plan; keep CASCADE as the stable
		// parent-drop invariant.
		sql = prefix + quote(object.Name) + " CASCADE;"
	case pgschema.Enum:
		sql = "DROP TYPE " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Domain:
		sql = "DROP DOMAIN " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Composite:
		sql = "DROP TYPE " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Range:
		sql = "DROP TYPE " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Extension:
		sql = "DROP EXTENSION " + quote(object.Name) + ";"
	case pgschema.Sequence:
		sql = "DROP SEQUENCE " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Table:
		prefix := "DROP TABLE "
		if options.IfExists {
			prefix += "IF EXISTS "
		}
		sql = prefix + qualified(object.Schema, object.Name)
		if options.CascadeDrops {
			sql += " CASCADE"
		}
		sql += ";"
	case pgschema.Column:
		sql = "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " DROP COLUMN " + quote(object.Name) + ";"
	case pgschema.Constraint:
		if object.Parent != nil {
			return nil
		}
		prefix := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " DROP CONSTRAINT "
		if options.IfExists {
			prefix += "IF EXISTS "
		}
		sql = prefix + quote(object.Name) + ";"
	case pgschema.Index:
		if object.Parent != nil {
			return nil
		}
		if options.ConcurrentIndexes && !object.Primary && !object.Exclusion && object.Constraint == "" {
			sql = "DROP INDEX CONCURRENTLY " + qualified(object.Table.Schema, object.Name) + ";"
			transactional = false
			hazards = []string{"data_loss", "concurrent_index_drop"}
		} else {
			sql = "DROP INDEX " + qualified(object.Table.Schema, object.Name) + ";"
		}
	case pgschema.View:
		kind := "VIEW"
		if object.Materialized {
			kind = "MATERIALIZED VIEW"
		}
		sql = "DROP " + kind + " " + qualified(object.Schema, object.Name) + ";"
	case pgschema.Routine:
		kind := strings.ToUpper(object.Kind)
		if kind != "FUNCTION" && kind != "PROCEDURE" {
			return nil
		}
		sql = "DROP " + kind + " " + qualified(object.Schema, object.Name) + "(" + object.Signature + ");"
	case pgschema.Trigger:
		sql = "DROP TRIGGER " + quote(object.Name) + " ON " + qualified(object.Table.Schema, object.Table.Name) + ";"
	case pgschema.Policy:
		sql = "DROP POLICY " + quote(object.Name) + " ON " + qualified(object.Table.Schema, object.Table.Name) + ";"
		hazards = []string{"authorization_change", "row_security_policy_change", "availability_risk"}
	case pgschema.ReplicaIdentity:
		// Every retained table also has a desired DEFAULT node, so this is only
		// reached when its owning table is being dropped and the parent operation
		// covers the catalog state.
		return nil
	case pgschema.RowSecurity:
		var statements []protocol.Statement
		if object.Enabled {
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" DISABLE ROW LEVEL SECURITY;", "contract", "dangerous", "row_security_disabled", "authorization_relaxation", "table_lock"))
		}
		if object.Forced {
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" NO FORCE ROW LEVEL SECURITY;", "contract", "dangerous", "row_security_unforced", "authorization_relaxation", "table_lock"))
		}
		return statements
	case pgschema.TablePrivilege:
		var unsupported string
		sql, unsupported = renderTablePrivilege(object, "REVOKE")
		if unsupported != "" {
			return nil
		}
		hazards = []string{"privilege_revoked", "authorization_change", "availability_risk"}
	default:
		return nil
	}
	result := statement(sql, "contract", "dangerous", transactional, hazards...)
	if object.ObjectID().Kind == pgschema.KindPolicy || object.ObjectID().Kind == pgschema.KindPrivilege {
		result = withTimeoutGuidance(result, 30000, 3000)
	}
	return []protocol.Statement{result}
}

func renderModify(before, after pgschema.Object, choices decisions, options Options, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	switch before := before.(type) {
	case pgschema.Schema:
		next := after.(pgschema.Schema)
		return commentModification("SCHEMA", quote(next.Name), before.Comment, next.Comment), nil, nil, nil
	case pgschema.Table:
		next := after.(pgschema.Table)
		if !reflect.DeepEqual(before.Partition, next.Partition) {
			work, exists := choices.partitionManual[next.ObjectID()]
			if !exists {
				return nil, nil, []string{"partition_reconfiguration:" + next.ObjectID().String()}, nil
			}
			return []protocol.Statement{manualWorkStatement(work, "partition_reconfiguration", "data_movement", "access_exclusive_lock")}, nil, nil, nil
		}
		var statements []protocol.Statement
		if !reflect.DeepEqual(before.PartitionOf, next.PartitionOf) {
			switch {
			case before.PartitionOf == nil && next.PartitionOf != nil:
				if next.PartitionOf.Parent.Kind != pgschema.KindTable || next.PartitionOf.Parent.Schema == "" || next.PartitionOf.Parent.Name == "" || strings.TrimSpace(next.PartitionOf.Bound) == "" {
					return nil, nil, []string{"partition_attach_invalid:" + next.ObjectID().String()}, nil
				}
				sql := "ALTER TABLE " + qualified(next.PartitionOf.Parent.Schema, next.PartitionOf.Parent.Name) + " ATTACH PARTITION " + qualified(next.Schema, next.Name) + " " + next.PartitionOf.Bound + ";"
				statements = append(statements, statement(sql, "contract", "review", true, "partition_attach", "table_scan_possible", "access_exclusive_lock"))
			case before.PartitionOf != nil && next.PartitionOf == nil:
				if before.PartitionOf.Parent.Kind != pgschema.KindTable || before.PartitionOf.Parent.Schema == "" || before.PartitionOf.Parent.Name == "" {
					return nil, nil, []string{"partition_detach_invalid:" + next.ObjectID().String()}, nil
				}
				sql := "ALTER TABLE " + qualified(before.PartitionOf.Parent.Schema, before.PartitionOf.Parent.Name) + " DETACH PARTITION " + qualified(before.Schema, before.Name) + ";"
				statements = append(statements, statement(sql, "contract", "review", true, "partition_detach", "access_exclusive_lock", "compatibility_removal"))
			default:
				// Moving a child to another parent or changing its bound can require
				// data movement and overlap analysis. The only executable path is
				// an explicit, fingerprint-bound operator contract; never infer a
				// detach/attach sequence or cast from declarative state.
				work, exists := choices.partitionManual[next.ObjectID()]
				if !exists {
					return nil, nil, []string{"partition_reconfiguration:" + next.ObjectID().String()}, nil
				}
				statements = append(statements, manualWorkStatement(work, "partition_reconfiguration", "data_movement", "access_exclusive_lock"))
			}
		}
		if before.Unlogged != next.Unlogged {
			mode := "SET LOGGED"
			if next.Unlogged {
				mode = "SET UNLOGGED"
			}
			statements = append(statements, statement("ALTER TABLE "+qualified(next.Schema, next.Name)+" "+mode+";", "contract", "review", true, "table_rewrite_possible", "access_exclusive_lock"))
		}
		if before.Owner != next.Owner {
			if !choices.authorization[next.ObjectID()] {
				return nil, nil, []string{"table_owner_change_unconfirmed:" + next.ObjectID().String()}, nil
			}
			owner := "CURRENT_USER"
			if next.Owner != "" {
				owner = quote(next.Owner)
			}
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(next.Schema, next.Name)+" OWNER TO "+owner+";", "contract", "dangerous", "table_owner_change", "authorization_change"))
		}
		statements = append(statements, commentModification("TABLE", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Column:
		next := after.(pgschema.Column)
		return renderColumnModify(before, next, choices, current, desired, options)
	case pgschema.Index:
		next := after.(pgschema.Index)
		if before.Parent == nil && next.Parent != nil {
			if partitionConstraintAttachmentOwned(before, next, current, desired) {
				// The newly-created local constraint must claim the existing
				// index before that constraint-owned index can be attached. The
				// constraint Create change emits both statements in that order.
				return nil, nil, nil, nil
			}
			statements, unsupported := renderPartitionIndexAttachment(before, next, desired)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			return statements, nil, nil, nil
		}
		if before.Parent != nil || next.Parent != nil {
			return nil, nil, []string{"partitioned_index_modify:" + next.ObjectID().String()}, nil
		}
		if before.Constraint == "" && next.Constraint != "" {
			// The associated constraint is scheduled after this index and emits
			// ADD CONSTRAINT ... USING INDEX.
			return nil, nil, nil, nil
		}
		if before.Constraint != "" && next.Constraint == "" {
			// The constraint drop is scheduled from the current graph; Build
			// appends this standalone-index recreation after that step.
			return nil, nil, nil, nil
		}
		if before.Constraint != "" && before.Constraint == next.Constraint {
			// ALTER TABLE drops and recreates the backing index together with a
			// primary/unique/exclusion constraint rebuild.
			return nil, nil, nil, nil
		}
		beforeNoComment, nextNoComment := before, next
		beforeNoComment.Comment, nextNoComment.Comment = nil, nil
		beforeNoComment.Definition, nextNoComment.Definition = "", ""
		if !reflect.DeepEqual(beforeNoComment, nextNoComment) {
			if next.Constraint != "" || before.Constraint != "" {
				return nil, nil, []string{"constraint_backed_index_modify:" + next.ObjectID().String()}, nil
			}
			if options.ConcurrentIndexes {
				if table, ok := desired.Object(next.Table); ok {
					if table, ok := table.(pgschema.Table); ok && table.Partition != nil {
						statements, unsupported, err := renderContinuousPartitionedIndexReplacement(before, next, current, desired)
						if err != nil {
							return nil, nil, nil, err
						}
						return statements, nil, unsupported, nil
					}
				}
				temporaryName, err := replacementIndexName(before, next)
				if err != nil {
					return nil, nil, nil, err
				}
				if relationNameExists(current, before.Table.Schema, temporaryName) || relationNameExists(desired, next.Table.Schema, temporaryName) {
					return nil, nil, []string{"continuous_index_temporary_name_collision:" + next.ObjectID().String()}, nil
				}
				createSQL, unsupported := renderIndex(next)
				if unsupported != "" {
					return nil, nil, []string{unsupported}, nil
				}
				createSQL = strings.Replace(createSQL, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
				createSQL = strings.Replace(createSQL, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1)
				statements := []protocol.Statement{
					withTimeoutGuidance(statement("ALTER INDEX "+qualified(before.Table.Schema, before.Name)+" RENAME TO "+quote(temporaryName)+";", "expand", "review", true, "continuous_index_replacement", "brief_lock"), 3000, 3000),
					withTimeoutGuidance(statement(strings.TrimSuffix(createSQL, ";")+";", "expand", "review", false, "continuous_index_replacement", "index_build", "table_lock_possible"), 1200000, 3000),
				}
				if next.Comment != nil {
					statements = append(statements, statement("COMMENT ON INDEX "+qualified(next.Table.Schema, next.Name)+" IS "+literal(*next.Comment)+";", "expand", "safe", true))
				}
				statements = append(statements, withTimeoutGuidance(statement("DROP INDEX CONCURRENTLY "+qualified(before.Table.Schema, temporaryName)+";", "contract", "review", false, "continuous_index_replacement", "index_cleanup"), 1200000, 3000))
				return statements, nil, nil, nil
			}
			statements := withPhase(renderDropWithOptions(before, options), "contract", "review", "index_rebuild")
			created, _, unsupported, err := renderCreate(next, pgschema.New(), nil, options)
			if err != nil {
				return nil, nil, nil, err
			}
			return append(statements, withPhase(created, "contract", "review", "index_rebuild")...), nil, unsupported, nil
		}
		return commentModification("INDEX", qualified(next.Table.Schema, next.Name), before.Comment, next.Comment), nil, nil, nil
	case pgschema.Enum:
		next := after.(pgschema.Enum)
		statements, renamed := renameEnumValues(before, next)
		if !renamed {
			var err error
			statements, err = addEnumValues(before, next)
			if err != nil {
				if !choices.enumRewrite[next.ObjectID()] {
					return nil, nil, []string{"enum_rewrite_confirmation:" + next.ObjectID().String()}, nil
				}
				return renderEnumRewrite(before, next, current, desired)
			}
		}
		phase := "expand"
		if renamed {
			phase = "contract"
		}
		statements = append(statements, commentModificationPhase("TYPE", qualified(next.Schema, next.Name), before.Comment, next.Comment, phase)...)
		return statements, nil, nil, nil
	case pgschema.Domain:
		next := after.(pgschema.Domain)
		if before.Schema != next.Schema || before.Name != next.Name || before.BaseType != next.BaseType || before.Collation != next.Collation {
			return nil, nil, []string{"domain_base_type_change:" + next.ObjectID().String()}, nil
		}
		identifier := qualified(next.Schema, next.Name)
		var statements []protocol.Statement
		if !reflect.DeepEqual(before.Default, next.Default) {
			sql := "ALTER DOMAIN " + identifier + " DROP DEFAULT;"
			if next.Default != nil {
				sql = "ALTER DOMAIN " + identifier + " SET DEFAULT " + *next.Default + ";"
			}
			statements = append(statements, statement(sql, "contract", "review", true, "domain_default_change"))
		}
		if before.NotNull != next.NotNull {
			mode := "DROP"
			hazards := []string{"domain_nullability_relaxed"}
			if next.NotNull {
				mode = "SET"
				hazards = []string{"domain_not_null", "existing_values_checked"}
			}
			statements = append(statements, statement("ALTER DOMAIN "+identifier+" "+mode+" NOT NULL;", "contract", "review", true, hazards...))
		}
		constraintStatements, unsupported := renderDomainConstraintChanges(before, next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements = append(statements, constraintStatements...)
		statements = append(statements, commentModification("DOMAIN", identifier, before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Composite:
		next := after.(pgschema.Composite)
		if before.Schema != next.Schema || before.Name != next.Name {
			return nil, nil, []string{"composite_identity_change:" + next.ObjectID().String()}, nil
		}
		statements, unsupported := renderCompositeAttributeChanges(before, next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements = append(statements, commentModification("TYPE", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Range:
		next := after.(pgschema.Range)
		beforeNoComment, nextNoComment := before, next
		beforeNoComment.Comment, nextNoComment.Comment = nil, nil
		if reflect.DeepEqual(beforeNoComment, nextNoComment) {
			return commentModification("TYPE", qualified(next.Schema, next.Name), before.Comment, next.Comment), nil, nil, nil
		}
		if hasChangedDependents(current, desired, before.ObjectID()) {
			return nil, nil, []string{"range_rewrite_dependents:" + next.ObjectID().String()}, nil
		}
		created, unsupported := renderRangeCreate(next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{
			statement("DROP TYPE "+qualified(before.Schema, before.Name)+";", "contract", "review", true, "range_recreate"),
			statement(created, "contract", "review", true, "range_recreate"),
		}
		if next.Comment != nil {
			statements = append(statements, statement("COMMENT ON TYPE "+qualified(next.Schema, next.Name)+" IS "+literal(*next.Comment)+";", "contract", "safe", true, "range_recreate"))
		}
		return statements, nil, nil, nil
	case pgschema.Extension:
		next := after.(pgschema.Extension)
		if before.Name != next.Name || before.Schema != next.Schema || next.Version == "" {
			return nil, nil, []string{"extension_change:" + after.ObjectID().String()}, nil
		}
		var statements []protocol.Statement
		if before.Version != next.Version && !stringIn(options.IgnoreExtensionVersions, next.Name) {
			sql := "ALTER EXTENSION " + quote(next.Name) + " UPDATE TO " + literal(next.Version) + ";"
			statements = append(statements, statement(sql, "contract", "review", true, "extension_update"))
		}
		statements = append(statements, commentModificationPhase("EXTENSION", quote(next.Name), before.Comment, next.Comment, "contract")...)
		return statements, nil, nil, nil
	case pgschema.Sequence:
		next := after.(pgschema.Sequence)
		if before.Schema != next.Schema || before.Name != next.Name || next.Type == "" {
			return nil, nil, []string{"sequence_modify:" + after.ObjectID().String()}, nil
		}
		beforeNoComment, nextNoComment := before, next
		beforeNoComment.Comment, nextNoComment.Comment = nil, nil
		if reflect.DeepEqual(beforeNoComment, nextNoComment) {
			return commentModification("SEQUENCE", qualified(next.Schema, next.Name), before.Comment, next.Comment), nil, nil, nil
		}
		beforeCore, nextCore := beforeNoComment, nextNoComment
		beforeCore.Unlogged, nextCore.Unlogged = false, false
		var statements []protocol.Statement
		if !reflect.DeepEqual(beforeCore, nextCore) {
			parts := []string{
				"AS " + next.Type,
				"START WITH " + fmt.Sprint(next.Start),
				"INCREMENT BY " + fmt.Sprint(next.Increment),
				"MINVALUE " + fmt.Sprint(next.Min),
				"MAXVALUE " + fmt.Sprint(next.Max),
				"CACHE " + fmt.Sprint(next.Cache),
			}
			if next.Cycle {
				parts = append(parts, "CYCLE")
			} else {
				parts = append(parts, "NO CYCLE")
			}
			if !reflect.DeepEqual(before.OwnedBy, next.OwnedBy) {
				if next.OwnedBy == nil {
					parts = append(parts, "OWNED BY NONE")
				} else {
					ownedBy, unsupported := renderSequenceOwnedBy(next)
					if unsupported != "" {
						return nil, nil, []string{unsupported}, nil
					}
					parts = append(parts, ownedBy)
				}
			}
			sql := "ALTER SEQUENCE " + qualified(next.Schema, next.Name) + " " + strings.Join(parts, " ") + ";"
			hazards := []string{"sequence_parameter_change"}
			if !reflect.DeepEqual(before.OwnedBy, next.OwnedBy) {
				hazards = append(hazards, "sequence_ownership_change", "drop_cascade_behavior")
			}
			statements = append(statements, statement(sql, "contract", "review", true, hazards...))
		}
		if before.Unlogged != next.Unlogged {
			persistence := "LOGGED"
			if next.Unlogged {
				persistence = "UNLOGGED"
			}
			statements = append(statements, statement("ALTER SEQUENCE "+qualified(next.Schema, next.Name)+" SET "+persistence+";", "contract", "review", true, "sequence_persistence_change"))
		}
		statements = append(statements, commentModification("SEQUENCE", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.View:
		next := after.(pgschema.View)
		if before.Materialized != next.Materialized {
			return nil, nil, []string{"view_kind_change:" + next.ObjectID().String()}, nil
		}
		if next.Materialized {
			if !materializedViewRequiresRebuild(before, next) {
				return commentModification("MATERIALIZED VIEW", qualified(next.Schema, next.Name), before.Comment, next.Comment), nil, nil, nil
			}
			if !choices.matViewRebuild[next.ObjectID()] {
				return nil, nil, []string{"materialized_view_rebuild:" + next.ObjectID().String()}, nil
			}
			return renderMaterializedViewRebuildClosure(before, next, current, desired, options)
		}
		sql, unsupported := renderViewCreate(next, true)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "contract", "review", true, "view_replace")}
		statements = append(statements, commentModification("VIEW", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Routine:
		next := after.(pgschema.Routine)
		if before.Kind != next.Kind || before.Signature != next.Signature {
			return nil, nil, []string{"routine_identity_change:" + next.ObjectID().String()}, nil
		}
		if before.ReturnType != next.ReturnType {
			return renderRoutineReturnTypeChange(before, next, current, desired)
		}
		beforeNoComment, nextNoComment := before, next
		beforeNoComment.Comment, nextNoComment.Comment = nil, nil
		if reflect.DeepEqual(beforeNoComment, nextNoComment) {
			return commentModification(strings.ToUpper(next.Kind), qualified(next.Schema, next.Name)+"("+next.Signature+")", before.Comment, next.Comment), nil, nil, nil
		}
		sql, unsupported := renderRoutineDefinition(next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "contract", "review", true, "routine_replace", "routine_behavior_change")}
		statements = append(statements, commentModification(strings.ToUpper(next.Kind), qualified(next.Schema, next.Name)+"("+next.Signature+")", before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Trigger:
		next := after.(pgschema.Trigger)
		beforeDefinition, nextDefinition := strings.TrimSpace(before.Definition), strings.TrimSpace(next.Definition)
		var statements []protocol.Statement
		recreated := before.Table != next.Table || before.Routine != next.Routine || beforeDefinition != nextDefinition
		if recreated {
			created, unsupported := renderTriggerDefinition(next)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements,
				statement("DROP TRIGGER "+quote(before.Name)+" ON "+qualified(before.Table.Schema, before.Table.Name)+";", "contract", "review", true, "trigger_recreate", "table_lock", "trigger_behavior_change"),
				statement(created, "contract", "review", true, "trigger_recreate", "table_lock", "trigger_behavior_change"),
			)
		}
		// CREATE TRIGGER always creates the ordinary enabled state. A recreate
		// must restore any non-default target state even when the old trigger
		// happened to carry that same state before it was dropped.
		if before.Enabled != next.Enabled || (recreated && next.Enabled != "O") {
			enabled, unsupported := renderTriggerEnabled(next)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements, statement(enabled, "contract", "review", true, "trigger_enable_state", "table_lock", "trigger_behavior_change"))
		}
		identifier := quote(next.Name) + " ON " + qualified(next.Table.Schema, next.Table.Name)
		commentBefore := before.Comment
		if recreated {
			commentBefore = nil
		}
		statements = append(statements, commentModificationPhase("TRIGGER", identifier, commentBefore, next.Comment, "contract")...)
		return statements, nil, nil, nil
	case pgschema.Policy:
		next := after.(pgschema.Policy)
		if !choices.authorization[next.ObjectID()] {
			return nil, nil, []string{"policy_change_unconfirmed:" + next.ObjectID().String()}, nil
		}
		if policyRequiresReplacement(before, next) {
			created, unsupported := renderPolicyCreate(next)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements := []protocol.Statement{
				authorizationStatement("DROP POLICY "+quote(before.Name)+" ON "+qualified(before.Table.Schema, before.Table.Name)+";", "contract", "dangerous", "policy_replacement", "authorization_change", "availability_risk"),
				authorizationStatement(created, "contract", "review", "policy_replacement", "authorization_change", "availability_risk"),
			}
			return statements, nil, nil, nil
		}
		sql, unsupported := renderPolicyAlter(before, next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		if sql == "" {
			return nil, nil, nil, nil
		}
		return []protocol.Statement{authorizationStatement(sql, "contract", "review", "policy_altered", "authorization_change", "availability_risk")}, nil, nil, nil
	case pgschema.ReplicaIdentity:
		next := after.(pgschema.ReplicaIdentity)
		if before.Table != next.Table {
			return nil, nil, []string{"replica_identity_table_change:" + next.ObjectID().String()}, nil
		}
		sql, unsupported := renderReplicaIdentity(next)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		if sql == "" {
			return nil, nil, nil, nil
		}
		return []protocol.Statement{statement(sql, "contract", "review", true, "replica_identity_change", "table_lock")}, nil, nil, nil
	case pgschema.RowSecurity:
		next := after.(pgschema.RowSecurity)
		if (before.Enabled && !next.Enabled || before.Forced && !next.Forced) && !choices.authorization[next.ObjectID()] {
			return nil, nil, []string{"row_security_relaxation_unconfirmed:" + next.ObjectID().String()}, nil
		}
		if before.Table != next.Table || (!next.Enabled && !next.Forced) {
			return nil, nil, []string{"row_security_modify:" + next.ObjectID().String()}, nil
		}
		var statements []protocol.Statement
		if before.Enabled != next.Enabled {
			mode, phase, safety := "ENABLE", "expand", "review"
			hazards := []string{"row_security_enabled", "authorization_change", "availability_risk", "table_lock"}
			if !next.Enabled {
				mode, phase, safety = "DISABLE", "contract", "dangerous"
				hazards = []string{"row_security_disabled", "authorization_relaxation", "table_lock"}
			}
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(next.Table.Schema, next.Table.Name)+" "+mode+" ROW LEVEL SECURITY;", phase, safety, hazards...))
		}
		if before.Forced != next.Forced {
			mode, phase, safety := "FORCE", "expand", "review"
			hazards := []string{"row_security_forced", "authorization_change", "availability_risk", "table_lock"}
			if !next.Forced {
				mode, phase, safety = "NO FORCE", "contract", "dangerous"
				hazards = []string{"row_security_unforced", "authorization_relaxation", "table_lock"}
			}
			statements = append(statements, authorizationStatement("ALTER TABLE "+qualified(next.Table.Schema, next.Table.Name)+" "+mode+" ROW LEVEL SECURITY;", phase, safety, hazards...))
		}
		return statements, nil, nil, nil
	case pgschema.TablePrivilege:
		next := after.(pgschema.TablePrivilege)
		if before.Table != next.Table || before.Grantee != next.Grantee || before.Grantor != next.Grantor || before.Privilege != next.Privilege {
			return nil, nil, []string{"table_privilege_identity_change:" + next.ObjectID().String()}, nil
		}
		if before.Grantable == next.Grantable {
			return nil, nil, nil, nil
		}
		if !next.Grantable && !choices.authorization[next.ObjectID()] {
			return nil, nil, []string{"grant_option_revoke_unconfirmed:" + next.ObjectID().String()}, nil
		}
		role, unsupported := renderRole(next.Grantee, next.ObjectID())
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		privilege, unsupported := renderPrivilege(next.Privilege, next.ObjectID())
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		if next.Grantable {
			sql := "GRANT " + privilege + " ON TABLE " + qualified(next.Table.Schema, next.Table.Name) + " TO " + role + " WITH GRANT OPTION;"
			return []protocol.Statement{authorizationStatement(sql, "expand", "review", "grant_option_added", "authorization_change")}, nil, nil, nil
		}
		sql := "REVOKE GRANT OPTION FOR " + privilege + " ON TABLE " + qualified(next.Table.Schema, next.Table.Name) + " FROM " + role + ";"
		return []protocol.Statement{authorizationStatement(sql, "contract", "dangerous", "grant_option_revoked", "authorization_change", "availability_risk")}, nil, nil, nil
	case pgschema.Constraint:
		next := after.(pgschema.Constraint)
		if before.Parent != nil || next.Parent != nil {
			return nil, nil, []string{"partitioned_constraint_modify:" + next.ObjectID().String()}, nil
		}
		if constraintUsesMatchPartial(next) {
			return nil, nil, []string{"foreign_key_match_partial:" + next.ObjectID().String()}, nil
		}
		beforeNoComment, nextNoComment := before, next
		beforeNoComment.Comment, nextNoComment.Comment = nil, nil
		if constraintsEqualIgnoringValidation(beforeNoComment, nextNoComment) && before.Validated != next.Validated {
			if next.Validated {
				sql := "ALTER TABLE " + qualified(next.Table.Schema, next.Table.Name) + " VALIDATE CONSTRAINT " + quote(next.Name) + ";"
				return []protocol.Statement{statement(sql, "expand", "review", true, "table_scan", "share_update_exclusive_lock")}, nil, nil, nil
			}
			// PostgreSQL cannot mark a validated constraint NOT VALID in place.
			// Let the ordinary structural-difference path below rebuild it with
			// the desired NOT VALID definition.
		}
		if !reflect.DeepEqual(beforeNoComment, nextNoComment) {
			if options.ConcurrentIndexes && (before.Type == pgschema.ConstraintPrimary || before.Type == pgschema.ConstraintUnique) && (next.Type == pgschema.ConstraintPrimary || next.Type == pgschema.ConstraintUnique) {
				statements, consumed, unsupported, err := renderContinuousConstraintReplacement(before, next, current, desired)
				if err != nil {
					return nil, nil, nil, err
				}
				if len(unsupported) == 0 {
					return statements, consumed, nil, nil
				}
				return nil, nil, unsupported, nil
			}
			drop := withPhase(renderDrop(before), "contract", "review", "constraint_rebuild", "blocking_lock")
			add := withPhase(renderConstraintCreate(next), "contract", "review", "constraint_rebuild")
			statements := append(drop, add...)
			if next.Comment != nil {
				statements = append(statements, statement("COMMENT ON CONSTRAINT "+quote(next.Name)+" ON "+qualified(next.Table.Schema, next.Table.Name)+" IS "+literal(*next.Comment)+";", "contract", "safe", true, "constraint_rebuild"))
			}
			return statements, nil, nil, nil
		}
		return renderConstraintComment(next, before.Comment, next.Comment), nil, nil, nil
	default:
		return nil, nil, []string{"modify:" + after.ObjectID().String()}, nil
	}
}

func renderPartitionIndexAttachment(before, after pgschema.Index, desired *pgschema.Snapshot) ([]protocol.Statement, string) {
	if before.Parent != nil || after.Parent == nil || before.Table != after.Table || before.Name != after.Name || before.Constraint != "" || after.Constraint != "" || before.Primary || after.Primary || before.Exclusion || after.Exclusion {
		return nil, "partition_index_attach_unsupported:" + after.ObjectID().String()
	}
	beforeComparable, afterComparable := before, after
	beforeComparable.Parent, afterComparable.Parent = nil, nil
	beforeComparable.Comment, afterComparable.Comment = nil, nil
	beforeComparable.Definition, afterComparable.Definition = "", ""
	beforeComparable.Concurrent, afterComparable.Concurrent = false, false
	if !reflect.DeepEqual(beforeComparable, afterComparable) {
		return nil, "partition_index_attach_rebuild:" + after.ObjectID().String()
	}
	parentObject, exists := desired.Object(*after.Parent)
	parent, ok := parentObject.(pgschema.Index)
	if !exists || !ok || parent.Constraint != "" || parent.Primary || parent.Exclusion {
		return nil, "partition_index_attach_parent:" + after.ObjectID().String()
	}
	childTableObject, childExists := desired.Object(after.Table)
	parentTableObject, parentExists := desired.Object(parent.Table)
	childTable, childOK := childTableObject.(pgschema.Table)
	parentTable, parentOK := parentTableObject.(pgschema.Table)
	if !childExists || !parentExists || !childOK || !parentOK || childTable.PartitionOf == nil || childTable.PartitionOf.Parent != parent.Table || parentTable.Partition == nil {
		return nil, "partition_index_attach_table_hierarchy:" + after.ObjectID().String()
	}
	if !equivalentPartitionAttachmentIndex(after, parent) {
		return nil, "partition_index_attach_structure:" + after.ObjectID().String()
	}
	sql := "ALTER INDEX " + qualified(parent.Table.Schema, parent.Name) + " ATTACH PARTITION " + qualified(after.Table.Schema, after.Name) + ";"
	statements := []protocol.Statement{
		withTimeoutGuidance(statement(sql, "expand", "review", true, "partition_index_attach", "brief_lock"), 30000, 3000),
	}
	statements = append(statements, commentModification("INDEX", qualified(after.Table.Schema, after.Name), before.Comment, after.Comment)...)
	return statements, ""
}

func equivalentPartitionAttachmentIndex(child, parent pgschema.Index) bool {
	return child.Unique == parent.Unique && child.Method == parent.Method &&
		reflect.DeepEqual(child.Parts, parent.Parts) && reflect.DeepEqual(child.Include, parent.Include) &&
		child.Predicate == parent.Predicate && child.NullsNotDistinct == parent.NullsNotDistinct
}

func partitionConstraintAttachmentOwned(before, after pgschema.Index, current, desired *pgschema.Snapshot) bool {
	if current == nil || desired == nil || before.Parent != nil || after.Parent == nil || before.Constraint != "" || after.Constraint == "" || before.Name != after.Name || before.Table != after.Table {
		return false
	}
	constraintID := (pgschema.Constraint{Table: after.Table, Name: after.Constraint}).ObjectID()
	if _, exists := current.Object(constraintID); exists {
		return false
	}
	object, exists := desired.Object(constraintID)
	constraint, ok := object.(pgschema.Constraint)
	if !exists || !ok || constraint.Parent == nil || constraint.UsingIndex != after.Name {
		return false
	}
	return partitionConstraintAttachmentValid(constraint, before, after, desired)
}

func partitionConstraintAttachmentValid(constraint pgschema.Constraint, before, after pgschema.Index, desired *pgschema.Snapshot) bool {
	if desired == nil || constraint.Parent == nil || (constraint.Type != pgschema.ConstraintPrimary && constraint.Type != pgschema.ConstraintUnique) ||
		before.Parent != nil || after.Parent == nil || before.Constraint != "" || after.Constraint != constraint.Name || before.Name != constraint.Name || after.Name != constraint.Name ||
		!before.Unique || !after.Unique || before.Table != constraint.Table || after.Table != constraint.Table {
		return false
	}
	parentConstraintObject, exists := desired.Object(*constraint.Parent)
	parentConstraint, ok := parentConstraintObject.(pgschema.Constraint)
	if !exists || !ok || parentConstraint.Type != constraint.Type || parentConstraint.Table == constraint.Table {
		return false
	}
	parentIndex, exists := backingIndexForConstraint(desired, parentConstraint)
	if !exists || parentIndex.ObjectID() != *after.Parent {
		return false
	}
	beforeComparable, afterComparable := before, after
	beforeComparable.Parent, afterComparable.Parent = nil, nil
	beforeComparable.Constraint, afterComparable.Constraint = "", ""
	beforeComparable.Primary, afterComparable.Primary = false, false
	beforeComparable.Definition, afterComparable.Definition = "", ""
	beforeComparable.Concurrent, afterComparable.Concurrent = false, false
	return reflect.DeepEqual(beforeComparable, afterComparable) && equivalentPartitionAttachmentIndex(after, parentIndex)
}

func renderContinuousConstraintReplacement(before, after pgschema.Constraint, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if before.Parent != nil || after.Parent != nil || before.Table != after.Table || before.Name != after.Name || before.Type != after.Type {
		return nil, nil, []string{"continuous_constraint_replacement:" + after.ObjectID().String()}, nil
	}
	tableObject, exists := desired.Object(after.Table)
	table, ok := tableObject.(pgschema.Table)
	if !exists || !ok || table.Partition != nil || table.PartitionOf != nil {
		return nil, nil, []string{"continuous_partitioned_constraint_replacement:" + after.ObjectID().String()}, nil
	}
	foreignKeys, consumed, dependentUnsupported := coordinatedConstraintForeignKeys(before, after, current, desired)
	if len(dependentUnsupported) > 0 {
		return nil, nil, dependentUnsupported, nil
	}
	oldIndex, oldExists := backingIndexForConstraint(current, before)
	newIndex, newExists := backingIndexForConstraint(desired, after)
	if !oldExists || !newExists || oldIndex.Parent != nil || newIndex.Parent != nil || !oldIndex.Unique || !newIndex.Unique {
		return nil, nil, []string{"continuous_constraint_backing_index:" + after.ObjectID().String()}, nil
	}
	replicaIdentity, identityRelevant, identityUnsupported := coordinatedConstraintReplicaIdentity(oldIndex, newIndex, current, desired)
	if len(identityUnsupported) > 0 {
		return nil, nil, identityUnsupported, nil
	}
	if identityRelevant {
		consumed = append(consumed, replicaIdentity.before.ObjectID())
	}
	temporaryName, err := replacementIndexName(oldIndex, newIndex)
	if err != nil {
		return nil, nil, nil, err
	}
	if relationNameExists(current, oldIndex.Table.Schema, temporaryName) || relationNameExists(desired, newIndex.Table.Schema, temporaryName) {
		return nil, nil, []string{"continuous_index_temporary_name_collision:" + newIndex.ObjectID().String()}, nil
	}
	replacement := newIndex
	replacement.Name = temporaryName
	replacement.Constraint = ""
	replacement.Primary = false
	replacement.Comment = nil
	createSQL, unsupported := renderIndex(replacement)
	if unsupported != "" {
		return nil, nil, []string{unsupported}, nil
	}
	createSQL = strings.Replace(createSQL, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
	createSQL = strings.Replace(createSQL, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1)
	statements := []protocol.Statement{
		withTimeoutGuidance(statement("DROP INDEX CONCURRENTLY IF EXISTS "+qualified(newIndex.Table.Schema, temporaryName)+";", "expand", "review", false, "continuous_constraint_replacement", "retry_cleanup", "reserved_temporary_identity", "index_drop"), 1200000, 3000),
		withTimeoutGuidance(statement(strings.TrimSuffix(createSQL, ";")+";", "expand", "review", false, "continuous_constraint_replacement", "index_build", "table_lock_possible"), 1200000, 3000),
	}
	if newIndex.Comment != nil {
		statements = append(statements, withTimeoutGuidance(statement("COMMENT ON INDEX "+qualified(newIndex.Table.Schema, temporaryName)+" IS "+literal(*newIndex.Comment)+";", "expand", "safe", true, "continuous_constraint_replacement"), 30000, 3000))
	}
	dropSQL := "ALTER TABLE " + qualified(before.Table.Schema, before.Table.Name) + " DROP CONSTRAINT " + quote(before.Name) + ";"
	addSQL, unsupported := renderConstraintUsingReplacementIndex(after, temporaryName)
	if unsupported != "" {
		return nil, nil, []string{unsupported}, nil
	}
	contractStart := len(statements)
	if identityRelevant && replicaIdentity.resetBeforeSwap {
		statements = append(statements, renderCoordinatedReplicaIdentityDefault(replicaIdentity.before.Table))
	}
	for _, foreignKey := range foreignKeys {
		statements = append(statements, renderCoordinatedForeignKeyDrop(foreignKey.before))
	}
	statements = append(statements,
		withTimeoutGuidance(statement(dropSQL, "contract", "review", true, "continuous_constraint_replacement", "brief_constraint_swap", "access_exclusive_lock"), 30000, 3000),
		withTimeoutGuidance(statement(addSQL, "contract", "review", true, "continuous_constraint_replacement", "brief_constraint_swap", "access_exclusive_lock"), 30000, 3000),
	)
	if after.Comment != nil {
		statements = append(statements, withTimeoutGuidance(statement("COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+qualified(after.Table.Schema, after.Table.Name)+" IS "+literal(*after.Comment)+";", "contract", "safe", true, "continuous_constraint_replacement"), 30000, 3000))
	}
	if identityRelevant && replicaIdentity.restoreAfterSwap {
		identitySQL, unsupported := renderReplicaIdentity(replicaIdentity.after)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements = append(statements, renderCoordinatedReplicaIdentity(identitySQL))
	}
	for _, foreignKey := range foreignKeys {
		if foreignKey.after == nil {
			continue
		}
		statements = append(statements, renderCoordinatedForeignKeyAdd(*foreignKey.after)...)
	}
	if contractStart < len(statements) {
		statements[contractStart].BatchBoundaryBefore = true
	}
	return statements, consumed, nil, nil
}

func backingIndexForConstraint(snapshot *pgschema.Snapshot, constraint pgschema.Constraint) (pgschema.Index, bool) {
	if snapshot == nil {
		return pgschema.Index{}, false
	}
	for _, object := range snapshot.Objects() {
		index, ok := object.(pgschema.Index)
		if ok && index.Table == constraint.Table && index.Constraint == constraint.Name {
			return index, true
		}
	}
	return pgschema.Index{}, false
}

type coordinatedForeignKey struct {
	before pgschema.Constraint
	after  *pgschema.Constraint
}

type coordinatedReplicaIdentity struct {
	before           pgschema.ReplicaIdentity
	after            pgschema.ReplicaIdentity
	resetBeforeSwap  bool
	restoreAfterSwap bool
}

func coordinatedConstraintReplicaIdentity(beforeIndex, afterIndex pgschema.Index, current, desired *pgschema.Snapshot) (coordinatedReplicaIdentity, bool, []string) {
	if current == nil || desired == nil || beforeIndex.Table != afterIndex.Table {
		return coordinatedReplicaIdentity{}, false, []string{"continuous_constraint_replica_identity_snapshot:" + afterIndex.ObjectID().String()}
	}
	identityID := (pgschema.ReplicaIdentity{Table: beforeIndex.Table}).ObjectID()
	beforeObject, beforeExists := current.Object(identityID)
	afterObject, afterExists := desired.Object(identityID)
	before, beforeOK := beforeObject.(pgschema.ReplicaIdentity)
	after, afterOK := afterObject.(pgschema.ReplicaIdentity)
	if !beforeExists || !afterExists || !beforeOK || !afterOK || before.Table != beforeIndex.Table || after.Table != afterIndex.Table {
		return coordinatedReplicaIdentity{}, false, []string{"continuous_constraint_replica_identity:" + identityID.String()}
	}
	beforeUsesReplacedIndex := before.Mode == pgschema.ReplicaIdentityIndex && before.Index != nil && *before.Index == beforeIndex.ObjectID()
	afterUsesReplacementIndex := after.Mode == pgschema.ReplicaIdentityIndex && after.Index != nil && *after.Index == afterIndex.ObjectID()
	if !beforeUsesReplacedIndex && !afterUsesReplacementIndex {
		return coordinatedReplicaIdentity{}, false, nil
	}
	return coordinatedReplicaIdentity{
		before:           before,
		after:            after,
		resetBeforeSwap:  beforeUsesReplacedIndex,
		restoreAfterSwap: after.Mode != pgschema.ReplicaIdentityDefault,
	}, true, nil
}

func coordinatedConstraintForeignKeys(before, after pgschema.Constraint, current, desired *pgschema.Snapshot) ([]coordinatedForeignKey, []pgschema.ID, []string) {
	if current == nil || desired == nil {
		return nil, nil, []string{"continuous_constraint_replacement_snapshot:" + after.ObjectID().String()}
	}
	var foreignKeys []coordinatedForeignKey
	var consumed []pgschema.ID
	var unsupported []string
	for _, id := range current.IDs() {
		if id == before.ObjectID() || !containsID(current.Dependencies(id), before.ObjectID()) {
			continue
		}
		object, exists := current.Object(id)
		constraint, ok := object.(pgschema.Constraint)
		if !exists || !ok || constraint.Type != pgschema.ConstraintForeign {
			if id.Kind != pgschema.KindIndex {
				unsupported = append(unsupported, "continuous_constraint_replacement_dependent:"+id.String())
			}
			continue
		}
		if constraint.Parent != nil || constraintUsesMatchPartial(constraint) {
			unsupported = append(unsupported, "continuous_constraint_replacement_foreign_key:"+id.String())
			continue
		}
		entry := coordinatedForeignKey{before: constraint}
		if afterObject, desiredExists := desired.Object(id); desiredExists {
			afterConstraint, afterOK := afterObject.(pgschema.Constraint)
			if !afterOK || afterConstraint.Type != pgschema.ConstraintForeign || afterConstraint.Parent != nil || constraintUsesMatchPartial(afterConstraint) {
				unsupported = append(unsupported, "continuous_constraint_replacement_foreign_key:"+id.String())
				continue
			}
			entry.after = &afterConstraint
		}
		foreignKeys = append(foreignKeys, entry)
		consumed = append(consumed, id)
	}
	sort.Slice(foreignKeys, func(i, j int) bool {
		return foreignKeys[i].before.ObjectID().String() < foreignKeys[j].before.ObjectID().String()
	})
	sort.Slice(consumed, func(i, j int) bool { return consumed[i].String() < consumed[j].String() })
	return foreignKeys, consumed, unionStrings(unsupported, nil)
}

func coordinatedConstraintChangeIDs(changes []change.Change, current, desired *pgschema.Snapshot, options Options) map[pgschema.ID]bool {
	result := make(map[pgschema.ID]bool)
	if !options.ConcurrentIndexes {
		return result
	}
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindConstraint {
			continue
		}
		before, beforeOK := item.Before.(pgschema.Constraint)
		after, afterOK := item.After.(pgschema.Constraint)
		if !beforeOK || !afterOK || before.Parent != nil || after.Parent != nil ||
			(before.Type != pgschema.ConstraintPrimary && before.Type != pgschema.ConstraintUnique) ||
			(after.Type != pgschema.ConstraintPrimary && after.Type != pgschema.ConstraintUnique) {
			continue
		}
		foreignKeys, _, unsupported := coordinatedConstraintForeignKeys(before, after, current, desired)
		if len(unsupported) > 0 {
			continue
		}
		for _, foreignKey := range foreignKeys {
			result[foreignKey.before.ObjectID()] = true
		}
		oldIndex, oldExists := backingIndexForConstraint(current, before)
		newIndex, newExists := backingIndexForConstraint(desired, after)
		if !oldExists || !newExists {
			continue
		}
		replicaIdentity, relevant, identityUnsupported := coordinatedConstraintReplicaIdentity(oldIndex, newIndex, current, desired)
		if relevant && len(identityUnsupported) == 0 {
			result[replicaIdentity.before.ObjectID()] = true
		}
	}
	return result
}

func renderCoordinatedReplicaIdentityDefault(table pgschema.ID) protocol.Statement {
	sql := "ALTER TABLE " + qualified(table.Schema, table.Name) + " REPLICA IDENTITY DEFAULT;"
	return renderCoordinatedReplicaIdentity(sql)
}

func renderCoordinatedReplicaIdentity(sql string) protocol.Statement {
	return withTimeoutGuidance(statement(sql, "contract", "review", true, "continuous_constraint_replacement", "replica_identity_change", "logical_replication_contract", "brief_constraint_swap", "access_exclusive_lock"), 30000, 3000)
}

func renderCoordinatedForeignKeyDrop(constraint pgschema.Constraint) protocol.Statement {
	sql := "ALTER TABLE " + qualified(constraint.Table.Schema, constraint.Table.Name) + " DROP CONSTRAINT " + quote(constraint.Name) + ";"
	return withTimeoutGuidance(statement(sql, "contract", "review", true, "continuous_constraint_replacement", "foreign_key_coordination", "brief_constraint_swap", "access_exclusive_lock"), 30000, 3000)
}

func renderCoordinatedForeignKeyAdd(constraint pgschema.Constraint) []protocol.Statement {
	definition := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(constraint.Definition), ";"))
	if strings.HasSuffix(strings.ToUpper(definition), " NOT VALID") {
		definition = strings.TrimSpace(definition[:len(definition)-len(" NOT VALID")])
	}
	addSQL := "ALTER TABLE " + qualified(constraint.Table.Schema, constraint.Table.Name) + " ADD CONSTRAINT " + quote(constraint.Name) + " " + definition + " NOT VALID;"
	statements := []protocol.Statement{
		withTimeoutGuidance(statement(addSQL, "contract", "review", true, "continuous_constraint_replacement", "foreign_key_coordination", "share_row_exclusive_lock"), 30000, 3000),
	}
	statements[0].BatchBoundaryBefore = true
	if constraint.Comment != nil {
		commentSQL := "COMMENT ON CONSTRAINT " + quote(constraint.Name) + " ON " + qualified(constraint.Table.Schema, constraint.Table.Name) + " IS " + literal(*constraint.Comment) + ";"
		statements = append(statements, withTimeoutGuidance(statement(commentSQL, "contract", "safe", true, "continuous_constraint_replacement", "foreign_key_coordination"), 30000, 3000))
	}
	if constraint.Validated {
		validateSQL := "ALTER TABLE " + qualified(constraint.Table.Schema, constraint.Table.Name) + " VALIDATE CONSTRAINT " + quote(constraint.Name) + ";"
		validate := withTimeoutGuidance(statement(validateSQL, "contract", "review", true, "continuous_constraint_replacement", "foreign_key_coordination", "table_scan", "share_update_exclusive_lock"), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	return statements
}

func hasChangedDependents(current, desired *pgschema.Snapshot, dependency pgschema.ID) bool {
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if snapshot == nil {
			continue
		}
		for _, id := range snapshot.IDs() {
			if id == dependency || !containsID(snapshot.Dependencies(id), dependency) {
				continue
			}
			// Any external dependent can prevent DROP CONSTRAINT or observe the
			// identity swap. It needs its own typed coordinated strategy.
			if id.Kind != pgschema.KindIndex {
				return true
			}
		}
	}
	return false
}

func renderConstraintUsingReplacementIndex(constraint pgschema.Constraint, indexName string) (string, string) {
	kind := ""
	switch constraint.Type {
	case pgschema.ConstraintPrimary:
		kind = "PRIMARY KEY"
	case pgschema.ConstraintUnique:
		kind = "UNIQUE"
	default:
		return "", "continuous_constraint_type:" + constraint.ObjectID().String()
	}
	sql := "ALTER TABLE " + qualified(constraint.Table.Schema, constraint.Table.Name) + " ADD CONSTRAINT " + quote(constraint.Name) + " " + kind + " USING INDEX " + quote(indexName)
	if constraint.Deferrable {
		sql += " DEFERRABLE"
		if constraint.Deferred {
			sql += " INITIALLY DEFERRED"
		}
	}
	return sql + ";", ""
}

func renderContinuousPartitionedIndexReplacement(before, after pgschema.Index, current, desired *pgschema.Snapshot) ([]protocol.Statement, []string, error) {
	if before.Parent != nil || after.Parent != nil || before.Constraint != "" || after.Constraint != "" || before.Primary || after.Primary || before.Exclusion || after.Exclusion {
		return nil, []string{"continuous_partitioned_index_replacement:" + after.ObjectID().String()}, nil
	}
	parentTemporaryName, err := replacementIndexName(before, after)
	if err != nil {
		return nil, nil, err
	}
	if relationNameExists(current, before.Table.Schema, parentTemporaryName) || relationNameExists(desired, after.Table.Schema, parentTemporaryName) {
		return nil, []string{"continuous_index_temporary_name_collision:" + after.ObjectID().String()}, nil
	}
	root := partitionIndexReplacement{before: before, after: after, temporaryName: parentTemporaryName, partitioned: true}
	children, unsupported, err := buildPartitionIndexReplacementChildren(before, after, current, desired)
	if err != nil {
		return nil, nil, err
	}
	if unsupported != "" {
		return nil, []string{unsupported}, nil
	}
	root.children = children
	parentSQL, unsupported := renderPartitionedIndexShell(after)
	if unsupported != "" {
		return nil, []string{unsupported}, nil
	}
	statements := []protocol.Statement{
		withTimeoutGuidance(statement("ALTER INDEX "+qualified(before.Table.Schema, before.Name)+" RENAME TO "+quote(parentTemporaryName)+";", "expand", "review", true, "continuous_partitioned_index_replacement", "brief_lock"), 3000, 3000),
	}
	for _, child := range flattenPartitionIndexReplacements(root.children) {
		statements = append(statements, withTimeoutGuidance(statement("ALTER INDEX "+qualified(child.before.Table.Schema, child.before.Name)+" RENAME TO "+quote(child.temporaryName)+";", "expand", "review", true, "continuous_partitioned_index_replacement", "brief_lock"), 3000, 3000))
	}
	statements = append(statements, withTimeoutGuidance(statement(strings.TrimSuffix(parentSQL, ";")+";", "expand", "review", true, "continuous_partitioned_index_replacement", "partitioned_index_shell", "brief_lock"), 30000, 3000))
	if after.Comment != nil {
		statements = append(statements, withTimeoutGuidance(statement("COMMENT ON INDEX "+qualified(after.Table.Schema, after.Name)+" IS "+literal(*after.Comment)+";", "expand", "safe", true, "continuous_partitioned_index_replacement"), 30000, 3000))
	}
	for _, child := range root.children {
		built, unsupported := renderPartitionIndexReplacement(child, root.after)
		if unsupported != "" {
			return nil, []string{unsupported}, nil
		}
		statements = append(statements, built...)
	}
	statements = append(statements, withTimeoutGuidance(statement("DROP INDEX "+qualified(before.Table.Schema, parentTemporaryName)+";", "contract", "review", true, "continuous_partitioned_index_replacement", "index_cleanup", "brief_lock"), 30000, 3000))
	return statements, nil, nil
}

type partitionIndexReplacement struct {
	before        pgschema.Index
	after         pgschema.Index
	temporaryName string
	partitioned   bool
	children      []partitionIndexReplacement
}

func buildPartitionIndexReplacementChildren(before, after pgschema.Index, current, desired *pgschema.Snapshot) ([]partitionIndexReplacement, string, error) {
	oldChildren := partitionIndexChildren(current, before.ObjectID())
	newChildren := partitionIndexChildren(desired, after.ObjectID())
	if len(oldChildren) != len(newChildren) {
		return nil, "continuous_partitioned_index_children_changed:" + after.ObjectID().String(), nil
	}
	oldByTable := make(map[pgschema.ID]pgschema.Index, len(oldChildren))
	for _, child := range oldChildren {
		oldByTable[child.Table] = child
	}
	result := make([]partitionIndexReplacement, 0, len(newChildren))
	for _, child := range newChildren {
		old, exists := oldByTable[child.Table]
		if !exists || old.Parent == nil || child.Parent == nil || old.Constraint != "" || child.Constraint != "" || old.Primary || child.Primary || old.Exclusion || child.Exclusion {
			return nil, "continuous_partitioned_index_child_mismatch:" + child.ObjectID().String(), nil
		}
		oldTableObject, oldExists := current.Object(old.Table)
		newTableObject, newExists := desired.Object(child.Table)
		oldTable, oldOK := oldTableObject.(pgschema.Table)
		newTable, newOK := newTableObject.(pgschema.Table)
		if !oldExists || !newExists || !oldOK || !newOK || (oldTable.Partition != nil) != (newTable.Partition != nil) {
			return nil, "continuous_partitioned_index_child_table_mismatch:" + child.ObjectID().String(), nil
		}
		temporaryName, err := replacementIndexName(old, child)
		if err != nil {
			return nil, "", err
		}
		if relationNameExists(current, old.Table.Schema, temporaryName) || relationNameExists(desired, child.Table.Schema, temporaryName) {
			return nil, "continuous_index_temporary_name_collision:" + child.ObjectID().String(), nil
		}
		node := partitionIndexReplacement{before: old, after: child, temporaryName: temporaryName, partitioned: newTable.Partition != nil}
		nested, unsupported, err := buildPartitionIndexReplacementChildren(old, child, current, desired)
		if err != nil || unsupported != "" {
			return nil, unsupported, err
		}
		node.children = nested
		if !node.partitioned && len(node.children) != 0 {
			return nil, "continuous_partitioned_index_leaf_has_children:" + child.ObjectID().String(), nil
		}
		result = append(result, node)
	}
	return result, "", nil
}

func flattenPartitionIndexReplacements(nodes []partitionIndexReplacement) []partitionIndexReplacement {
	var result []partitionIndexReplacement
	for _, node := range nodes {
		result = append(result, node)
		result = append(result, flattenPartitionIndexReplacements(node.children)...)
	}
	return result
}

func renderPartitionedIndexShell(index pgschema.Index) (string, string) {
	sql, unsupported := renderIndex(index)
	if unsupported != "" {
		return "", unsupported
	}
	needle := " ON " + qualified(index.Table.Schema, index.Table.Name)
	if !strings.Contains(sql, needle) {
		return "", "partitioned_index_only_render:" + index.ObjectID().String()
	}
	return strings.Replace(sql, needle, " ON ONLY "+qualified(index.Table.Schema, index.Table.Name), 1), ""
}

func renderPartitionIndexReplacement(node partitionIndexReplacement, parent pgschema.Index) ([]protocol.Statement, string) {
	var statements []protocol.Statement
	if node.partitioned {
		sql, unsupported := renderPartitionedIndexShell(node.after)
		if unsupported != "" {
			return nil, unsupported
		}
		statements = append(statements, withTimeoutGuidance(statement(strings.TrimSuffix(sql, ";")+";", "expand", "review", true, "continuous_partitioned_index_replacement", "partitioned_index_shell", "brief_lock"), 30000, 3000))
	} else {
		sql, unsupported := renderIndex(node.after)
		if unsupported != "" {
			return nil, unsupported
		}
		sql = strings.Replace(sql, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
		sql = strings.Replace(sql, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1)
		statements = append(statements, withTimeoutGuidance(statement(strings.TrimSuffix(sql, ";")+";", "expand", "review", false, "continuous_partitioned_index_replacement", "index_build", "table_lock_possible"), 1200000, 3000))
	}
	if node.after.Comment != nil {
		statements = append(statements, withTimeoutGuidance(statement("COMMENT ON INDEX "+qualified(node.after.Table.Schema, node.after.Name)+" IS "+literal(*node.after.Comment)+";", "expand", "safe", true, "continuous_partitioned_index_replacement"), 30000, 3000))
	}
	for _, child := range node.children {
		built, unsupported := renderPartitionIndexReplacement(child, node.after)
		if unsupported != "" {
			return nil, unsupported
		}
		statements = append(statements, built...)
	}
	statements = append(statements, withTimeoutGuidance(statement("ALTER INDEX "+qualified(parent.Table.Schema, parent.Name)+" ATTACH PARTITION "+qualified(node.after.Table.Schema, node.after.Name)+";", "expand", "review", true, "continuous_partitioned_index_replacement", "partition_index_attach", "brief_lock"), 30000, 3000))
	return statements, ""
}

func partitionIndexChildren(snapshot *pgschema.Snapshot, parent pgschema.ID) []pgschema.Index {
	if snapshot == nil {
		return nil
	}
	var result []pgschema.Index
	for _, object := range snapshot.Objects() {
		index, ok := object.(pgschema.Index)
		if ok && index.Parent != nil && *index.Parent == parent {
			result = append(result, index)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Table.String() < result[j].Table.String() })
	return result
}

func partitionReconfiguration(before, after *pgschema.PartitionOf) bool {
	if reflect.DeepEqual(before, after) || before == nil || after == nil {
		return false
	}
	return true
}

func materializedViewRequiresRebuild(before, after pgschema.View) bool {
	before.Comment, after.Comment = nil, nil
	return !reflect.DeepEqual(before, after)
}

type dependentViewDepth struct {
	before pgschema.View
	after  pgschema.View
	depth  int
}

func renderMaterializedViewRebuildClosure(before, after pgschema.View, current, desired *pgschema.Snapshot, options Options) ([]protocol.Statement, []pgschema.ID, []string, error) {
	dependents, safe := unchangedDependentViewClosure(current, desired, before.ObjectID())
	if !safe {
		return nil, nil, []string{"materialized_view_changed_dependent:" + after.ObjectID().String()}, nil
	}
	var statements []protocol.Statement
	consumed := make([]pgschema.ID, 0, len(dependents))
	for index := len(dependents) - 1; index >= 0; index-- {
		dependent := dependents[index]
		statements = append(statements, withPhase(renderDrop(dependent.before), "contract", "dangerous", "materialized_view_rebuild", "data_loss", "blocking_lock")...)
		consumed = append(consumed, dependent.before.ObjectID())
	}
	statements = append(statements, withPhase(renderDrop(before), "contract", "dangerous", "materialized_view_rebuild", "data_loss", "blocking_lock")...)
	views := append([]pgschema.View{after}, func() []pgschema.View {
		result := make([]pgschema.View, 0, len(dependents))
		for _, dependent := range dependents {
			result = append(result, dependent.after)
		}
		return result
	}()...)
	for _, view := range views {
		created, _, unsupported, err := renderCreate(view, pgschema.New(), nil, options)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(unsupported) > 0 {
			return nil, nil, unsupported, nil
		}
		statements = append(statements, withPhase(created, "contract", "review", "materialized_view_rebuild", "data_loss", "blocking_lock")...)
		if desired == nil || !view.Materialized {
			continue
		}
		for _, relationIndex := range indexesForRelation(desired, view.ObjectID()) {
			indexes, _, indexUnsupported, err := renderCreate(relationIndex, desired, nil, options)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(indexUnsupported) > 0 {
				return nil, nil, indexUnsupported, nil
			}
			statements = append(statements, withPhase(indexes, "contract", "review", "materialized_view_index_rebuild", "index_build", "table_lock_possible")...)
		}
	}
	return statements, consumed, nil, nil
}

func unchangedDependentViewClosure(current, desired *pgschema.Snapshot, base pgschema.ID) ([]dependentViewDepth, bool) {
	if current == nil || desired == nil {
		return nil, true
	}
	depths := map[pgschema.ID]int{base: 0}
	dependents := make(map[pgschema.ID]dependentViewDepth)
	changed := true
	for changed {
		changed = false
		for _, object := range current.Objects() {
			view, ok := object.(pgschema.View)
			if !ok || view.ObjectID() == base {
				continue
			}
			depth := 0
			for _, dependency := range current.Dependencies(view.ObjectID()) {
				if dependencyDepth, exists := depths[dependency]; exists && dependencyDepth+1 > depth {
					depth = dependencyDepth + 1
				}
			}
			if depth == 0 {
				continue
			}
			afterObject, exists := desired.Object(view.ObjectID())
			after, afterIsView := afterObject.(pgschema.View)
			if !exists || !afterIsView || !reflect.DeepEqual(view, after) {
				return nil, false
			}
			if previous, exists := depths[view.ObjectID()]; !exists || depth > previous {
				depths[view.ObjectID()] = depth
				dependents[view.ObjectID()] = dependentViewDepth{before: view, after: after, depth: depth}
				changed = true
			}
		}
	}
	result := make([]dependentViewDepth, 0, len(dependents))
	for _, dependent := range dependents {
		result = append(result, dependent)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].depth != result[j].depth {
			return result[i].depth < result[j].depth
		}
		return result[i].before.ObjectID().String() < result[j].before.ObjectID().String()
	})
	return result, true
}

func constraintsEqualIgnoringValidation(before, after pgschema.Constraint) bool {
	before.Validated, after.Validated = false, false
	before.Definition = strings.ReplaceAll(before.Definition, " NOT VALID", "")
	after.Definition = strings.ReplaceAll(after.Definition, " NOT VALID", "")
	return reflect.DeepEqual(before, after)
}

func renderDomainConstraintAdd(domain pgschema.Domain, constraint pgschema.DomainConstraint) (string, string) {
	if constraint.Name == "" || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(constraint.Definition)), "CHECK ") {
		return "", "domain_constraint_definition:" + domain.ObjectID().String() + ":" + constraint.Name
	}
	sql := "ALTER DOMAIN " + qualified(domain.Schema, domain.Name) + " ADD CONSTRAINT " + quote(constraint.Name) + " " + constraint.Definition
	if !constraint.Validated {
		sql += " NOT VALID"
	}
	return sql + ";", ""
}

func renderDomainConstraintChanges(before, after pgschema.Domain) ([]protocol.Statement, string) {
	beforeByName := make(map[string]pgschema.DomainConstraint, len(before.Constraints))
	afterByName := make(map[string]pgschema.DomainConstraint, len(after.Constraints))
	for _, constraint := range before.Constraints {
		if constraint.Name == "" || beforeByName[constraint.Name].Name != "" {
			return nil, "domain_constraint_identity:" + before.ObjectID().String()
		}
		beforeByName[constraint.Name] = constraint
	}
	for _, constraint := range after.Constraints {
		if constraint.Name == "" || afterByName[constraint.Name].Name != "" {
			return nil, "domain_constraint_identity:" + after.ObjectID().String()
		}
		afterByName[constraint.Name] = constraint
	}
	identifier := qualified(after.Schema, after.Name)
	var statements []protocol.Statement
	var dropped, added []string
	for name, oldConstraint := range beforeByName {
		newConstraint, exists := afterByName[name]
		if !exists {
			dropped = append(dropped, name)
			continue
		}
		if oldConstraint.Definition != newConstraint.Definition || oldConstraint.Validated && !newConstraint.Validated {
			statements = append(statements, statement("ALTER DOMAIN "+identifier+" DROP CONSTRAINT "+quote(name)+";", "contract", "review", true, "domain_constraint_replaced"))
			created, unsupported := renderDomainConstraintAdd(after, newConstraint)
			if unsupported != "" {
				return nil, unsupported
			}
			statements = append(statements, statement(created, "contract", "review", true, "domain_constraint_replaced", "existing_values_checked"))
		} else if !oldConstraint.Validated && newConstraint.Validated {
			statements = append(statements, statement("ALTER DOMAIN "+identifier+" VALIDATE CONSTRAINT "+quote(name)+";", "contract", "review", true, "domain_constraint_validated", "existing_values_checked"))
		}
	}
	for name := range afterByName {
		if _, exists := beforeByName[name]; !exists {
			added = append(added, name)
		}
	}
	sort.Strings(dropped)
	sort.Strings(added)
	usedAdded := make(map[string]bool)
	usedDropped := make(map[string]bool)
	for _, oldName := range dropped {
		oldConstraint := beforeByName[oldName]
		var candidates []string
		for _, newName := range added {
			if usedAdded[newName] {
				continue
			}
			newConstraint := afterByName[newName]
			if oldConstraint.Definition == newConstraint.Definition && oldConstraint.Validated == newConstraint.Validated {
				candidates = append(candidates, newName)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		newName := candidates[0]
		reverseCandidates := 0
		for _, candidateOldName := range dropped {
			if usedDropped[candidateOldName] {
				continue
			}
			candidate := beforeByName[candidateOldName]
			if candidate.Definition == afterByName[newName].Definition && candidate.Validated == afterByName[newName].Validated {
				reverseCandidates++
			}
		}
		if reverseCandidates != 1 {
			continue
		}
		statements = append(statements, statement("ALTER DOMAIN "+identifier+" RENAME CONSTRAINT "+quote(oldName)+" TO "+quote(newName)+";", "contract", "review", true, "domain_constraint_identity"))
		usedDropped[oldName], usedAdded[newName] = true, true
	}
	for _, name := range dropped {
		if usedDropped[name] {
			continue
		}
		statements = append(statements, statement("ALTER DOMAIN "+identifier+" DROP CONSTRAINT "+quote(name)+";", "contract", "review", true, "domain_constraint_removed"))
	}
	for _, name := range added {
		if usedAdded[name] {
			continue
		}
		created, unsupported := renderDomainConstraintAdd(after, afterByName[name])
		if unsupported != "" {
			return nil, unsupported
		}
		statements = append(statements, statement(created, "contract", "review", true, "domain_constraint_added", "existing_values_checked"))
	}
	return statements, ""
}

func renderCompositeAttributeChanges(before, after pgschema.Composite) ([]protocol.Statement, string) {
	beforeByName := make(map[string]pgschema.CompositeAttribute, len(before.Attributes))
	afterByName := make(map[string]pgschema.CompositeAttribute, len(after.Attributes))
	for _, attribute := range before.Attributes {
		if attribute.Name == "" || beforeByName[attribute.Name].Name != "" {
			return nil, "composite_attribute_identity:" + before.ObjectID().String()
		}
		beforeByName[attribute.Name] = attribute
	}
	for _, attribute := range after.Attributes {
		if attribute.Name == "" || afterByName[attribute.Name].Name != "" {
			return nil, "composite_attribute_identity:" + after.ObjectID().String()
		}
		afterByName[attribute.Name] = attribute
	}
	var beforeRetained, afterRetained []string
	for _, attribute := range before.Attributes {
		if _, exists := afterByName[attribute.Name]; exists {
			beforeRetained = append(beforeRetained, attribute.Name)
		}
	}
	lastRetained := -1
	for index, attribute := range after.Attributes {
		if _, exists := beforeByName[attribute.Name]; exists {
			afterRetained = append(afterRetained, attribute.Name)
			lastRetained = index
		}
	}
	if !reflect.DeepEqual(beforeRetained, afterRetained) {
		return nil, "composite_attribute_reorder:" + after.ObjectID().String()
	}
	for index, attribute := range after.Attributes {
		if _, exists := beforeByName[attribute.Name]; !exists && index < lastRetained {
			return nil, "composite_attribute_reorder:" + after.ObjectID().String()
		}
	}
	identifier := qualified(after.Schema, after.Name)
	var statements []protocol.Statement
	for _, attribute := range before.Attributes {
		if _, exists := afterByName[attribute.Name]; exists {
			continue
		}
		statements = append(statements, statement("ALTER TYPE "+identifier+" DROP ATTRIBUTE "+quote(attribute.Name)+" CASCADE;", "contract", "review", true, "composite_attribute_removed", "dependent_row_type_change"))
	}
	for _, oldAttribute := range before.Attributes {
		newAttribute, exists := afterByName[oldAttribute.Name]
		if !exists || oldAttribute.Type == newAttribute.Type && oldAttribute.Collation == newAttribute.Collation {
			continue
		}
		definition := newAttribute.Type
		if newAttribute.Collation != "" {
			definition += " COLLATE " + newAttribute.Collation
		}
		statements = append(statements, statement("ALTER TYPE "+identifier+" ALTER ATTRIBUTE "+quote(newAttribute.Name)+" TYPE "+definition+" CASCADE;", "contract", "review", true, "composite_attribute_type_change", "dependent_row_type_change"))
	}
	for _, attribute := range after.Attributes {
		if _, exists := beforeByName[attribute.Name]; exists {
			continue
		}
		definition := attribute.Type
		if attribute.Collation != "" {
			definition += " COLLATE " + attribute.Collation
		}
		statements = append(statements, statement("ALTER TYPE "+identifier+" ADD ATTRIBUTE "+quote(attribute.Name)+" "+definition+" CASCADE;", "contract", "review", true, "composite_attribute_added", "dependent_row_type_change"))
	}
	return statements, ""
}

func renderRangeCreate(object pgschema.Range) (string, string) {
	if object.Schema == "" || object.Name == "" || strings.TrimSpace(object.Subtype) == "" || object.MultirangeName == "" {
		return "", "range_create:" + object.ObjectID().String()
	}
	if object.Canonical != "" {
		return "", "range_canonical_dependency:" + object.ObjectID().String()
	}
	options := []string{"SUBTYPE = " + object.Subtype}
	if object.Collation != "" {
		options = append(options, "COLLATION = "+object.Collation)
	}
	if object.SubtypeOpClass != "" {
		options = append(options, "SUBTYPE_OPCLASS = "+object.SubtypeOpClass)
	}
	if object.SubtypeDiff != "" {
		options = append(options, "SUBTYPE_DIFF = "+object.SubtypeDiff)
	}
	options = append(options, "MULTIRANGE_TYPE_NAME = "+quote(object.MultirangeName))
	return "CREATE TYPE " + qualified(object.Schema, object.Name) + " AS RANGE (" + strings.Join(options, ", ") + ");", ""
}

func addEnumValues(before, after pgschema.Enum) ([]protocol.Statement, error) {
	positions := make(map[string]int, len(before.Labels))
	for i, label := range before.Labels {
		positions[label] = i
	}
	at := 0
	var statements []protocol.Statement
	for i, label := range after.Labels {
		position, exists := positions[label]
		if exists {
			if position != at {
				return nil, fmt.Errorf("enum reorder")
			}
			at++
			continue
		}
		clause := ""
		if i == 0 && len(before.Labels) > 0 {
			clause = " BEFORE " + literal(before.Labels[0])
		} else if i > 0 && at != len(before.Labels) {
			clause = " AFTER " + literal(after.Labels[i-1])
		}
		sql := "ALTER TYPE " + qualified(after.Schema, after.Name) + " ADD VALUE " + literal(label) + clause + ";"
		statements = append(statements, statement(sql, "expand", "review", true))
	}
	for _, label := range before.Labels {
		found := false
		for _, desired := range after.Labels {
			found = found || label == desired
		}
		if !found {
			return nil, fmt.Errorf("enum drop")
		}
	}
	return statements, nil
}

// renameEnumValues recognizes only a pure positional label rename. Every old
// label being changed must disappear from the target and every replacement
// label must be absent from the source. That excludes reorders, swaps, and
// mixed rename/addition shapes, which continue through the explicit enum
// rewrite boundary.
func renameEnumValues(before, after pgschema.Enum) ([]protocol.Statement, bool) {
	if len(before.Labels) != len(after.Labels) {
		return nil, false
	}
	beforeLabels := make(map[string]bool, len(before.Labels))
	afterLabels := make(map[string]bool, len(after.Labels))
	for _, label := range before.Labels {
		beforeLabels[label] = true
	}
	for _, label := range after.Labels {
		afterLabels[label] = true
	}
	type rename struct{ before, after string }
	var renames []rename
	for i, oldLabel := range before.Labels {
		newLabel := after.Labels[i]
		if oldLabel == newLabel {
			continue
		}
		if afterLabels[oldLabel] || beforeLabels[newLabel] {
			return nil, false
		}
		renames = append(renames, rename{before: oldLabel, after: newLabel})
	}
	if len(renames) == 0 {
		return nil, false
	}
	statements := make([]protocol.Statement, 0, len(renames))
	for _, rename := range renames {
		sql := "ALTER TYPE " + qualified(after.Schema, after.Name) + " RENAME VALUE " + literal(rename.before) + " TO " + literal(rename.after) + ";"
		statements = append(statements, statement(sql, "contract", "review", true, "enum_value_rename", "application_compatibility"))
	}
	return statements, true
}

func enumRequiresRewrite(before, after pgschema.Enum) bool {
	if _, renamed := renameEnumValues(before, after); renamed {
		return false
	}
	_, err := addEnumValues(before, after)
	return err != nil
}

func renderEnumRewrite(before, after pgschema.Enum, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if before.Schema != after.Schema || before.Name != after.Name {
		return nil, nil, []string{"enum_rewrite_identity:" + after.ObjectID().String()}, nil
	}
	currentColumns, unsupported := enumRewriteColumns(current, before.ObjectID())
	desiredColumns, desiredUnsupported := enumRewriteColumns(desired, after.ObjectID())
	unsupported = append(unsupported, desiredUnsupported...)
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, nil, unsupported, nil
	}
	if len(currentColumns) != len(desiredColumns) {
		return nil, nil, []string{"enum_rewrite_column_set:" + after.ObjectID().String()}, nil
	}
	for id, column := range currentColumns {
		next, exists := desiredColumns[id]
		if !exists || !reflect.DeepEqual(column, next) {
			return nil, nil, []string{"enum_rewrite_column_change:" + id.String()}, nil
		}
	}
	temporaryName := enumRewriteTemporaryName(before)
	if schemaNameExists(current, before.Schema, temporaryName) || schemaNameExists(desired, after.Schema, temporaryName) {
		return nil, nil, []string{"enum_rewrite_temporary_name_collision:" + after.ObjectID().String()}, nil
	}
	hazards := []string{"enum_rewrite", "blocking_lock", "value_cast_validation", "application_compatibility"}
	statements := []protocol.Statement{
		statement("ALTER TYPE "+qualified(before.Schema, before.Name)+" RENAME TO "+quote(temporaryName)+";", "contract", "review", true, hazards...),
		statement(renderEnumDefinition(after), "contract", "review", true, hazards...),
	}
	if after.Comment != nil {
		statements = append(statements, statement("COMMENT ON TYPE "+qualified(after.Schema, after.Name)+" IS "+literal(*after.Comment)+";", "contract", "safe", true, "enum_rewrite"))
	}
	ids := make([]pgschema.ID, 0, len(currentColumns))
	for id := range currentColumns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		column := desiredColumns[id]
		identifier := "ALTER TABLE " + qualified(column.Table.Schema, column.Table.Name) + " ALTER COLUMN " + quote(column.Name)
		if column.Default != nil {
			statements = append(statements, statement(identifier+" DROP DEFAULT;", "contract", "review", true, "enum_rewrite", "blocking_lock"))
		}
		textType := "text" + arraySuffix(column.Type)
		statements = append(statements, statement(identifier+" TYPE "+column.Type+" USING "+quote(column.Name)+"::"+textType+"::"+column.Type+";", "contract", "review", true, hazards...))
		if column.Default != nil {
			statements = append(statements, statement(identifier+" SET DEFAULT "+*column.Default+";", "contract", "review", true, "enum_rewrite"))
		}
	}
	statements = append(statements, statement("DROP TYPE "+qualified(before.Schema, temporaryName)+";", "contract", "review", true, "enum_rewrite"))
	return statements, nil, nil, nil
}

func enumRewriteColumns(snapshot *pgschema.Snapshot, enumID pgschema.ID) (map[pgschema.ID]pgschema.Column, []string) {
	columns := make(map[pgschema.ID]pgschema.Column)
	if snapshot == nil {
		return columns, nil
	}
	var unsupported []string
	for _, id := range snapshot.IDs() {
		if id == enumID || !containsID(snapshot.Dependencies(id), enumID) {
			continue
		}
		object, _ := snapshot.Object(id)
		switch object := object.(type) {
		case pgschema.Column:
			if object.Generated != nil {
				unsupported = append(unsupported, "enum_rewrite_generated_column:"+id.String())
				continue
			}
			columns[id] = object
		case pgschema.Table:
			// CREATE TABLE consumes its column definitions, so tables carry a
			// synthetic type edge in addition to their typed columns.
		default:
			unsupported = append(unsupported, "enum_rewrite_dependent:"+id.String())
		}
	}
	for id := range columns {
		for _, dependentID := range snapshot.IDs() {
			if dependentID == id || !containsID(snapshot.Dependencies(dependentID), id) {
				continue
			}
			unsupported = append(unsupported, "enum_rewrite_column_dependent:"+dependentID.String())
		}
	}
	return columns, unsupported
}

func enumRewriteTemporaryName(object pgschema.Enum) string {
	digest := sha256.Sum256([]byte(object.ObjectID().String()))
	return "onwardpg_tmpenum_" + hex.EncodeToString(digest[:8])
}

func schemaNameExists(snapshot *pgschema.Snapshot, schema, name string) bool {
	if snapshot == nil {
		return false
	}
	for _, id := range snapshot.IDs() {
		if id.Schema == schema && id.Name == name && id.Part == "" && id.Signature == "" {
			return true
		}
	}
	return false
}

func arraySuffix(typeName string) string {
	suffix := ""
	for strings.HasSuffix(typeName, "[]") {
		suffix += "[]"
		typeName = strings.TrimSuffix(typeName, "[]")
	}
	return suffix
}

func renderEnumDefinition(object pgschema.Enum) string {
	labels := make([]string, len(object.Labels))
	for index, label := range object.Labels {
		labels[index] = literal(label)
	}
	return "CREATE TYPE " + qualified(object.Schema, object.Name) + " AS ENUM (" + strings.Join(labels, ", ") + ");"
}

func mutationQuestions(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]protocol.Question, decisions, error) {
	decisions := decisions{typeUsing: make(map[pgschema.ID]string), notNullStrategy: make(map[pgschema.ID]string), notNullBackfill: make(map[pgschema.ID]protocol.ManualWork), matViewRebuild: make(map[pgschema.ID]bool), matViewRefresh: make(map[pgschema.ID]protocol.ManualWork), partitionManual: make(map[pgschema.ID]protocol.ManualWork), identityDrop: make(map[pgschema.ID]bool), enumRewrite: make(map[pgschema.ID]bool), authorization: make(map[pgschema.ID]bool), renameBridge: make(map[pgschema.ID]protocol.ManualWork)}
	var questions []protocol.Question
	refreshes := dependentMaterializedViewsNeedingRefresh(changes, current, desired)
	for _, view := range refreshes {
		question := protocol.Question{
			ID: "refresh_materialized_view:" + view.ObjectID().String(), Kind: "refresh_materialized_view", Key: view.ObjectID().String(),
			Message:            "Materialized view " + view.ObjectID().String() + " depends on a changed ordinary view. Supply an explicit reviewed refresh and verification contract; onwardpg will not infer refresh mode, locking, or data checks.",
			Choices:            []string{"provided"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		work, found, err := resolver.ResolveManual(question)
		if err != nil {
			return nil, decisions, err
		}
		if !found {
			questions = append(questions, question)
		} else {
			decisions.matViewRefresh[view.ObjectID()] = work
		}
	}
	for _, item := range changes {
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindTable {
			before, after := item.Before.(pgschema.Table), item.After.(pgschema.Table)
			if before.Owner != after.Owner {
				question := protocol.Question{
					ID: "change_table_owner:" + item.ID.String(), Kind: "change_table_owner", Key: item.ID.String(),
					Message:            "Table " + item.ID.String() + " changes owner from " + before.Owner + " to " + after.Owner + ". Confirm this reviewed authorization transfer.",
					Choices:            []string{"authorize"},
					CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
				}
				_, found, err := resolver.Resolve(question)
				if err != nil {
					return nil, decisions, err
				}
				if !found {
					questions = append(questions, question)
				} else {
					decisions.authorization[item.ID] = true
				}
			}
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindEnum {
			before, after := item.Before.(pgschema.Enum), item.After.(pgschema.Enum)
			if enumRequiresRewrite(before, after) {
				question := protocol.Question{
					ID: "rewrite_enum:" + item.ID.String(), Kind: "rewrite_enum", Key: item.ID.String(),
					Message:            "Enum " + item.ID.String() + " requires an identity-changing rewrite. Confirm the reviewed blocking operation; every stored scalar or array value must cast through text into the desired labels.",
					Choices:            []string{"rewrite"},
					CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
				}
				_, found, err := resolver.Resolve(question)
				if err != nil {
					return nil, decisions, err
				}
				if !found {
					questions = append(questions, question)
				} else {
					decisions.enumRewrite[item.ID] = true
				}
			}
			continue
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindPolicy {
			before, after := item.Before.(pgschema.Policy), item.After.(pgschema.Policy)
			kind, choice, action := "alter_policy", "alter", "changes roles or policy expressions"
			if policyRequiresReplacement(before, after) {
				kind, choice, action = "replace_policy", "replace", "changes an immutable policy attribute or removes an expression, requiring DROP POLICY followed by CREATE POLICY"
			}
			question := protocol.Question{
				ID: kind + ":" + item.ID.String(), Kind: kind, Key: item.ID.String(),
				Message:            "Policy " + item.ID.String() + " " + action + ". Confirm the reviewed authorization transition.",
				Choices:            []string{choice},
				CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
			}
			_, found, err := resolver.Resolve(question)
			if err != nil {
				return nil, decisions, err
			}
			if !found {
				questions = append(questions, question)
			} else {
				decisions.authorization[item.ID] = true
			}
			continue
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindRowSecurity {
			before, after := item.Before.(pgschema.RowSecurity), item.After.(pgschema.RowSecurity)
			if before.Enabled && !after.Enabled || before.Forced && !after.Forced {
				question := protocol.Question{
					ID: "relax_row_security:" + item.ID.String(), Kind: "relax_row_security", Key: item.ID.String(),
					Message:            "Row security on " + item.ID.String() + " is being disabled or unforced. Confirm this reviewed authorization relaxation.",
					Choices:            []string{"relax"},
					CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
				}
				_, found, err := resolver.Resolve(question)
				if err != nil {
					return nil, decisions, err
				}
				if !found {
					questions = append(questions, question)
				} else {
					decisions.authorization[item.ID] = true
				}
			}
			continue
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindPrivilege {
			before, after := item.Before.(pgschema.TablePrivilege), item.After.(pgschema.TablePrivilege)
			if before.Grantable && !after.Grantable {
				question := protocol.Question{
					ID: "revoke_grant_option:" + item.ID.String(), Kind: "revoke_grant_option", Key: item.ID.String(),
					Message:            "Privilege " + item.ID.String() + " loses WITH GRANT OPTION. Confirm this reviewed authorization contraction.",
					Choices:            []string{"revoke"},
					CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
				}
				_, found, err := resolver.Resolve(question)
				if err != nil {
					return nil, decisions, err
				}
				if !found {
					questions = append(questions, question)
				} else {
					decisions.authorization[item.ID] = true
				}
			}
			continue
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindTable {
			before, after := item.Before.(pgschema.Table), item.After.(pgschema.Table)
			if partitionReconfiguration(before.PartitionOf, after.PartitionOf) || !reflect.DeepEqual(before.Partition, after.Partition) {
				question := protocol.Question{
					ID: "partition_reconfiguration:" + item.ID.String(), Kind: "partition_reconfiguration", Key: item.ID.String(),
					Message:            "Partitioning for " + item.ID.String() + " changes parent, bound, strategy, or key. Supply an explicit, reviewed manual work contract (including data movement and verification SQL); onwardpg will not infer it.",
					Choices:            []string{"provided"},
					CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
				}
				work, found, err := resolver.ResolveManual(question)
				if err != nil {
					return nil, decisions, err
				}
				if !found {
					questions = append(questions, question)
				} else {
					decisions.partitionManual[item.ID] = work
				}
			}
		}
		if item.Kind == change.Modify && item.ID.Kind == pgschema.KindMatView {
			before, after := item.Before.(pgschema.View), item.After.(pgschema.View)
			if !materializedViewRequiresRebuild(before, after) {
				continue
			}
			question := protocol.Question{
				ID: "rebuild_materialized_view:" + item.ID.String(), Kind: "rebuild_materialized_view", Key: item.ID.String(),
				Message:            "Materialized view " + item.ID.String() + " changes. Rebuilding drops its stored data before recreating it; confirm this reviewed forward operation.",
				Choices:            []string{"rebuild"},
				CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
			}
			_, found, err := resolver.Resolve(question)
			if err != nil {
				return nil, decisions, err
			}
			if !found {
				questions = append(questions, question)
			} else {
				decisions.matViewRebuild[item.ID] = true
			}
			continue
		}
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindColumn {
			continue
		}
		before, after := item.Before.(pgschema.Column), item.After.(pgschema.Column)
		if before.Identity != nil && after.Identity == nil {
			question := protocol.Question{
				ID: "drop_identity:" + item.ID.String(), Kind: "drop_identity", Key: item.ID.String(),
				Message:            "Column " + item.ID.String() + " drops its identity and owned sequence state. Confirm this destructive forward transition.",
				Choices:            []string{"drop"},
				CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
			}
			_, found, err := resolver.Resolve(question)
			if err != nil {
				return nil, decisions, err
			}
			if !found {
				questions = append(questions, question)
			} else {
				decisions.identityDrop[item.ID] = true
			}
		}
		if before.Serial == nil && after.Serial == nil && before.Type != after.Type && !generatedColumnTypeChange(before, after) {
			question := protocol.Question{
				ID: "type_change:" + item.ID.String(), Kind: "type_change", Key: item.ID.String(),
				Message:            "Column " + item.ID.String() + " changes from " + before.Type + " to " + after.Type + ". Choose reviewed expand/contract SQL that preserves both application contracts around one deployment, or split the feature because it needs two deployments.",
				Choices:            []string{"manual_sql", "split_plan"},
				CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
			}
			answer, found, err := resolver.Resolve(question)
			if err != nil {
				return nil, decisions, err
			}
			if !found {
				questions = append(questions, question)
			} else {
				decisions.typeUsing[item.ID] = answer
			}
		}
		if !before.NotNull && after.NotNull {
			question := protocol.Question{
				ID: "set_not_null:" + item.ID.String(), Kind: "set_not_null", Key: item.ID.String(),
				Message:            "Column " + item.ID.String() + " becomes NOT NULL. Choose a direct lock/scan, a staged validation for already-clean data, or staged validation with an explicit application-owned backfill.",
				Choices:            []string{"direct", "staged", "staged_with_backfill"},
				CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
			}
			answer, found, err := resolver.Resolve(question)
			if err != nil {
				return nil, decisions, err
			}
			if !found {
				questions = append(questions, question)
			} else {
				decisions.notNullStrategy[item.ID] = answer
				if answer == "staged_with_backfill" {
					backfillQuestion := protocol.Question{
						ID: "backfill_not_null:" + item.ID.String(), Kind: "backfill_not_null", Key: item.ID.String(),
						Message:            "Supply the reviewed application-owned backfill and a boolean postcondition proving " + item.ID.String() + " contains no NULL values before contract validation.",
						Choices:            []string{"provided"},
						CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
					}
					work, provided, err := resolver.ResolveManual(backfillQuestion)
					if err != nil {
						return nil, decisions, err
					}
					if !provided {
						questions = append(questions, backfillQuestion)
					} else {
						decisions.notNullBackfill[item.ID] = work
					}
				}
			}
		}
	}
	return questions, decisions, nil
}

// dependentMaterializedViewsNeedingRefresh finds materialized views whose
// stored data can be stale after a semantic ordinary-view replacement or
// routine-body replacement. It follows only typed catalog dependency edges,
// including ordinary-view chains.
// A materialized view that is itself recreated has fresh rows from that
// operation, so it deliberately does not receive a redundant refresh contract.
func dependentMaterializedViewsNeedingRefresh(changes []change.Change, current, desired *pgschema.Snapshot) []pgschema.View {
	if current == nil || desired == nil {
		return nil
	}
	changed := make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind != change.Modify {
			continue
		}
		switch item.ID.Kind {
		case pgschema.KindView:
			before, beforeOK := item.Before.(pgschema.View)
			after, afterOK := item.After.(pgschema.View)
			if beforeOK && afterOK && !before.Materialized && !after.Materialized && ordinaryViewRequiresRefresh(before, after) {
				changed[item.ID] = true
			}
		case pgschema.KindRoutine:
			before, beforeOK := item.Before.(pgschema.Routine)
			after, afterOK := item.After.(pgschema.Routine)
			if beforeOK && afterOK && routineRequiresMaterializedRefresh(before, after) {
				changed[item.ID] = true
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// Any ordinary view depending (transitively) on a changed one can lead to a
	// stale materialized dependent. Grow the changed set before collecting
	// materialized leaves so ordering is independent of catalog object order.
	for progressed := true; progressed; {
		progressed = false
		for _, object := range current.Objects() {
			view, ok := object.(pgschema.View)
			if !ok || view.Materialized || changed[view.ObjectID()] {
				continue
			}
			for _, dependency := range current.Dependencies(view.ObjectID()) {
				if changed[dependency] {
					changed[view.ObjectID()] = true
					progressed = true
					break
				}
			}
		}
	}
	var result []pgschema.View
	for _, object := range current.Objects() {
		view, ok := object.(pgschema.View)
		if !ok || !view.Materialized {
			continue
		}
		for _, dependency := range current.Dependencies(view.ObjectID()) {
			if !changed[dependency] {
				continue
			}
			desiredObject, exists := desired.Object(view.ObjectID())
			desiredView, desiredOK := desiredObject.(pgschema.View)
			if exists && desiredOK && desiredView.Materialized && !materializedViewRequiresRebuild(view, desiredView) {
				result = append(result, view)
			}
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ObjectID().String() < result[j].ObjectID().String() })
	return result
}

func ordinaryViewRequiresRefresh(before, after pgschema.View) bool {
	return before.Definition != after.Definition || !reflect.DeepEqual(before.Options, after.Options)
}

func routineRequiresMaterializedRefresh(before, after pgschema.Routine) bool {
	before.Comment, after.Comment = nil, nil
	return !reflect.DeepEqual(before, after)
}

func sortedManualWorkIDs(work map[pgschema.ID]protocol.ManualWork) []pgschema.ID {
	ids := make([]pgschema.ID, 0, len(work))
	for id := range work {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func splitPlanGuidance(changes []change.Change, choices decisions) []protocol.Guidance {
	var guidance []protocol.Guidance
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindColumn || choices.typeUsing[item.ID] != "split_plan" {
			continue
		}
		before, beforeOK := item.Before.(pgschema.Column)
		after, afterOK := item.After.(pgschema.Column)
		if !beforeOK || !afterOK || before.Type == after.Type {
			continue
		}
		table := qualified(after.Table.Schema, after.Table.Name)
		shadow := splitPlanShadowColumnName(before, after)
		guidance = append(guidance, protocol.Guidance{
			Kind: "split_plan", Key: item.ID.String(),
			Summary: "Revise this feature's desired DDL to retain the current column and add a separately named nullable shadow column. Deploy compatibility code and complete synchronization/backfill before a later feature removes the old contract.",
			Steps: []protocol.GuidanceStep{
				{
					Stage: "plan_a_expand_scaffold",
					SQL:   "ALTER TABLE " + table + " ADD COLUMN " + quote(shadow) + " " + after.Type + ";",
				},
				{
					Stage: "plan_a_product_semantics",
					SQL: "-- ONWARDPG TODO: choose the durable application-facing name for " + quote(shadow) + ".\n" +
						"-- Install reviewed synchronization/conflict behavior and backfill " + quote(before.Name) + " -> " + quote(shadow) + ".\n" +
						"-- Add boolean verification assertions; do not infer a reverse transform.",
				},
				{
					Stage: "plan_b_boundary",
					SQL: "-- Create a later onwardpg feature only after deploying code which no longer depends on " + table + "." + quote(before.Name) + ".\n" +
						"-- That later desired DDL may remove the old column and compatibility objects.",
				},
			},
		})
	}
	sort.Slice(guidance, func(i, j int) bool { return guidance[i].Key < guidance[j].Key })
	return guidance
}

// partitionReconfigurationGuidance turns the otherwise operator-authored
// partition handoff into a bounded, schema-aware runbook. It remains guidance:
// copying and dual-write semantics are product decisions, and old hierarchy
// cleanup must be authorized by a later fingerprint-bound plan rather than
// smuggled into this one.
func partitionReconfigurationGuidance(changes []change.Change, current, desired *pgschema.Snapshot, options Options) []protocol.Guidance {
	var guidance []protocol.Guidance
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindTable {
			continue
		}
		before, beforeOK := item.Before.(pgschema.Table)
		after, afterOK := item.After.(pgschema.Table)
		if !beforeOK || !afterOK || (!partitionReconfiguration(before.PartitionOf, after.PartitionOf) && reflect.DeepEqual(before.Partition, after.Partition)) {
			continue
		}
		if reflect.DeepEqual(before.Partition, after.Partition) {
			guidance = append(guidance, partitionBoundGuidance(before, after, current, desired))
			continue
		}
		guidance = append(guidance, partitionTopologyShadowGuidance(before, after, current, desired, options))
	}
	sort.Slice(guidance, func(i, j int) bool { return guidance[i].Key < guidance[j].Key })
	return guidance
}

func partitionBoundGuidance(before, after pgschema.Table, current, desired *pgschema.Snapshot) protocol.Guidance {
	constraint := partitionTemporaryName("bound", before.ObjectID(), after.ObjectID())
	oldParent, newParent := "detached", "detached"
	oldBound, newBound := "unattached", "unattached"
	if before.PartitionOf != nil {
		oldParent, oldBound = qualified(before.PartitionOf.Parent.Schema, before.PartitionOf.Parent.Name), before.PartitionOf.Bound
	}
	if after.PartitionOf != nil {
		newParent, newBound = qualified(after.PartitionOf.Parent.Schema, after.PartitionOf.Parent.Name), after.PartitionOf.Bound
	}
	table := qualified(after.Schema, after.Name)
	steps := []protocol.GuidanceStep{
		{Stage: "topology_inventory", SQL: partitionTreeInventory(current, before.ObjectID(), "current") + "\n" + partitionTreeInventory(desired, after.ObjectID(), "desired")},
		{Stage: "prevalidate_new_bound", SQL: "-- ONWARDPG TODO: replace <new-bound-predicate> with the exact boolean predicate equivalent to " + newBound + ".\n" +
			"ALTER TABLE " + table + " ADD CONSTRAINT " + quote(constraint) + " CHECK (<new-bound-predicate>) NOT VALID;\n" +
			"ALTER TABLE " + table + " VALIDATE CONSTRAINT " + quote(constraint) + ";\n" +
			"-- A validated matching CHECK lets PostgreSQL avoid scanning this child during ATTACH; prove equivalence during review."},
		{Stage: "brief_detach_attach_cutover", SQL: "-- Current parent: " + oldParent + "; desired parent: " + newParent + ".\n" +
			"-- Current bound: " + oldBound + "; desired bound: " + newBound + ".\n" +
			partitionDetachSQL(before) + "\n" + partitionAttachSQL(after)},
		{Stage: "post_cutover_assertions", SQL: "DO $onwardpg$ BEGIN IF EXISTS (SELECT 1 FROM " + table + " WHERE NOT (<new-bound-predicate>)) THEN RAISE EXCEPTION 'onwardpg_partition_bound_holds failed'; END IF; END $onwardpg$;\n" +
			partitionCatalogBoundAssertion(after)},
		{Stage: "separately_authorized_cleanup", SQL: "ALTER TABLE " + table + " DROP CONSTRAINT " + quote(constraint) + ";\n" +
			"-- Do not drop or truncate detached/populated relations here. Any destructive cleanup belongs to a later fingerprint-bound onwardpg plan."},
	}
	return protocol.Guidance{
		Kind: "partition_reconfiguration", Key: after.ObjectID().String(),
		Summary: "Move the partition only after validating its desired bound. The supplied manual contract must preserve rows, account for overlapping/default partitions, and keep the detach/attach lock window brief.",
		Steps:   steps,
	}
}

func partitionTopologyShadowGuidance(before, after pgschema.Table, current, desired *pgschema.Snapshot, options Options) protocol.Guidance {
	currentTree := partitionTreeTables(current, before.ObjectID())
	desiredTree := partitionTreeTables(desired, after.ObjectID())
	shadowNames := partitionTemporaryTableNames("shadow", after.ObjectID(), desiredTree)
	oldNames := partitionTemporaryTableNames("old", before.ObjectID(), currentTree)
	temporaryPreflight := partitionTemporaryIdentityPreflight(current, desired, currentTree, desiredTree, oldNames, shadowNames)

	shadowDDL, shadowIntegrity := partitionShadowDDL(desired, desiredTree, shadowNames)
	shadowIntegrity = partitionKeyCoverageEvidence(desired, desiredTree) + "\n" + shadowIntegrity
	copyColumns := partitionCopyColumns(desired, after.ObjectID())
	columns := joinQuoted(copyColumns)
	copySQL := "-- ONWARDPG TODO: choose batching, throttling, retry, and conflict policy for this data volume.\n"
	if len(copyColumns) == 0 {
		copySQL += "-- No insertable columns were proven; supply a reviewed projection for generated/identity/sequence behavior."
	} else {
		copySQL += "INSERT INTO " + qualified(after.Schema, shadowNames[after.ObjectID()]) + " (" + columns + ") OVERRIDING SYSTEM VALUE\n" +
			"SELECT " + columns + " FROM " + qualified(before.Schema, before.Name) + ";"
	}

	dependencyDrop, dependencyRecreate := partitionTypedDependencyClosure(current, desired, before.ObjectID(), after.ObjectID(), options)
	cutover := partitionCutoverSQL(current, desired, currentTree, desiredTree, oldNames, shadowNames)
	if dependencyDrop != "" {
		cutover = dependencyDrop + "\n" + cutover
	}
	if dependencyRecreate != "" {
		cutover += "\n" + dependencyRecreate
	}

	rootBefore := qualified(before.Schema, before.Name)
	rootShadow := qualified(after.Schema, shadowNames[after.ObjectID()])
	assertions := []string{
		"DO $onwardpg$ BEGIN\n  IF (SELECT count(*) FROM " + rootBefore + ") <> (SELECT count(*) FROM " + rootShadow + ") THEN\n    RAISE EXCEPTION 'onwardpg_row_count_matches failed';\n  END IF;\n  IF EXISTS ((TABLE " + rootBefore + " EXCEPT ALL TABLE " + rootShadow + ") UNION ALL (TABLE " + rootShadow + " EXCEPT ALL TABLE " + rootBefore + ")) THEN\n    RAISE EXCEPTION 'onwardpg_rows_match failed';\n  END IF;\nEND $onwardpg$;",
	}
	for _, table := range desiredTree {
		if table.PartitionOf != nil {
			assertions = append(assertions, partitionCatalogBoundAssertionWithName(table, shadowNames[table.ObjectID()]))
		}
	}

	cleanup := "-- Requires a separate, current-catalog fingerprint authorization after rollback retention expires:\n-- DROP TABLE " + qualified(before.Schema, oldNames[before.ObjectID()]) + " CASCADE;\n" +
		"-- Never infer production copy duration, write-catch-up tolerance, or a safe rollback-retention interval from disposable-clone convergence."
	return protocol.Guidance{
		Kind: "partition_reconfiguration", Key: after.ObjectID().String(),
		Summary: "Build the complete desired hierarchy under deterministic shadow identities, synchronize and verify retained data, perform one brief rename cutover, and retain the old populated hierarchy until separately authorized cleanup.",
		Steps: []protocol.GuidanceStep{
			{Stage: "topology_inventory", SQL: partitionTreeInventory(current, before.ObjectID(), "current") + "\n" + partitionTreeInventory(desired, after.ObjectID(), "desired")},
			{Stage: "temporary_identity_preflight", SQL: temporaryPreflight},
			{Stage: "expand_shadow_hierarchy", SQL: shadowDDL},
			{Stage: "expand_keys_indexes_and_partition_locals", SQL: shadowIntegrity},
			{Stage: "expand_write_synchronization", SQL: "-- ONWARDPG TODO: install reviewed dual-write/capture behavior from " + rootBefore + " to " + rootShadow + ".\n-- Define conflict precedence, delete propagation, sequence/identity ownership, trigger ordering, RLS behavior, and retry semantics; onwardpg cannot infer them from DDL."},
			{Stage: "expand_bulk_copy", SQL: copySQL},
			{Stage: "contract_catch_up", SQL: "-- ONWARDPG TODO: after pre-deployment writers drain, replay the final captured changes idempotently and disable the synchronization path.\n-- Do not proceed to cutover until the assertions in the next stage all return true."},
			{Stage: "contract_cutover_assertions", SQL: strings.Join(assertions, "\n")},
			{Stage: "brief_rename_cutover_and_typed_closure", SQL: cutover},
			{Stage: "separately_authorized_cleanup", SQL: cleanup},
		},
	}
}

func partitionTemporaryIdentityPreflight(current, desired *pgschema.Snapshot, currentTree, desiredTree []pgschema.Table, oldNames, shadowNames map[pgschema.ID]string) string {
	type relationName struct{ schema, name string }
	unique := make(map[relationName]bool)
	for _, table := range currentTree {
		unique[relationName{table.Schema, oldNames[table.ObjectID()]}] = true
	}
	for _, table := range desiredTree {
		unique[relationName{table.Schema, shadowNames[table.ObjectID()]}] = true
	}
	if len(desiredTree) > 0 {
		root := desiredTree[0].ObjectID()
		for _, object := range desired.Objects() {
			switch object := object.(type) {
			case pgschema.Constraint:
				if (object.Type == pgschema.ConstraintPrimary || object.Type == pgschema.ConstraintUnique || object.Type == pgschema.ConstraintExclusion) && shadowNames[object.Table] != "" && object.Parent == nil {
					unique[relationName{object.Table.Schema, partitionTemporaryName("key", root, object.ObjectID())}] = true
				}
			case pgschema.Index:
				if shadowNames[object.Table] != "" && object.Parent == nil && object.Constraint == "" {
					unique[relationName{object.Table.Schema, partitionTemporaryName("idx", root, object.ObjectID())}] = true
				}
			}
		}
	}
	if len(currentTree) > 0 {
		root := currentTree[0].ObjectID()
		for _, object := range current.Objects() {
			switch object := object.(type) {
			case pgschema.Constraint:
				if (object.Type == pgschema.ConstraintPrimary || object.Type == pgschema.ConstraintUnique || object.Type == pgschema.ConstraintExclusion) && oldNames[object.Table] != "" && object.Parent == nil {
					unique[relationName{object.Table.Schema, partitionTemporaryName("oldkey", root, object.ObjectID())}] = true
				}
			case pgschema.Index:
				if oldNames[object.Table] != "" && object.Parent == nil && object.Constraint == "" {
					unique[relationName{object.Table.Schema, partitionTemporaryName("oldidx", root, object.ObjectID())}] = true
				}
			}
		}
	}
	names := make([]relationName, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].schema != names[j].schema {
			return names[i].schema < names[j].schema
		}
		return names[i].name < names[j].name
	})
	var lines []string
	for _, name := range names {
		qualifiedName := qualified(name.schema, name.name)
		lines = append(lines, "DO $onwardpg$ BEGIN IF to_regclass("+literal(qualifiedName)+") IS NOT NULL THEN RAISE EXCEPTION 'reserved partition handoff identity already exists: "+strings.ReplaceAll(qualifiedName, "'", "''")+"'; END IF; END $onwardpg$;")
	}
	return strings.Join(lines, "\n")
}

func partitionTreeTables(snapshot *pgschema.Snapshot, root pgschema.ID) []pgschema.Table {
	if snapshot == nil {
		return nil
	}
	reached := map[pgschema.ID]int{root: 0}
	changed := true
	for changed {
		changed = false
		for _, object := range snapshot.Objects() {
			table, ok := object.(pgschema.Table)
			if !ok || table.PartitionOf == nil {
				continue
			}
			parentDepth, parentReached := reached[table.PartitionOf.Parent]
			if !parentReached {
				continue
			}
			if _, exists := reached[table.ObjectID()]; !exists {
				reached[table.ObjectID()] = parentDepth + 1
				changed = true
			}
		}
	}
	result := make([]pgschema.Table, 0, len(reached))
	for id := range reached {
		if object, exists := snapshot.Object(id); exists {
			if table, ok := object.(pgschema.Table); ok {
				result = append(result, table)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := reached[result[i].ObjectID()], reached[result[j].ObjectID()]
		if left != right {
			return left < right
		}
		return result[i].ObjectID().String() < result[j].ObjectID().String()
	})
	return result
}

func partitionTreeInventory(snapshot *pgschema.Snapshot, root pgschema.ID, label string) string {
	tables := partitionTreeTables(snapshot, root)
	lines := []string{"-- " + label + " partition tree:"}
	if len(tables) == 0 {
		return strings.Join(append(lines, "--   "+root.String()+" (not present)"), "\n")
	}
	for _, table := range tables {
		detail := "ordinary"
		if table.Partition != nil {
			detail = "PARTITION BY " + table.Partition.Raw
		}
		if table.PartitionOf != nil {
			detail += "; PARTITION OF " + table.PartitionOf.Parent.String() + " " + table.PartitionOf.Bound
		}
		lines = append(lines, "--   "+table.ObjectID().String()+" — "+detail)
	}
	return strings.Join(lines, "\n")
}

func partitionTemporaryTableNames(kind string, transition pgschema.ID, tables []pgschema.Table) map[pgschema.ID]string {
	names := make(map[pgschema.ID]string, len(tables))
	for _, table := range tables {
		names[table.ObjectID()] = partitionTemporaryName(kind, transition, table.ObjectID())
	}
	return names
}

func partitionTemporaryName(kind string, transition, object pgschema.ID) string {
	digest := sha256.Sum256([]byte("partition\x00" + kind + "\x00" + transition.String() + "\x00" + object.String()))
	return "onwardpg_" + kind + "_" + hex.EncodeToString(digest[:8])
}

func partitionShadowDDL(snapshot *pgschema.Snapshot, tables []pgschema.Table, names map[pgschema.ID]string) (string, string) {
	var create, integrity []string
	createdTables := make(map[pgschema.ID]bool, len(tables))
	for _, table := range tables {
		createdTables[table.ObjectID()] = true
		name := qualified(table.Schema, names[table.ObjectID()])
		prefix := "CREATE TABLE "
		if table.Unlogged {
			prefix = "CREATE UNLOGGED TABLE "
		}
		if table.PartitionOf == nil {
			columns := tableColumns(snapshot, table.ObjectID())
			definitions := make([]string, 0, len(columns))
			for _, column := range columns {
				definitions = append(definitions, renderColumn(column))
			}
			sql := prefix + name + " (" + strings.Join(definitions, ", ") + ")"
			if table.Partition != nil {
				sql += " PARTITION BY " + table.Partition.Raw
			}
			create = append(create, sql+";")
		} else {
			parentName := names[table.PartitionOf.Parent]
			sql := prefix + name + " PARTITION OF " + qualified(table.PartitionOf.Parent.Schema, parentName) + " " + table.PartitionOf.Bound
			if table.Partition != nil {
				sql += " PARTITION BY " + table.Partition.Raw
			}
			create = append(create, sql+";")
		}
		if table.Owner != "" {
			create = append(create, "ALTER TABLE "+name+" OWNER TO "+quote(table.Owner)+";")
		}
		if table.Comment != nil {
			create = append(create, "COMMENT ON TABLE "+name+" IS "+literal(*table.Comment)+";")
		}
		for _, column := range tableColumns(snapshot, table.ObjectID()) {
			if column.Comment != nil {
				create = append(create, "COMMENT ON COLUMN "+name+"."+quote(column.Name)+" IS "+literal(*column.Comment)+";")
			}
		}
	}

	tree := make(map[pgschema.ID]bool, len(tables))
	for _, table := range tables {
		tree[table.ObjectID()] = true
	}
	for _, object := range snapshot.Objects() {
		switch object := object.(type) {
		case pgschema.Constraint:
			if !tree[object.Table] || object.Parent != nil || object.Type == pgschema.ConstraintForeign {
				continue
			}
			name := object.Name
			if object.Type == pgschema.ConstraintPrimary || object.Type == pgschema.ConstraintUnique || object.Type == pgschema.ConstraintExclusion {
				name = partitionTemporaryName("key", tables[0].ObjectID(), object.ObjectID())
			}
			integrity = append(integrity, "ALTER TABLE "+qualified(object.Table.Schema, names[object.Table])+" ADD CONSTRAINT "+quote(name)+" "+object.Definition+";")
			if object.Comment != nil {
				integrity = append(integrity, "COMMENT ON CONSTRAINT "+quote(name)+" ON "+qualified(object.Table.Schema, names[object.Table])+" IS "+literal(*object.Comment)+";")
			}
		case pgschema.Index:
			if !tree[object.Table] || object.Parent != nil || object.Constraint != "" {
				continue
			}
			shadow := object
			shadow.Table.Name = names[object.Table]
			shadow.Name = partitionTemporaryName("idx", tables[0].ObjectID(), object.ObjectID())
			sql, unsupported := renderIndex(shadow)
			if unsupported != "" {
				integrity = append(integrity, "-- ONWARDPG TODO: render "+object.ObjectID().String()+" after resolving "+unsupported+".")
			} else {
				integrity = append(integrity, strings.TrimSuffix(sql, ";")+";")
				if object.Comment != nil {
					integrity = append(integrity, "COMMENT ON INDEX "+qualified(object.Table.Schema, shadow.Name)+" IS "+literal(*object.Comment)+";")
				}
			}
		}
	}
	if len(integrity) == 0 {
		integrity = append(integrity, "-- No desired primary/unique/check/exclusion constraints or standalone indexes were present in this hierarchy.")
	}
	return strings.Join(create, "\n"), strings.Join(integrity, "\n")
}

func partitionKeyCoverageEvidence(snapshot *pgschema.Snapshot, tables []pgschema.Table) string {
	var lines []string
	for _, table := range tables {
		if table.Partition == nil {
			continue
		}
		var required []string
		provable := len(table.Partition.Parts) > 0
		for _, part := range table.Partition.Parts {
			if part.Column == "" || part.Expression != "" {
				provable = false
				continue
			}
			required = append(required, part.Column)
		}
		if len(required) == 0 {
			if parsed, ok := constraintKeyColumns(table.Partition.Raw); ok {
				required, provable = parsed, true
			}
		}
		for _, object := range snapshot.Objects() {
			constraint, ok := object.(pgschema.Constraint)
			if !ok || constraint.Table != table.ObjectID() || constraint.Parent != nil || (constraint.Type != pgschema.ConstraintPrimary && constraint.Type != pgschema.ConstraintUnique) {
				continue
			}
			columns, parsed := constraintKeyColumns(constraint.Definition)
			covered := provable && parsed
			for _, partitionColumn := range required {
				if !stringIn(columns, partitionColumn) {
					covered = false
				}
			}
			if covered {
				lines = append(lines, "-- PROVED: "+constraint.ObjectID().String()+" includes partition columns ("+strings.Join(required, ", ")+").")
			} else {
				lines = append(lines, "-- BLOCKED: cannot prove "+constraint.ObjectID().String()+" covers every partition column; do not cut over until the desired key is corrected and re-introspected.")
			}
		}
	}
	if len(lines) == 0 {
		return "-- No primary or unique key required partition-column coverage proof."
	}
	return strings.Join(lines, "\n")
}

func partitionCopyColumns(snapshot *pgschema.Snapshot, table pgschema.ID) []string {
	var result []string
	for _, column := range tableColumns(snapshot, table) {
		if column.Generated == nil {
			result = append(result, column.Name)
		}
	}
	return result
}

func joinQuoted(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quote(value)
	}
	return strings.Join(quoted, ", ")
}

func partitionDetachSQL(table pgschema.Table) string {
	if table.PartitionOf == nil {
		return "-- Current table is not attached; no DETACH statement is required."
	}
	return "ALTER TABLE " + qualified(table.PartitionOf.Parent.Schema, table.PartitionOf.Parent.Name) + " DETACH PARTITION " + qualified(table.Schema, table.Name) + ";"
}

func partitionAttachSQL(table pgschema.Table) string {
	if table.PartitionOf == nil {
		return "-- Desired table is not attached; retain it as an ordinary table after DETACH."
	}
	return "ALTER TABLE " + qualified(table.PartitionOf.Parent.Schema, table.PartitionOf.Parent.Name) + " ATTACH PARTITION " + qualified(table.Schema, table.Name) + " " + table.PartitionOf.Bound + ";"
}

func partitionCatalogBoundAssertion(table pgschema.Table) string {
	return partitionCatalogBoundAssertionWithName(table, table.Name)
}

func partitionCatalogBoundAssertionWithName(table pgschema.Table, relationName string) string {
	if table.PartitionOf == nil {
		return "-- Desired relation is unattached; no catalog partition bound is expected."
	}
	return "DO $onwardpg$ DECLARE actual_bound text; BEGIN SELECT pg_get_expr(c.relpartbound, c.oid) INTO actual_bound FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = " + literal(table.Schema) + " AND c.relname = " + literal(relationName) + "; IF actual_bound IS DISTINCT FROM " + literal(table.PartitionOf.Bound) + " THEN RAISE EXCEPTION 'onwardpg_catalog_bound_matches failed for %.%', " + literal(table.Schema) + ", " + literal(relationName) + "; END IF; END $onwardpg$;"
}

func partitionCutoverSQL(current, desired *pgschema.Snapshot, currentTree, desiredTree []pgschema.Table, oldNames, shadowNames map[pgschema.ID]string) string {
	var sql []string
	currentSet, desiredSet := make(map[pgschema.ID]bool), make(map[pgschema.ID]bool)
	for _, table := range currentTree {
		currentSet[table.ObjectID()] = true
	}
	for _, table := range desiredTree {
		desiredSet[table.ObjectID()] = true
	}
	// Foreign keys retain relation OIDs across a rename. Drop every FK touching
	// the old hierarchy before swapping identities, then recreate the desired
	// definitions only after the shadow hierarchy owns the public names.
	for _, object := range current.Objects() {
		constraint, ok := object.(pgschema.Constraint)
		if !ok || constraint.Type != pgschema.ConstraintForeign || (!currentSet[constraint.Table] && (constraint.Reference == nil || !currentSet[*constraint.Reference])) {
			continue
		}
		sql = append(sql, "ALTER TABLE "+qualified(constraint.Table.Schema, constraint.Table.Name)+" DROP CONSTRAINT "+quote(constraint.Name)+";")
	}
	for _, object := range current.Objects() {
		switch object := object.(type) {
		case pgschema.Constraint:
			if currentSet[object.Table] && object.Parent == nil && (object.Type == pgschema.ConstraintPrimary || object.Type == pgschema.ConstraintUnique || object.Type == pgschema.ConstraintExclusion) {
				sql = append(sql, "ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" RENAME CONSTRAINT "+quote(object.Name)+" TO "+quote(partitionTemporaryName("oldkey", currentTree[0].ObjectID(), object.ObjectID()))+";")
			}
		case pgschema.Index:
			if currentSet[object.Table] && object.Parent == nil && object.Constraint == "" {
				sql = append(sql, "ALTER INDEX "+qualified(object.Table.Schema, object.Name)+" RENAME TO "+quote(partitionTemporaryName("oldidx", currentTree[0].ObjectID(), object.ObjectID()))+";")
			}
		}
	}
	for index := len(currentTree) - 1; index >= 0; index-- {
		table := currentTree[index]
		sql = append(sql, "ALTER TABLE "+qualified(table.Schema, table.Name)+" RENAME TO "+quote(oldNames[table.ObjectID()])+";")
	}
	for _, table := range desiredTree {
		sql = append(sql, "ALTER TABLE "+qualified(table.Schema, shadowNames[table.ObjectID()])+" RENAME TO "+quote(table.Name)+";")
	}
	for _, object := range desired.Objects() {
		switch object := object.(type) {
		case pgschema.Constraint:
			if desiredSet[object.Table] && object.Parent == nil && (object.Type == pgschema.ConstraintPrimary || object.Type == pgschema.ConstraintUnique || object.Type == pgschema.ConstraintExclusion) {
				temporary := partitionTemporaryName("key", desiredTree[0].ObjectID(), object.ObjectID())
				sql = append(sql, "ALTER TABLE "+qualified(object.Table.Schema, object.Table.Name)+" RENAME CONSTRAINT "+quote(temporary)+" TO "+quote(object.Name)+";")
			}
		case pgschema.Index:
			if desiredSet[object.Table] && object.Parent == nil && object.Constraint == "" {
				temporary := partitionTemporaryName("idx", desiredTree[0].ObjectID(), object.ObjectID())
				sql = append(sql, "ALTER INDEX "+qualified(object.Table.Schema, temporary)+" RENAME TO "+quote(object.Name)+";")
			}
		}
	}
	createdTables := make(map[pgschema.ID]bool, len(desiredTree))
	for _, table := range desiredTree {
		createdTables[table.ObjectID()] = true
	}
	for _, object := range desired.Objects() {
		include := false
		switch object := object.(type) {
		case pgschema.Constraint:
			include = object.Type == pgschema.ConstraintForeign && (desiredSet[object.Table] || object.Reference != nil && desiredSet[*object.Reference])
		case pgschema.Trigger:
			include = desiredSet[object.Table]
		case pgschema.Policy:
			include = desiredSet[object.Table]
		case pgschema.RowSecurity:
			include = desiredSet[object.Table]
		case pgschema.ReplicaIdentity:
			include = desiredSet[object.Table]
		case pgschema.TablePrivilege:
			include = desiredSet[object.Table]
		}
		if !include {
			continue
		}
		statements, _, unsupported, err := renderCreate(object, desired, createdTables, Options{})
		if err != nil || len(unsupported) > 0 {
			sql = append(sql, "-- ONWARDPG TODO: recreate "+object.ObjectID().String()+" from its typed desired definition after cutover.")
			continue
		}
		for _, statement := range statements {
			sql = append(sql, statement.SQL)
		}
		if constraint, ok := object.(pgschema.Constraint); ok && constraint.Comment != nil {
			sql = append(sql, "COMMENT ON CONSTRAINT "+quote(constraint.Name)+" ON "+qualified(constraint.Table.Schema, constraint.Table.Name)+" IS "+literal(*constraint.Comment)+";")
		}
	}
	for _, object := range desired.Objects() {
		sequence, ok := object.(pgschema.Sequence)
		if !ok || sequence.OwnedBy == nil {
			continue
		}
		column := *sequence.OwnedBy
		tableID := (pgschema.Table{Schema: column.Schema, Name: column.Name}).ObjectID()
		if !desiredSet[tableID] {
			continue
		}
		sql = append(sql, "ALTER SEQUENCE "+qualified(sequence.Schema, sequence.Name)+" OWNED BY "+qualified(column.Schema, column.Name)+"."+quote(column.Part)+";")
	}
	return strings.Join(sql, "\n")
}

func partitionTypedDependencyClosure(current, desired *pgschema.Snapshot, currentRoot, desiredRoot pgschema.ID, options Options) (string, string) {
	currentViews := dependentViewsForRoot(current, currentRoot)
	desiredViews := dependentViewsForRoot(desired, desiredRoot)
	for id := range currentViews {
		if object, exists := desired.Object(id); exists {
			if _, ok := object.(pgschema.View); ok {
				desiredViews[id] = true
			}
		}
	}
	desiredViews = dependentViewsForViewSeeds(desired, desiredViews)
	currentOrder, currentUnsupported := orderedTypedDerivedViews(current, currentViews, currentRoot)
	desiredOrder, desiredUnsupported := orderedTypedDerivedViews(desired, desiredViews, desiredRoot)
	if len(currentUnsupported) > 0 || len(desiredUnsupported) > 0 {
		return "-- ONWARDPG TODO: typed dependent-view closure could not be ordered: " + strings.Join(unionStrings(currentUnsupported, desiredUnsupported), ", ") + ".", ""
	}
	var drops, recreates []string
	for index := len(currentOrder) - 1; index >= 0; index-- {
		for _, statement := range renderDrop(currentOrder[index].view) {
			drops = append(drops, statement.SQL)
		}
	}
	for _, node := range desiredOrder {
		statements, _, unsupported, err := renderCreate(node.view, desired, nil, options)
		if err != nil || len(unsupported) > 0 {
			recreates = append(recreates, "-- ONWARDPG TODO: recreate "+node.view.ObjectID().String()+" from its typed desired definition.")
			continue
		}
		for _, statement := range statements {
			recreates = append(recreates, statement.SQL)
		}
		for _, object := range desired.Objects() {
			associated := false
			switch object := object.(type) {
			case pgschema.Index:
				associated = object.Table == node.view.ObjectID()
			case pgschema.Trigger:
				associated = object.Table == node.view.ObjectID()
			case pgschema.TablePrivilege:
				associated = object.Table == node.view.ObjectID()
			}
			if !associated {
				continue
			}
			created, _, _, err := renderCreate(object, desired, nil, options)
			if err == nil {
				for _, statement := range created {
					recreates = append(recreates, statement.SQL)
				}
			}
		}
	}
	return strings.Join(drops, "\n"), strings.Join(recreates, "\n")
}

func splitPlanShadowColumnName(before, after pgschema.Column) string {
	digest := sha256.Sum256([]byte(before.ObjectID().String() + "\x00" + before.Type + "\x00" + after.Type))
	return "onwardpg_next_" + hex.EncodeToString(digest[:6])
}

func renderColumnModify(before, after pgschema.Column, choices decisions, current, desired *pgschema.Snapshot, options Options) ([]protocol.Statement, []pgschema.ID, []string, error) {
	var statements []protocol.Statement
	var deferredDerivedRecreate []protocol.Statement
	var consumed []pgschema.ID
	table := qualified(after.Table.Schema, after.Table.Name)
	column := quote(after.Name)
	if before.Collation != after.Collation {
		target := after.Type
		if after.Collation != "" {
			target += " COLLATE " + after.Collation
		}
		statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" TYPE "+target+" USING "+column+"::"+after.Type+";", "contract", "review", true, "column_collation_change", "table_rewrite_possible", "access_exclusive_lock"))
	}
	if !reflect.DeepEqual(before.Serial, after.Serial) {
		if !serialColumnSupported(before) || !serialColumnSupported(after) {
			return nil, nil, []string{"serial_sequence_name:" + after.ObjectID().String()}, nil
		}
		serialStatements, _, unsupported, err := renderSerialModify(before, after)
		if err != nil || len(unsupported) > 0 {
			return serialStatements, nil, unsupported, err
		}
		statements = append(statements, serialStatements...)
	}
	if before.Generated != nil || after.Generated != nil {
		switch {
		case before.Generated != nil && after.Generated == nil:
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP EXPRESSION;", "contract", "review", true, "table_lock"))
		case before.Generated == nil && after.Generated != nil:
			return nil, nil, []string{"generated_column_kind_change:" + after.ObjectID().String()}, nil
		case before.Generated.Kind != after.Generated.Kind:
			return nil, nil, []string{"generated_column_kind_change:" + after.ObjectID().String()}, nil
		case before.Generated.Expression != after.Generated.Expression:
			switch after.Generated.Kind {
			case "VIRTUAL":
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET EXPRESSION AS ("+after.Generated.Expression+");", "contract", "review", true, "generated_expression_change", "table_lock"))
			case "STORED":
				statements = append(statements,
					statement("ALTER TABLE "+table+" DROP COLUMN "+column+";", "contract", "review", true, "generated_expression_rebuild", "table_lock"),
					statement("ALTER TABLE "+table+" ADD COLUMN "+renderColumn(after)+";", "contract", "review", true, "generated_expression_rebuild", "table_rewrite_possible", "table_lock"),
				)
				if after.Comment != nil {
					statements = append(statements, statement("COMMENT ON COLUMN "+table+"."+column+" IS "+literal(*after.Comment)+";", "contract", "safe", true, "generated_expression_rebuild"))
				}
				return statements, nil, nil, nil
			default:
				return nil, nil, []string{"generated_column_kind:" + after.ObjectID().String()}, nil
			}
		}
	}
	if generatedColumnTypeChange(before, after) {
		statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" TYPE "+after.Type+";", "contract", "review", true, "generated_type_change", "table_rewrite_possible", "access_exclusive_lock"))
	}
	identityHandledDefault := false
	identityHandledNotNull := false
	if !reflect.DeepEqual(before.Identity, after.Identity) {
		switch {
		case before.Identity == nil && after.Identity != nil:
			if before.Default != nil {
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT;", "contract", "review", true, "identity_replaces_default"))
				identityHandledDefault = true
			}
			if !before.NotNull && after.NotNull {
				notNullStatements, err := renderSetNotNull(after, choices, current, desired)
				if err != nil {
					return nil, nil, nil, err
				}
				statements = append(statements, notNullStatements...)
				identityHandledNotNull = true
			}
			options, unsupported := renderIdentityOptions(*after.Identity, false)
			if unsupported != "" {
				return nil, nil, []string{unsupported + ":" + after.ObjectID().String()}, nil
			}
			sql := "ALTER TABLE " + table + " ALTER COLUMN " + column + " ADD GENERATED " + after.Identity.Generation + " AS IDENTITY (" + options + ");"
			statements = append(statements, statement(sql, "contract", "review", true, "identity_sequence_create", "table_lock"))
		case before.Identity != nil && after.Identity == nil:
			if !choices.identityDrop[after.ObjectID()] {
				return nil, nil, []string{"identity_drop_unconfirmed:" + after.ObjectID().String()}, nil
			}
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP IDENTITY;", "contract", "dangerous", true, "identity_sequence_drop", "data_loss", "table_lock"))
			if before.NotNull && !after.NotNull {
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP NOT NULL;", "contract", "safe", true))
				identityHandledNotNull = true
			}
			if after.Default != nil {
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET DEFAULT "+*after.Default+";", "contract", "review", true, "identity_replacement_default"))
			}
			identityHandledDefault = true
		case before.Identity != nil && after.Identity != nil:
			if before.Identity.Generation != after.Identity.Generation {
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET GENERATED "+after.Identity.Generation+";", "contract", "review", true, "table_lock"))
			}
			if before.Identity.Start != after.Identity.Start || before.Identity.Increment != after.Identity.Increment || !reflect.DeepEqual(before.Identity.Min, after.Identity.Min) || !reflect.DeepEqual(before.Identity.Max, after.Identity.Max) || before.Identity.Cache != after.Identity.Cache || before.Identity.Cycle != after.Identity.Cycle {
				options, unsupported := renderIdentityOptions(*after.Identity, true)
				if unsupported != "" {
					return nil, nil, []string{unsupported + ":" + after.ObjectID().String()}, nil
				}
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" "+options+";", "contract", "review", true, "sequence_state"))
			}
		}
	}
	if before.Serial == nil && after.Serial == nil && before.Type != after.Type && !generatedColumnTypeChange(before, after) {
		using, exists := choices.typeUsing[after.ObjectID()]
		if !exists {
			return nil, nil, nil, fmt.Errorf("missing type-change decision for %s", after.ObjectID())
		}
		if using == "manual_sql" {
			derivedDrops, derivedRecreate, derivedConsumed, derivedUnsupported, err := renderColumnDerivedViewClosure(before, after, current, desired, options)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(derivedUnsupported) > 0 {
				return nil, nil, derivedUnsupported, nil
			}
			deferredDerivedRecreate = derivedRecreate
			consumed = append(consumed, derivedConsumed...)
			dependentViews := dependentViewAndIndexIDs(current, desired, after.ObjectID())
			dependencyGuidance := ""
			if len(dependentViews) > 0 {
				dependencyGuidance = "\n-- Dependent view/materialized-view/index objects in scope: " + strings.Join(dependentViews, ", ") + ". Preserve or recreate this exact dependency closure in the reviewed bridge SQL."
				if opaqueRoutines := boundedRoutineReviewIDs(current, desired, 25); len(opaqueRoutines) > 0 {
					dependencyGuidance += "\n-- Opaque routine bodies are outside PostgreSQL catalog dependency proof; review these bounded candidates separately: " + strings.Join(opaqueRoutines, ", ") + "."
				}
			}
			statements = append(statements,
				statement(
					"-- ONWARDPG TODO: replace this comment with reviewed EXPAND SQL for "+after.ObjectID().String()+" ("+before.Type+" -> "+after.Type+").\n"+
						"-- Establish both old and new interfaces, synchronization/conflict behavior, and any initial backfill while old code is live.\n"+
						"-- Do not use a direct ALTER TYPE here: this plan surrounds one rolling application deployment."+dependencyGuidance,
					protocol.PhaseExpand, "manual", true, "manual_sql", "single_deployment_bridge_required", "product_semantics_required",
				),
			)
			statements = append(statements, derivedDrops...)
			if before.Default != nil {
				statements = append(statements, statement(
					"ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT;",
					protocol.PhaseContract, "review", true, "type_change_default_choreography", "access_exclusive_lock",
				))
			}
			statements = append(statements,
				statement(
					"-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for "+after.ObjectID().String()+" ("+before.Type+" -> "+after.Type+").\n"+
						"-- After pre-deployment writers drain, perform final catch-up/assertions, remove compatibility objects, and converge to PostgreSQL type "+after.Type+".\n"+
						"-- Required mutation shape: convert "+table+"."+column+" to "+after.Type+" with a reviewed expression; do not rely on an inferred cast.\n"+
						"-- Add boolean assertions to verify.sql for every data-dependent conversion assumption.",
					protocol.PhaseContract, "manual", true, "manual_sql", "table_rewrite_possible", "access_exclusive_lock", "single_deployment_bridge_required",
				),
				statement(
					"ANALYZE "+table+" ("+column+");",
					protocol.PhaseContract, "review", true, "type_change_statistics_refresh", "database_performance_impact",
				),
			)
			if after.Default != nil {
				statements = append(statements, statement(
					"ALTER TABLE "+table+" ALTER COLUMN "+column+" SET DEFAULT "+*after.Default+";",
					protocol.PhaseContract, "safe", true, "type_change_default_choreography",
				))
			}
			identityHandledDefault = true
		} else if using == "split_plan" {
			return nil, nil, []string{"two_application_deployments_required:" + after.ObjectID().String()}, nil
		} else {
			sql := "ALTER TABLE " + table + " ALTER COLUMN " + column + " TYPE " + after.Type
			if using != "direct" {
				sql += " USING " + using
			}
			statements = append(statements, statement(sql+";", "contract", "review", true, "table_rewrite_possible", "access_exclusive_lock"))
		}
	}
	if !identityHandledDefault && reflect.DeepEqual(before.Serial, after.Serial) && !reflect.DeepEqual(before.Default, after.Default) {
		phase := "contract"
		// PostgreSQL validates/coerces a default against the column's current
		// type. Keep it in the same ordered migration phase after TYPE so a
		// combined type/default change is executable in one plan.
		if after.Default == nil {
			phase = "contract"
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT;", phase, "safe", true))
		} else {
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET DEFAULT "+*after.Default+";", phase, "safe", true))
		}
	}
	if before.NotNull != after.NotNull && !identityHandledNotNull {
		if !after.NotNull {
			phase := "expand"
			hazards := []string(nil)
			if droppingPrimaryKeyForColumn(current, desired, after.ObjectID()) {
				phase = "contract"
				hazards = append(hazards, "after_primary_key_drop")
			}
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP NOT NULL;", phase, "safe", true, hazards...))
		} else {
			notNullStatements, err := renderSetNotNull(after, choices, current, desired)
			if err != nil {
				return nil, nil, nil, err
			}
			statements = append(statements, notNullStatements...)
		}
	}
	statements = append(statements, commentModification("COLUMN", table+"."+column, before.Comment, after.Comment)...)
	statements = append(statements, deferredDerivedRecreate...)
	return statements, consumed, nil, nil
}

func renderSetNotNull(after pgschema.Column, choices decisions, current, desired *pgschema.Snapshot) ([]protocol.Statement, error) {
	table := qualified(after.Table.Schema, after.Table.Name)
	column := quote(after.Name)
	strategy, exists := choices.notNullStrategy[after.ObjectID()]
	if !exists {
		return nil, fmt.Errorf("missing NOT NULL strategy for %s", after.ObjectID())
	}
	switch strategy {
	case "direct":
		return []protocol.Statement{
			statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "table_scan", "access_exclusive_lock"),
		}, nil
	case "staged":
		if check, reusable := preservedValidatedNotNullCheck(current, desired, after); reusable {
			return []protocol.Statement{
				statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "access_exclusive_lock", "validated_check_reuse:"+check),
			}, nil
		}
		check := notNullCheckName(after.ObjectID())
		return []protocol.Statement{
			statement("ALTER TABLE "+table+" ADD CONSTRAINT "+quote(check)+" CHECK ("+column+" IS NOT NULL) NOT VALID;", "contract", "review", true, "share_row_exclusive_lock", "post_drain_writers_required"),
			statement("ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(check)+";", "contract", "review", true, "table_scan"),
			statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "access_exclusive_lock"),
			statement("ALTER TABLE "+table+" DROP CONSTRAINT "+quote(check)+";", "contract", "review", true, "access_exclusive_lock"),
		}, nil
	case "staged_with_backfill":
		work, exists := choices.notNullBackfill[after.ObjectID()]
		if !exists {
			return nil, fmt.Errorf("missing NOT NULL backfill for %s", after.ObjectID())
		}
		check := notNullCheckName(after.ObjectID())
		return []protocol.Statement{
			statement("ALTER TABLE "+table+" ADD CONSTRAINT "+quote(check)+" CHECK ("+column+" IS NOT NULL) NOT VALID;", "contract", "review", true, "share_row_exclusive_lock", "post_drain_writers_required"),
			manualWorkStatement(work, "application_backfill", "data_movement"),
			statement("ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(check)+";", "contract", "review", true, "table_scan"),
			statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "access_exclusive_lock"),
			statement("ALTER TABLE "+table+" DROP CONSTRAINT "+quote(check)+";", "contract", "review", true, "access_exclusive_lock"),
		}, nil
	default:
		return nil, fmt.Errorf("invalid NOT NULL strategy %q", strategy)
	}
}

// preservedValidatedNotNullCheck returns a catalog-proven check which already
// establishes the desired column invariant and remains present after the
// migration. PostgreSQL can use such a check to avoid rescanning the table
// while SET NOT NULL holds its brief catalog lock. The recognizer is
// deliberately narrow: compound predicates and checks which are dropped or
// changed never qualify.
func preservedValidatedNotNullCheck(current, desired *pgschema.Snapshot, column pgschema.Column) (string, bool) {
	if current == nil || desired == nil {
		return "", false
	}
	for _, object := range current.Objects() {
		before, ok := object.(pgschema.Constraint)
		if !ok || before.Table != column.Table || before.Type != pgschema.ConstraintCheck || !before.Validated || before.NoInherit || !isSimpleNotNullCheck(before.Definition, column.Name) {
			continue
		}
		afterObject, exists := desired.Object(before.ObjectID())
		after, ok := afterObject.(pgschema.Constraint)
		if !exists || !ok || after.Type != pgschema.ConstraintCheck || !after.Validated || after.NoInherit || !isSimpleNotNullCheck(after.Definition, column.Name) {
			continue
		}
		return before.Name, true
	}
	return "", false
}

type typedDerivedView struct {
	view  pgschema.View
	depth int
}

type columnDerivedViewClosure struct {
	current     []typedDerivedView
	desired     []typedDerivedView
	objectIDs   map[pgschema.ID]bool
	unsupported []string
}

func coordinatedColumnDerivedChangeIDs(changes []change.Change, current, desired *pgschema.Snapshot, choices decisions) map[pgschema.ID]bool {
	result := make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind != change.Modify || item.ID.Kind != pgschema.KindColumn || choices.typeUsing[item.ID] != "manual_sql" {
			continue
		}
		before, beforeOK := item.Before.(pgschema.Column)
		after, afterOK := item.After.(pgschema.Column)
		if !beforeOK || !afterOK || before.Type == after.Type {
			continue
		}
		closure := buildColumnDerivedViewClosure(before, after, current, desired)
		if len(closure.unsupported) > 0 {
			continue
		}
		for _, candidate := range changes {
			if closure.objectIDs[candidate.ID] {
				result[candidate.ID] = true
			}
		}
	}
	return result
}

func renderColumnDerivedViewClosure(before, after pgschema.Column, current, desired *pgschema.Snapshot, options Options) ([]protocol.Statement, []protocol.Statement, []pgschema.ID, []string, error) {
	closure := buildColumnDerivedViewClosure(before, after, current, desired)
	if len(closure.unsupported) > 0 {
		return nil, nil, nil, closure.unsupported, nil
	}
	var drops []protocol.Statement
	for index := len(closure.current) - 1; index >= 0; index-- {
		view := closure.current[index].view
		hazards := []string{"derived_object_rebuild", "blocking_lock"}
		if view.Materialized {
			hazards = append(hazards, "materialized_view_rebuild", "stored_data_recomputed")
		}
		drops = append(drops, withPhase(renderDrop(view), protocol.PhaseContract, "review", hazards...)...)
	}
	var recreates []protocol.Statement
	for _, node := range closure.desired {
		view := node.view
		created, _, unsupported, err := renderCreate(view, desired, nil, options)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if len(unsupported) > 0 {
			return nil, nil, nil, unsupported, nil
		}
		hazards := []string{"derived_object_rebuild", "blocking_lock"}
		if view.Materialized {
			hazards = append(hazards, "materialized_view_rebuild", "stored_data_recomputed")
		}
		recreates = append(recreates, withPhase(created, protocol.PhaseContract, "review", hazards...)...)
		for _, relationIndex := range indexesForRelation(desired, view.ObjectID()) {
			indexStatements, _, indexUnsupported, err := renderCreate(relationIndex, desired, nil, options)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if len(indexUnsupported) > 0 {
				return nil, nil, nil, indexUnsupported, nil
			}
			recreates = append(recreates, withPhase(indexStatements, protocol.PhaseContract, "review", "derived_object_rebuild", "materialized_view_index_rebuild", "index_build")...)
		}
		for _, trigger := range triggersForRelation(desired, view.ObjectID()) {
			triggerStatements, _, triggerUnsupported, err := renderCreate(trigger, desired, nil, options)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if len(triggerUnsupported) > 0 {
				return nil, nil, nil, triggerUnsupported, nil
			}
			recreates = append(recreates, withPhase(triggerStatements, protocol.PhaseContract, "review", "derived_object_rebuild", "typed_trigger_dependency")...)
		}
		for _, privilege := range privilegesForRelation(desired, view.ObjectID()) {
			privilegeStatements, _, privilegeUnsupported, err := renderCreate(privilege, desired, nil, options)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if len(privilegeUnsupported) > 0 {
				return nil, nil, nil, privilegeUnsupported, nil
			}
			recreates = append(recreates, withPhase(privilegeStatements, protocol.PhaseContract, "review", "derived_object_rebuild", "relation_privilege_preserved")...)
		}
	}
	consumed := make([]pgschema.ID, 0, len(closure.objectIDs))
	for id := range closure.objectIDs {
		consumed = append(consumed, id)
	}
	sort.Slice(consumed, func(i, j int) bool { return consumed[i].String() < consumed[j].String() })
	return drops, recreates, consumed, nil, nil
}

func buildColumnDerivedViewClosure(before, after pgschema.Column, current, desired *pgschema.Snapshot) columnDerivedViewClosure {
	closure := columnDerivedViewClosure{objectIDs: make(map[pgschema.ID]bool)}
	if current == nil || desired == nil || before.Table != after.Table || before.ObjectID() != after.ObjectID() {
		closure.unsupported = []string{"derived_view_closure_snapshot:" + after.ObjectID().String()}
		return closure
	}
	currentViews := dependentViewsForRoot(current, before.ObjectID())
	desiredViews := dependentViewsForRoot(desired, after.ObjectID())
	desiredSeeds := make(map[pgschema.ID]bool)
	for id := range desiredViews {
		desiredSeeds[id] = true
	}
	for id := range currentViews {
		if object, exists := desired.Object(id); exists {
			if _, ok := object.(pgschema.View); ok {
				desiredSeeds[id] = true
			}
		}
	}
	desiredViews = dependentViewsForViewSeeds(desired, desiredSeeds)
	closure.current, closure.unsupported = orderedTypedDerivedViews(current, currentViews, after.ObjectID())
	if len(closure.unsupported) > 0 {
		return closure
	}
	closure.desired, closure.unsupported = orderedTypedDerivedViews(desired, desiredViews, after.ObjectID())
	if len(closure.unsupported) > 0 {
		return closure
	}
	for id := range currentViews {
		closure.objectIDs[id] = true
		for _, associated := range derivedRelationSecondaryObjectIDs(current, id) {
			closure.objectIDs[associated] = true
		}
		for _, dependentID := range current.IDs() {
			if dependentID == id || !containsID(current.Dependencies(dependentID), id) || currentViews[dependentID] {
				continue
			}
			object, _ := current.Object(dependentID)
			supported := false
			switch object := object.(type) {
			case pgschema.Index:
				supported = object.Table == id
			case pgschema.Trigger:
				supported = object.Table == id
			case pgschema.TablePrivilege:
				supported = object.Table == id
			}
			if !supported {
				closure.unsupported = append(closure.unsupported, "derived_view_external_dependent:"+dependentID.String())
			}
		}
	}
	for id := range desiredViews {
		closure.objectIDs[id] = true
		for _, associated := range derivedRelationSecondaryObjectIDs(desired, id) {
			closure.objectIDs[associated] = true
		}
	}
	closure.unsupported = unionStrings(closure.unsupported, nil)
	return closure
}

func dependentViewsForRoot(snapshot *pgschema.Snapshot, root pgschema.ID) map[pgschema.ID]bool {
	reached := map[pgschema.ID]bool{root: true}
	if root.Kind == pgschema.KindColumn {
		reached[(pgschema.Table{Schema: root.Schema, Name: root.Name}).ObjectID()] = true
	} else if root.Kind == pgschema.KindTable {
		for _, object := range snapshot.Objects() {
			if column, ok := object.(pgschema.Column); ok && column.Table == root {
				reached[column.ObjectID()] = true
			}
		}
	}
	views := make(map[pgschema.ID]bool)
	changed := true
	for changed {
		changed = false
		for _, object := range snapshot.Objects() {
			view, ok := object.(pgschema.View)
			if !ok || views[view.ObjectID()] {
				continue
			}
			for _, dependency := range snapshot.Dependencies(view.ObjectID()) {
				if reached[dependency] {
					views[view.ObjectID()] = true
					reached[view.ObjectID()] = true
					changed = true
					break
				}
			}
		}
	}
	return views
}

func dependentViewsForViewSeeds(snapshot *pgschema.Snapshot, seeds map[pgschema.ID]bool) map[pgschema.ID]bool {
	views := make(map[pgschema.ID]bool, len(seeds))
	for id := range seeds {
		views[id] = true
	}
	changed := true
	for changed {
		changed = false
		for _, object := range snapshot.Objects() {
			view, ok := object.(pgschema.View)
			if !ok || views[view.ObjectID()] {
				continue
			}
			for _, dependency := range snapshot.Dependencies(view.ObjectID()) {
				if views[dependency] {
					views[view.ObjectID()] = true
					changed = true
					break
				}
			}
		}
	}
	return views
}

func orderedTypedDerivedViews(snapshot *pgschema.Snapshot, included map[pgschema.ID]bool, root pgschema.ID) ([]typedDerivedView, []string) {
	depths := make(map[pgschema.ID]int, len(included))
	remaining := make(map[pgschema.ID]bool, len(included))
	for id := range included {
		remaining[id] = true
	}
	for len(remaining) > 0 {
		progress := false
		for id := range remaining {
			object, exists := snapshot.Object(id)
			view, ok := object.(pgschema.View)
			if !exists || !ok {
				return nil, []string{"derived_view_closure_object:" + id.String()}
			}
			depth := 1
			ready := true
			for _, dependency := range snapshot.Dependencies(id) {
				if !included[dependency] {
					continue
				}
				dependencyDepth, resolved := depths[dependency]
				if !resolved {
					ready = false
					break
				}
				if dependencyDepth+1 > depth {
					depth = dependencyDepth + 1
				}
			}
			if !ready {
				continue
			}
			depths[id] = depth
			delete(remaining, id)
			progress = true
			_ = view
		}
		if !progress {
			return nil, []string{"derived_view_dependency_cycle:" + root.String()}
		}
	}
	result := make([]typedDerivedView, 0, len(included))
	for id, depth := range depths {
		object, _ := snapshot.Object(id)
		result = append(result, typedDerivedView{view: object.(pgschema.View), depth: depth})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].depth != result[j].depth {
			return result[i].depth < result[j].depth
		}
		return result[i].view.ObjectID().String() < result[j].view.ObjectID().String()
	})
	return result, nil
}

func derivedRelationSecondaryObjectIDs(snapshot *pgschema.Snapshot, relation pgschema.ID) []pgschema.ID {
	var ids []pgschema.ID
	for _, object := range snapshot.Objects() {
		switch object := object.(type) {
		case pgschema.Index:
			if object.Table == relation {
				ids = append(ids, object.ObjectID())
			}
		case pgschema.Trigger:
			if object.Table == relation {
				ids = append(ids, object.ObjectID())
			}
		case pgschema.TablePrivilege:
			if object.Table == relation {
				ids = append(ids, object.ObjectID())
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func triggersForRelation(snapshot *pgschema.Snapshot, relation pgschema.ID) []pgschema.Trigger {
	var triggers []pgschema.Trigger
	for _, object := range snapshot.Objects() {
		trigger, ok := object.(pgschema.Trigger)
		if ok && trigger.Table == relation {
			triggers = append(triggers, trigger)
		}
	}
	sort.Slice(triggers, func(i, j int) bool { return triggers[i].ObjectID().String() < triggers[j].ObjectID().String() })
	return triggers
}

func privilegesForRelation(snapshot *pgschema.Snapshot, relation pgschema.ID) []pgschema.TablePrivilege {
	var privileges []pgschema.TablePrivilege
	for _, object := range snapshot.Objects() {
		privilege, ok := object.(pgschema.TablePrivilege)
		if ok && privilege.Table == relation {
			privileges = append(privileges, privilege)
		}
	}
	sort.Slice(privileges, func(i, j int) bool { return privileges[i].ObjectID().String() < privileges[j].ObjectID().String() })
	return privileges
}

func isSimpleNotNullCheck(definition, columnName string) bool {
	definition = strings.TrimSpace(definition)
	if len(definition) < len("CHECK") || !strings.EqualFold(definition[:len("CHECK")], "CHECK") {
		return false
	}
	expression := strings.TrimSpace(definition[len("CHECK"):])
	if strings.HasSuffix(strings.ToUpper(expression), " NOT VALID") {
		expression = strings.TrimSpace(expression[:len(expression)-len(" NOT VALID")])
	}
	for {
		stripped, ok := stripOuterSQLParentheses(expression)
		if !ok {
			break
		}
		expression = stripped
	}
	compact := strings.Join(strings.Fields(expression), "")
	const predicate = "ISNOTNULL"
	if len(compact) <= len(predicate) || !strings.EqualFold(compact[len(compact)-len(predicate):], predicate) {
		return false
	}
	identifier := compact[:len(compact)-len(predicate)]
	quotedIdentifier := strings.Join(strings.Fields(quote(columnName)), "")
	return identifier == strings.Join(strings.Fields(columnName), "") || identifier == quotedIdentifier
}

func stripOuterSQLParentheses(expression string) (string, bool) {
	expression = strings.TrimSpace(expression)
	if len(expression) < 2 || expression[0] != '(' || expression[len(expression)-1] != ')' {
		return expression, false
	}
	depth := 0
	inQuote := false
	for index := 0; index < len(expression); index++ {
		character := expression[index]
		if character == '\'' {
			if inQuote && index+1 < len(expression) && expression[index+1] == '\'' {
				index++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch character {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && index != len(expression)-1 {
				return expression, false
			}
			if depth < 0 {
				return expression, false
			}
		}
	}
	if depth != 0 || inQuote {
		return expression, false
	}
	return strings.TrimSpace(expression[1 : len(expression)-1]), true
}

func droppingPrimaryKeyForColumn(current, desired *pgschema.Snapshot, column pgschema.ID) bool {
	if current == nil || desired == nil {
		return false
	}
	for _, object := range current.Objects() {
		constraint, ok := object.(pgschema.Constraint)
		if !ok || constraint.Type != pgschema.ConstraintPrimary || !containsID(current.Dependencies(constraint.ObjectID()), column) {
			continue
		}
		afterObject, exists := desired.Object(constraint.ObjectID())
		after, afterIsConstraint := afterObject.(pgschema.Constraint)
		if !exists || !afterIsConstraint || after.Type != pgschema.ConstraintPrimary || after.Definition != constraint.Definition {
			return true
		}
	}
	return false
}

func dependentViewAndIndexIDs(current, desired *pgschema.Snapshot, dependency pgschema.ID) []string {
	views := make(map[pgschema.ID]bool)
	indexes := make(map[pgschema.ID]bool)
	wholeRowDependency := pgschema.ID{}
	if dependency.Kind == pgschema.KindColumn {
		wholeRowDependency = (pgschema.Table{Schema: dependency.Schema, Name: dependency.Name}).ObjectID()
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if snapshot == nil {
			continue
		}
		changed := true
		for changed {
			changed = false
			for _, id := range snapshot.IDs() {
				object, exists := snapshot.Object(id)
				_, ok := object.(pgschema.View)
				if !exists || !ok || views[id] {
					continue
				}
				for _, candidate := range snapshot.Dependencies(id) {
					if candidate == dependency || wholeRowDependency.Kind != "" && candidate == wholeRowDependency || views[candidate] {
						views[id] = true
						changed = true
						break
					}
				}
			}
		}
		for _, id := range snapshot.IDs() {
			object, exists := snapshot.Object(id)
			index, ok := object.(pgschema.Index)
			if exists && ok && views[index.Table] {
				indexes[id] = true
			}
		}
	}
	ids := make([]string, 0, len(views)+len(indexes))
	for id := range views {
		ids = append(ids, id.String())
	}
	for id := range indexes {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	return ids
}

func boundedRoutineReviewIDs(current, desired *pgschema.Snapshot, limit int) []string {
	if limit < 1 {
		return nil
	}
	ids := make(map[string]bool)
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if snapshot == nil {
			continue
		}
		for _, object := range snapshot.Objects() {
			if routine, ok := object.(pgschema.Routine); ok {
				ids[routine.ObjectID().String()] = true
			}
		}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	if len(result) <= limit {
		return result
	}
	omitted := len(result) - limit
	result = append(result[:limit], fmt.Sprintf("... and %d more routines", omitted))
	return result
}

func generatedColumnTypeChange(before, after pgschema.Column) bool {
	return before.Type != after.Type && before.Generated != nil && after.Generated != nil && before.Generated.Kind == after.Generated.Kind && before.Generated.Expression == after.Generated.Expression
}

func renderSerialModify(before, after pgschema.Column) ([]protocol.Statement, []pgschema.ID, []string, error) {
	table := qualified(after.Table.Schema, after.Table.Name)
	column := quote(after.Name)
	var statements []protocol.Statement
	baseType := func(column pgschema.Column) (string, bool) {
		if column.Serial == nil {
			return column.Type, true
		}
		switch column.Serial.Type {
		case "smallserial":
			return "smallint", true
		case "serial":
			return "integer", true
		case "bigserial":
			return "bigint", true
		default:
			return "", false
		}
	}
	fromType, fromOK := baseType(before)
	toType, toOK := baseType(after)
	if !fromOK || !toOK {
		return nil, nil, []string{"serial_type_invalid:" + after.ObjectID().String()}, nil
	}
	sequence := qualified(after.Table.Schema, after.Table.Name+"_"+after.Name+"_seq")
	switch {
	case before.Serial == nil && after.Serial != nil:
		statements = append(statements,
			statement("CREATE SEQUENCE IF NOT EXISTS "+sequence+" OWNED BY "+table+"."+column+";", "expand", "review", true, "sequence_create", "table_lock"),
			statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET DEFAULT nextval("+literal(sequence)+"::regclass);", "expand", "review", true, "table_lock"),
		)
	case before.Serial != nil && after.Serial == nil:
		statements = append(statements,
			statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT;", "contract", "review", true, "table_lock"),
			statement("DROP SEQUENCE IF EXISTS "+qualified(before.Table.Schema, before.Table.Name+"_"+before.Name+"_seq")+";", "contract", "dangerous", true, "sequence_drop", "data_loss"),
		)
	}
	if fromType != toType {
		statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" TYPE "+toType+";", "contract", "review", true, "table_rewrite_possible", "access_exclusive_lock"))
	}
	return statements, nil, nil, nil
}

func serialColumnSupported(column pgschema.Column) bool {
	if column.Serial == nil {
		return true
	}
	if !column.NotNull {
		return false
	}
	if column.Serial.SequenceName == "" {
		return true
	}
	return column.Serial.SequenceName == column.Table.Name+"_"+column.Name+"_seq"
}

func renderConstraintCreate(object pgschema.Constraint) []protocol.Statement {
	sql := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD CONSTRAINT " + quote(object.Name) + " " + object.Definition + ";"
	return []protocol.Statement{statement(sql, "expand", "review", true, "table_lock", "validation_scan_possible")}
}

func constraintUsesMatchPartial(object pgschema.Constraint) bool {
	return object.Type == pgschema.ConstraintForeign && strings.Contains(strings.ToUpper(object.Definition), "MATCH PARTIAL")
}

func renderConstraintCreateUsingExistingIndex(object pgschema.Constraint, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if object.Parent != nil {
		// A child constraint with a typed parent edge is normally created by
		// PostgreSQL together with the partitioned parent's constraint. The one
		// exception is claiming a compatible standalone child index and attaching
		// it to the parent hierarchy; that transition is proven by current state.
		if object.UsingIndex != "" {
			indexID := (pgschema.Index{Table: object.Table, Name: object.UsingIndex}).ObjectID()
			beforeObject, beforeExists := current.Object(indexID)
			afterObject, afterExists := desired.Object(indexID)
			before, beforeOK := beforeObject.(pgschema.Index)
			after, afterOK := afterObject.(pgschema.Index)
			if beforeExists && afterExists && beforeOK && afterOK && partitionConstraintAttachmentValid(object, before, after, desired) {
				return renderPartitionConstraintAttachment(object, current, desired)
			}
		}
		if constraintPropagatedByPartitionParent(object, desired) {
			return nil, nil, nil, nil
		}
		return nil, nil, []string{"partition_constraint_parent:" + object.ObjectID().String()}, nil
	}
	if constraintPropagatedByPartitionParent(object, desired) {
		return nil, nil, nil, nil
	}
	if constraintUsesMatchPartial(object) {
		return nil, nil, []string{"foreign_key_match_partial:" + object.ObjectID().String()}, nil
	}
	if object.UsingIndex == "" {
		return renderConstraintCreate(object), nil, nil, nil
	}
	indexID := (pgschema.Index{Table: object.Table, Name: object.UsingIndex}).ObjectID()
	indexObject, exists := current.Object(indexID)
	if !exists {
		return renderConstraintCreate(object), nil, nil, nil
	}
	index, ok := indexObject.(pgschema.Index)
	if !ok || index.Constraint != "" || (object.Type != pgschema.ConstraintPrimary && object.Type != pgschema.ConstraintUnique) {
		return nil, nil, []string{"constraint_using_index:" + object.ObjectID().String()}, nil
	}
	expectedObject, exists := desired.Object(indexID)
	expected, ok := expectedObject.(pgschema.Index)
	if !exists || !ok || !equivalentIndexForConstraintAttachment(index, expected) {
		return nil, nil, []string{"constraint_using_index_structure:" + object.ObjectID().String()}, nil
	}
	kind := "UNIQUE"
	if object.Type == pgschema.ConstraintPrimary {
		kind = "PRIMARY KEY"
	}
	sql := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD CONSTRAINT " + quote(object.Name) + " " + kind + " USING INDEX " + quote(object.UsingIndex)
	if object.Deferrable {
		sql += " DEFERRABLE"
		if object.Deferred {
			sql += " INITIALLY DEFERRED"
		}
	}
	sql += ";"
	return []protocol.Statement{statement(sql, "expand", "review", true, "table_lock")}, nil, nil, nil
}

func renderPartitionConstraintAttachment(object pgschema.Constraint, current, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if object.UsingIndex == "" || object.Name != object.UsingIndex {
		return nil, nil, []string{"partition_constraint_attach_index_identity:" + object.ObjectID().String()}, nil
	}
	indexID := (pgschema.Index{Table: object.Table, Name: object.UsingIndex}).ObjectID()
	currentObject, currentExists := current.Object(indexID)
	desiredObject, desiredExists := desired.Object(indexID)
	before, beforeOK := currentObject.(pgschema.Index)
	after, afterOK := desiredObject.(pgschema.Index)
	if !currentExists || !desiredExists || !beforeOK || !afterOK || !partitionConstraintAttachmentValid(object, before, after, desired) {
		return nil, nil, []string{"partition_constraint_attach_structure:" + object.ObjectID().String()}, nil
	}
	parentConstraintObject, _ := desired.Object(*object.Parent)
	parentConstraint := parentConstraintObject.(pgschema.Constraint)
	parentIndex, _ := backingIndexForConstraint(desired, parentConstraint)
	addSQL, unsupported := renderConstraintUsingReplacementIndex(object, before.Name)
	if unsupported != "" {
		return nil, nil, []string{unsupported}, nil
	}
	attachSQL := "ALTER INDEX " + qualified(parentIndex.Table.Schema, parentIndex.Name) + " ATTACH PARTITION " + qualified(after.Table.Schema, after.Name) + ";"
	statements := []protocol.Statement{
		withTimeoutGuidance(statement(addSQL, "expand", "review", true, "partition_constraint_attach", "brief_constraint_claim", "access_exclusive_lock"), 30000, 3000),
		withTimeoutGuidance(statement(attachSQL, "expand", "review", true, "partition_constraint_attach", "partition_index_attach", "brief_lock"), 30000, 3000),
	}
	if object.Comment != nil {
		statements = append(statements, withTimeoutGuidance(statement("COMMENT ON CONSTRAINT "+quote(object.Name)+" ON "+qualified(object.Table.Schema, object.Table.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true, "partition_constraint_attach"), 30000, 3000))
	}
	return statements, nil, nil, nil
}

func equivalentIndexForConstraintAttachment(current, desired pgschema.Index) bool {
	current.Constraint, desired.Constraint = "", ""
	current.Primary, desired.Primary = false, false
	current.Definition, desired.Definition = "", ""
	current.Comment, desired.Comment = nil, nil
	return reflect.DeepEqual(current, desired)
}

func renderConstraintComment(object pgschema.Constraint, before, after *string) []protocol.Statement {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	value := "NULL"
	if after != nil {
		value = literal(*after)
	}
	return []protocol.Statement{statement("COMMENT ON CONSTRAINT "+quote(object.Name)+" ON "+qualified(object.Table.Schema, object.Table.Name)+" IS "+value+";", "expand", "safe", true)}
}

func notNullCheckName(id pgschema.ID) string {
	sum := sha256.Sum256([]byte(id.String()))
	return "onwardpg_nn_" + hex.EncodeToString(sum[:4])
}

func destructiveQuestions(changes []change.Change, schemas, tables map[pgschema.ID]bool, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]protocol.Question, map[pgschema.ID]bool, error) {
	var questions []protocol.Question
	approved := make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind != change.Drop || coveredByParent(item.ID, schemas, tables) && !foreignKeyMustDropBeforeReferencedTable(item, tables) || implicitConstraintIndexDrop(item, changes) || propagatedPartitionChildDrop(item, changes) || propagatedPartitionChildRebuild(item, changes) {
			continue
		}
		question := protocol.Question{
			ID: "drop:" + item.ID.String(), Kind: "drop", Key: item.ID.String(),
			Message: "The desired graph omits " + item.ID.String() + ". Confirm destructive removal.", Choices: []string{"drop"},
			CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		}
		_, found, err := resolver.Resolve(question)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			questions = append(questions, question)
			continue
		}
		approved[item.ID] = true
	}
	return questions, approved, nil
}

func foreignKeyMustDropBeforeReferencedTable(item change.Change, droppingTables map[pgschema.ID]bool) bool {
	constraint, ok := item.Before.(pgschema.Constraint)
	return ok && constraint.Type == pgschema.ConstraintForeign && constraint.Reference != nil && droppingTables[constraint.Table] && droppingTables[*constraint.Reference]
}

func implicitConstraintIndexDrop(item change.Change, changes []change.Change) bool {
	if item.Kind != change.Drop || item.ID.Kind != pgschema.KindIndex {
		return false
	}
	index, ok := item.Before.(pgschema.Index)
	if !ok || index.Constraint == "" {
		return false
	}
	for _, candidate := range changes {
		if candidate.Kind != change.Drop || candidate.ID.Kind != pgschema.KindConstraint {
			continue
		}
		constraint, ok := candidate.Before.(pgschema.Constraint)
		if ok && constraint.Table == index.Table && constraint.Name == index.Constraint {
			return true
		}
	}
	return false
}

// propagatedPartitionChildDrop reports a child resource that PostgreSQL drops
// as part of its partitioned parent operation. This keeps destructive approval
// at the owning parent and avoids emitting no-op child statements after the
// parent has already removed them.
func propagatedPartitionChildDrop(item change.Change, changes []change.Change) bool {
	if item.Kind != change.Drop {
		return false
	}
	var parent *pgschema.ID
	switch object := item.Before.(type) {
	case pgschema.Table:
		if object.PartitionOf != nil {
			parent = &object.PartitionOf.Parent
		}
	case pgschema.Constraint:
		parent = object.Parent
	case pgschema.Index:
		parent = object.Parent
	default:
		return false
	}
	if parent == nil {
		return false
	}
	for _, candidate := range changes {
		if candidate.Kind != change.Drop || candidate.ID != *parent {
			continue
		}
		return true
	}
	// A partitioned primary/unique constraint owns its backing parent index;
	// suppress descendants even when the parent-index change is itself implicit.
	if parent.Kind == pgschema.KindIndex {
		for _, candidate := range changes {
			if candidate.Kind == change.Drop && candidate.ID == *parent && implicitConstraintIndexDrop(candidate, changes) {
				return true
			}
		}
	}
	return false
}

// propagatedPartitionChildModify suppresses only a child index/constraint
// whose typed partition parent is being rebuilt. PostgreSQL rebuilds these
// child objects as part of the parent operation; issuing separate child DDL
// would either duplicate the work or fail after the parent has removed it.
func propagatedPartitionChildModify(item change.Change, changes []change.Change) bool {
	if item.Kind != change.Modify {
		return false
	}
	var parent *pgschema.ID
	switch object := item.Before.(type) {
	case pgschema.Index:
		parent = object.Parent
	case pgschema.Constraint:
		parent = object.Parent
	default:
		return false
	}
	if parent == nil {
		return false
	}
	for _, candidate := range changes {
		if candidate.Kind == change.Modify && candidate.ID == *parent {
			return true
		}
	}
	return false
}

// propagatedPartitionChildRebuild covers a parent index/constraint rebuild
// whose generated child objects change identity (and therefore diff as a
// drop/create pair rather than a Modify). The parent operation is the only
// executable owner of that hierarchy-wide replacement.
func propagatedPartitionChildRebuild(item change.Change, changes []change.Change) bool {
	var parent *pgschema.ID
	switch item.Kind {
	case change.Drop, change.Modify:
		switch object := item.Before.(type) {
		case pgschema.Index:
			parent = object.Parent
		case pgschema.Constraint:
			parent = object.Parent
		}
	case change.Create:
		switch object := item.After.(type) {
		case pgschema.Index:
			parent = object.Parent
		case pgschema.Constraint:
			parent = object.Parent
		}
	}
	if parent == nil {
		return false
	}
	for _, candidate := range changes {
		if candidate.Kind == change.Modify && candidate.ID == *parent {
			return true
		}
	}
	return false
}

func droppingParents(changes []change.Change) (map[pgschema.ID]bool, map[pgschema.ID]bool) {
	schemas, tables := make(map[pgschema.ID]bool), make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind != change.Drop {
			continue
		}
		switch item.ID.Kind {
		case pgschema.KindSchema:
			schemas[item.ID] = true
		case pgschema.KindTable, pgschema.KindMatView:
			tables[item.ID] = true
		}
	}
	return schemas, tables
}

func coveredByParent(id pgschema.ID, schemas, tables map[pgschema.ID]bool) bool {
	if id.Kind == pgschema.KindSchema {
		return false
	}
	if schemas[(pgschema.Schema{Name: id.Schema}).ObjectID()] {
		return true
	}
	if id.Kind == pgschema.KindTable || id.Kind == pgschema.KindMatView {
		return false
	}
	if tables[(pgschema.Table{Schema: id.Schema, Name: id.Name}).ObjectID()] {
		return true
	}
	return tables[(pgschema.View{Schema: id.Schema, Name: id.Name, Materialized: true}).ObjectID()]
}

func tableColumns(snapshot *pgschema.Snapshot, table pgschema.ID) []pgschema.Column {
	var columns []pgschema.Column
	for _, object := range snapshot.Objects() {
		if column, ok := object.(pgschema.Column); ok && column.Table == table {
			columns = append(columns, column)
		}
	}
	sort.Slice(columns, func(i, j int) bool {
		if columns[i].Position == columns[j].Position {
			return columns[i].Name < columns[j].Name
		}
		return columns[i].Position < columns[j].Position
	})
	return columns
}

func indexesForRelation(snapshot *pgschema.Snapshot, relation pgschema.ID) []pgschema.Index {
	if snapshot == nil {
		return nil
	}
	var indexes []pgschema.Index
	for _, object := range snapshot.Objects() {
		if index, ok := object.(pgschema.Index); ok && index.Table == relation {
			indexes = append(indexes, index)
		}
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].Name < indexes[j].Name })
	return indexes
}

func renderColumn(column pgschema.Column) string {
	typ := column.Type
	if column.Serial != nil {
		typ = column.Serial.Type
	}
	parts := []string{quote(column.Name), typ}
	if column.Collation != "" {
		parts = append(parts, "COLLATE "+column.Collation)
	}
	if column.Identity != nil {
		identity := column.Identity
		options := []string{"START WITH " + fmt.Sprint(identity.Start), "INCREMENT BY " + fmt.Sprint(identity.Increment)}
		if identity.Min != nil {
			options = append(options, "MINVALUE "+fmt.Sprint(*identity.Min))
		}
		if identity.Max != nil {
			options = append(options, "MAXVALUE "+fmt.Sprint(*identity.Max))
		}
		if identity.Cache != 0 {
			options = append(options, "CACHE "+fmt.Sprint(identity.Cache))
		}
		if identity.Cycle {
			options = append(options, "CYCLE")
		}
		parts = append(parts, "GENERATED "+identity.Generation+" AS IDENTITY ("+strings.Join(options, " ")+")")
	}
	if column.Generated != nil {
		kind := column.Generated.Kind
		if kind == "" {
			kind = "STORED"
		}
		parts = append(parts, "GENERATED ALWAYS AS ("+column.Generated.Expression+") "+kind)
	}
	if column.Default != nil && column.Serial == nil {
		parts = append(parts, "DEFAULT "+*column.Default)
	}
	if column.NotNull {
		parts = append(parts, "NOT NULL")
	}
	return strings.Join(parts, " ")
}

func renderIdentityOptions(identity pgschema.Identity, alter bool) (string, string) {
	if identity.Generation != "ALWAYS" && identity.Generation != "BY DEFAULT" {
		return "", "identity_generation_invalid"
	}
	if identity.Increment == 0 || identity.Cache < 1 {
		return "", "identity_sequence_option_invalid"
	}
	prefix := ""
	if alter {
		prefix = "SET "
	}
	options := []string{
		prefix + "START WITH " + fmt.Sprint(identity.Start),
		prefix + "INCREMENT BY " + fmt.Sprint(identity.Increment),
	}
	if identity.Min == nil {
		options = append(options, prefix+"NO MINVALUE")
	} else {
		options = append(options, prefix+"MINVALUE "+fmt.Sprint(*identity.Min))
	}
	if identity.Max == nil {
		options = append(options, prefix+"NO MAXVALUE")
	} else {
		options = append(options, prefix+"MAXVALUE "+fmt.Sprint(*identity.Max))
	}
	options = append(options, prefix+"CACHE "+fmt.Sprint(identity.Cache))
	if identity.Cycle {
		options = append(options, prefix+"CYCLE")
	} else {
		options = append(options, prefix+"NO CYCLE")
	}
	return strings.Join(options, " "), ""
}

func commentModification(kind, identifier string, before, after *string) []protocol.Statement {
	return commentModificationPhase(kind, identifier, before, after, "expand")
}

func commentModificationPhase(kind, identifier string, before, after *string, phase string) []protocol.Statement {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	value := "NULL"
	if after != nil {
		value = literal(*after)
	}
	return []protocol.Statement{statement("COMMENT ON "+kind+" "+identifier+" IS "+value+";", phase, "safe", true)}
}

func statement(sql, phase, safety string, transactional bool, hazards ...string) protocol.Statement {
	return protocol.Statement{SQL: sql, Phase: phase, Safety: safety, Hazards: hazards, NonTransactional: !transactional}
}

func authorizationStatement(sql, phase, safety string, hazards ...string) protocol.Statement {
	return withTimeoutGuidance(statement(sql, phase, safety, true, hazards...), 30000, 3000)
}

func withTimeoutGuidance(statement protocol.Statement, statementTimeoutMS, lockTimeoutMS int64) protocol.Statement {
	statement.StatementTimeoutMS = statementTimeoutMS
	statement.LockTimeoutMS = lockTimeoutMS
	return statement
}

func manualWorkStatement(work protocol.ManualWork, hazards ...string) protocol.Statement {
	lines := []string{"-- PRODUCT-SPECIFIC SQL: " + work.Summary}
	for _, verification := range work.VerificationSQL {
		lines = append(lines, "-- Verify: "+verification)
	}
	lines = append(lines, work.Statements...)
	return protocol.Statement{
		SQL: strings.Join(lines, "\n"), Phase: protocol.PhaseContract, Safety: "manual",
		Hazards: hazards, Manual: &work, NonTransactional: work.ExecutionMode == "non_transactional",
	}
}

func appendStatement(result *protocol.Result, item protocol.Statement) {
	result.Statements = append(result.Statements, item)
}

func rebuildBatches(result *protocol.Result) error {
	orderedPhases := []string{protocol.PhaseExpand, protocol.PhaseContract}
	byPhase := make(map[string][]protocol.Statement, len(orderedPhases))
	for _, item := range result.Statements {
		if !protocol.ValidPhase(item.Phase) {
			return fmt.Errorf("unknown statement phase %q", item.Phase)
		}
		byPhase[item.Phase] = append(byPhase[item.Phase], item)
	}
	result.Statements, result.Batches = nil, nil
	for _, phase := range orderedPhases {
		phaseBatch := 0
		for _, item := range byPhase[phase] {
			result.Statements = append(result.Statements, item)
			transactional := !item.NonTransactional
			if len(result.Batches) > 0 {
				last := &result.Batches[len(result.Batches)-1]
				lastIsManual := len(last.Statements) > 0 && last.Statements[len(last.Statements)-1].Manual != nil
				if last.Phase == phase && last.Transactional == transactional && !lastIsManual && item.Manual == nil && !item.BatchBoundaryBefore {
					last.Statements = append(last.Statements, item)
					continue
				}
			}
			phaseBatch++
			result.Batches = append(result.Batches, protocol.Batch{
				ID: fmt.Sprintf("batch-%s-%03d", phase, phaseBatch), Phase: phase,
				Transactional: transactional, Statements: []protocol.Statement{item},
			})
		}
	}
	return nil
}

func rebuildUnsortedBatches(result *protocol.Result) error {
	statements := append([]protocol.Statement(nil), result.Statements...)
	result.Statements, result.Batches = nil, nil
	phaseBatches := make(map[string]int)
	for _, item := range statements {
		result.Statements = append(result.Statements, item)
		transactional := !item.NonTransactional
		if len(result.Batches) > 0 {
			last := &result.Batches[len(result.Batches)-1]
			if last.Transactional == transactional && !item.BatchBoundaryBefore {
				last.Statements = append(last.Statements, item)
				continue
			}
		}
		phaseBatches[item.Phase]++
		result.Batches = append(result.Batches, protocol.Batch{
			ID: fmt.Sprintf("batch-%s-%03d", item.Phase, phaseBatches[item.Phase]), Phase: item.Phase,
			Transactional: transactional, Statements: []protocol.Statement{item},
		})
	}
	return nil
}

func withPhase(statements []protocol.Statement, phase, safety string, hazards ...string) []protocol.Statement {
	result := make([]protocol.Statement, len(statements))
	for i, statement := range statements {
		statement.Phase, statement.Safety = phase, safety
		statement.Hazards = unionStrings(statement.Hazards, hazards)
		result[i] = statement
	}
	return result
}

func quote(identifier string) string          { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
func qualified(namespace, name string) string { return quote(namespace) + "." + quote(name) }
func literal(value string) string             { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func unionStrings(left, right []string) []string {
	set := make(map[string]bool, len(left)+len(right))
	for _, item := range append(left, right...) {
		set[item] = true
	}
	items := make([]string, 0, len(set))
	for item := range set {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}
