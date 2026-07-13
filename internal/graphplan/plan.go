// Package graphplan renders forward-only PostgreSQL plans from typed graphs.
package graphplan

import (
	"crypto/sha256"
	"encoding/hex"
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
}

type decisions struct {
	typeUsing       map[pgschema.ID]string
	notNullStrategy map[pgschema.ID]string
	matViewRebuild  map[pgschema.ID]bool
	matViewRefresh  map[pgschema.ID]protocol.ManualWork
	partitionManual map[pgschema.ID]protocol.ManualWork
}

type renameIndex struct {
	from pgschema.Index
	to   pgschema.Index
}

type renameTable struct {
	from pgschema.Table
	to   pgschema.Table
}

type renameColumn struct {
	from pgschema.Column
	to   pgschema.Column
}

type renameEnum struct {
	from pgschema.Enum
	to   pgschema.Enum
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
	currentFingerprint, err := current.Fingerprint()
	if err != nil {
		return protocol.Result{}, err
	}
	desiredFingerprint, err := desired.Fingerprint()
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
	if rejected := rejectedConstraintRenames(changes); len(rejected) > 0 {
		return unsupportedResult(result, resolver, rejected)
	}
	changes, tableRenames, tableRenameQuestions, tableRenameUnsupported, err := resolveTableRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(tableRenameUnsupported) > 0 {
		return unsupportedResult(result, resolver, tableRenameUnsupported)
	}
	if len(tableRenameQuestions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, tableRenameQuestions
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
		return result, nil
	}
	changes, extensionMoves := resolveExtensionMoves(changes)
	changes, columnRenames, columnRenameQuestions, columnRenameUnsupported, err := resolveColumnRenames(changes, current, desired, resolver, currentFingerprint, desiredFingerprint)
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
		return result, nil
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
	if len(questions) > 0 {
		if err := resolver.ValidateAllUsed(); err != nil {
			return protocol.Result{}, err
		}
		result.Status, result.Questions = protocol.NeedsInput, questions
		return result, nil
	}
	// These statements must precede same-phase work that refers to the desired
	// routine identity. Trigger definitions are reapplied after a confirmed
	// routine rename, never before the new routine name exists.
	for _, rename := range routineRenames {
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
		appendStatement(&result, renderTriggerRename(rename))
	}

	createdTables := make(map[pgschema.ID]bool)
	for _, item := range changes {
		if item.Kind == change.Create && item.ID.Kind == pgschema.KindTable {
			createdTables[item.ID] = true
		}
	}
	detachedIndexes := detachedConstraintIndexes(changes)
	deferredIndexCreates := make([]change.Change, 0)
	consumed := make(map[pgschema.ID]bool)
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
			if implicitConstraintIndexDrop(item, changes) || propagatedPartitionChildDrop(item, changes) || propagatedPartitionChildModify(item, changes) || propagatedPartitionChildRebuild(item, changes) {
				continue
			}
			if item.Kind == change.Drop {
				if coveredByParent(item.ID, droppingSchemas, droppingTables) {
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
				appendStatement(&result, statement)
			}
		}
	}
	if len(dynamicUnsupported) > 0 {
		return unsupportedResult(result, resolver, dynamicUnsupported)
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
		for _, statement := range renderTableRename(rename) {
			appendStatement(&result, statement)
		}
	}
	for _, rename := range viewRenames {
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
		appendStatement(&result, renderEnumRename(rename))
	}
	for _, move := range extensionMoves {
		appendStatement(&result, renderExtensionMove(move))
	}
	for _, rename := range columnRenames {
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
	result.Status = protocol.Planned
	return result, nil
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

func rejectedConstraintRenames(changes []change.Change) []string {
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
	var rejected []string
	for _, drop := range drops {
		before := drop.Before.(pgschema.Constraint)
		candidates := 0
		for _, create := range creates {
			after := create.After.(pgschema.Constraint)
			if equivalentConstraintForRejectedRename(before, after) {
				candidates++
			}
		}
		if candidates == 1 {
			rejected = append(rejected, "constraint_rename:"+drop.ID.String())
		}
	}
	return unionStrings(rejected, nil)
}

func equivalentConstraintForRejectedRename(before, after pgschema.Constraint) bool {
	before.Name, after.Name = "", ""
	return reflect.DeepEqual(before, after)
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

func renderEnumRename(rename renameEnum) protocol.Statement {
	sql := "ALTER TYPE " + qualified(rename.from.Schema, rename.from.Name) + " RENAME TO " + quote(rename.to.Name) + ";"
	return statement(sql, "contract", "review", true, "application_contract")
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

func renderExtensionMove(move moveExtension) protocol.Statement {
	sql := "ALTER EXTENSION " + quote(move.from.Name) + " SET SCHEMA " + quote(move.to.Schema) + ";"
	return statement(sql, "migrate", "review", true, "extension_schema_move")
}

func resolveColumnRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameColumn, []protocol.Question, []string, error) {
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
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		to := candidate.After.(pgschema.Column)
		question := protocol.Question{
			ID: "rename_column:" + drop.ID.String(), Kind: "rename_column", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
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
		if answer == candidate.ID.String() {
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
	from.Comment, to.Comment = nil, nil
	return reflect.DeepEqual(from, to)
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
	sql := "ALTER TABLE " + table + " RENAME COLUMN " + quote(rename.from.Name) + " TO " + quote(rename.to.Name) + ";"
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "application_contract")}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON COLUMN "+table+"."+quote(rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func resolveTableRenames(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]change.Change, []renameTable, []protocol.Question, []string, error) {
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
	var questions []protocol.Question
	for _, drop := range drops {
		from := drop.Before.(pgschema.Table)
		var candidates []change.Change
		for _, create := range creates {
			to := create.After.(pgschema.Table)
			if equivalentTableForRename(current, desired, from, to) {
				candidates = append(candidates, create)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		candidate := candidates[0]
		to := candidate.After.(pgschema.Table)
		question := protocol.Question{
			ID: "rename_table:" + drop.ID.String(), Kind: "rename_table", Key: drop.ID.String(),
			Message:            "Was " + drop.ID.String() + " renamed to " + candidate.ID.String() + "?",
			Choices:            []string{candidate.ID.String(), "create"},
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
		if answer == candidate.ID.String() {
			consumed[drop.ID], consumed[candidate.ID] = true, true
			for _, object := range current.Objects() {
				if childTable(object) == from.ObjectID() {
					consumed[object.ObjectID()] = true
				}
			}
			for _, object := range desired.Objects() {
				if childTable(object) == to.ObjectID() {
					consumed[object.ObjectID()] = true
				}
			}
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
					return nil, nil, nil, []string{"table_rename_dependent_view:" + view.ObjectID().String()}, nil
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
			renames = append(renames, renameTable{from: from, to: to})
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
		statements = append(statements, statement("ALTER "+kind+" "+from+" SET SCHEMA "+quote(rename.to.Schema)+";", "migrate", "review", true, "application_contract"))
		current = qualified(rename.to.Schema, rename.from.Name) + "(" + rename.from.Signature + ")"
	}
	if rename.from.Name != rename.to.Name {
		statements = append(statements, statement("ALTER "+kind+" "+current+" RENAME TO "+quote(rename.to.Name)+";", "migrate", "review", true, "application_contract"))
	}
	// pg_get_functiondef remains the desired authoritative definition after an
	// ALTER rename, including a body that may intentionally mention the new
	// name. Reapply it rather than attempting a source rewrite.
	if definition, unsupported := renderRoutineDefinition(rename.to); unsupported == "" {
		statements = append(statements, statement(definition, "migrate", "review", true, "routine_replace", "application_contract"))
	}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON "+strings.ToUpper(rename.to.Kind)+" "+qualified(rename.to.Schema, rename.to.Name)+"("+rename.to.Signature+") IS "+value+";", "migrate", "safe", true))
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
	statements := []protocol.Statement{statement(sql, "migrate", "review", true, "dependent_trigger_rewrite", "application_contract", "table_lock")}
	if rewrite.after.Enabled != rewrite.before.Enabled {
		enabled, unsupported := renderTriggerEnabled(rewrite.after)
		if unsupported != "" {
			return nil
		}
		statements = append(statements, statement(enabled, "migrate", "review", true, "trigger_enable_state", "application_contract", "table_lock"))
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
	const prefix = "CREATE TRIGGER "
	if !strings.HasPrefix(strings.ToUpper(value), prefix) {
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

func renderTriggerRename(rename renameTrigger) protocol.Statement {
	sql := "ALTER TRIGGER " + quote(rename.from.Name) + " ON " + qualified(rename.from.Table.Schema, rename.from.Table.Name) + " RENAME TO " + quote(rename.to.Name) + ";"
	return statement(sql, "migrate", "review", true, "application_contract")
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
	if len(fromChildren) != len(toChildren) {
		return false
	}
	for key, before := range fromChildren {
		after, exists := toChildren[key]
		if !exists || !equivalentChildForTableRename(before, after, from, to) {
			return false
		}
	}
	return true
}

func tableChildren(snapshot *pgschema.Snapshot, table pgschema.ID) map[string]pgschema.Object {
	children := make(map[string]pgschema.Object)
	for _, object := range snapshot.Objects() {
		if childTable(object) == table {
			id := object.ObjectID()
			children[string(id.Kind)+":"+id.Part+":"+id.Signature] = object
		}
	}
	return children
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
		return reflect.DeepEqual(before, next)
	case pgschema.Constraint:
		next, ok := after.(pgschema.Constraint)
		if !ok {
			return false
		}
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

func renderTableRename(rename renameTable) []protocol.Statement {
	from := qualified(rename.from.Schema, rename.from.Name)
	current := from
	var statements []protocol.Statement
	if rename.from.Schema != rename.to.Schema {
		statements = append(statements, statement("ALTER TABLE "+from+" SET SCHEMA "+quote(rename.to.Schema)+";", "contract", "review", true, "application_contract"))
		current = qualified(rename.to.Schema, rename.from.Name)
	}
	if rename.from.Name != rename.to.Name {
		statements = append(statements, statement("ALTER TABLE "+current+" RENAME TO "+quote(rename.to.Name)+";", "contract", "review", true, "application_contract"))
	}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON TABLE "+qualified(rename.to.Schema, rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
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
	statements := []protocol.Statement{statement(sql, "contract", "review", true, "application_contract")}
	if !reflect.DeepEqual(rename.from.Comment, rename.to.Comment) {
		value := "NULL"
		if rename.to.Comment != nil {
			value = literal(*rename.to.Comment)
		}
		statements = append(statements, statement("COMMENT ON INDEX "+qualified(rename.to.Table.Schema, rename.to.Name)+" IS "+value+";", "contract", "safe", true))
	}
	return statements
}

func renderChange(item change.Change, current, desired *pgschema.Snapshot, createdTables map[pgschema.ID]bool, options Options, choices decisions) ([]protocol.Statement, []pgschema.ID, []string, error) {
	switch item.Kind {
	case change.Create:
		if constraint, ok := item.After.(pgschema.Constraint); ok {
			return renderConstraintCreateUsingExistingIndex(constraint, current, desired)
		}
		return renderCreate(item.After, desired, createdTables, options)
	case change.Drop:
		return renderDropWithOptions(item.Before, options), nil, nil, nil
	case change.Modify:
		return renderModify(item.Before, item.After, choices, options, desired)
	default:
		return nil, nil, nil, fmt.Errorf("unknown change kind %q", item.Kind)
	}
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
		return []protocol.Statement{statement("CREATE TYPE "+qualified(object.Schema, object.Name)+" AS ENUM ("+strings.Join(labels, ", ")+");", "expand", "safe", true)}, nil, nil, nil
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
		statements := []protocol.Statement{statement("CREATE SEQUENCE "+qualified(object.Schema, object.Name)+" "+strings.Join(parts, " ")+";", "expand", "safe", true)}
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
		} else if object.Partition != nil {
			if object.Partition.Raw == "" {
				return nil, nil, nil, fmt.Errorf("partitioned table %s has no canonical partition key", object.ObjectID())
			}
			sql += " PARTITION BY " + object.Partition.Raw
		}
		statements := []protocol.Statement{statement(sql+";", "expand", "review", true)}
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
		sql := "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD COLUMN " + renderColumn(object) + ";"
		statements := []protocol.Statement{statement(sql, "expand", "review", true, "table_lock", "table_rewrite_possible")}
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
		hazards := []string{"table_lock", "validation_scan_possible"}
		return []protocol.Statement{statement(sql, "expand", "review", true, hazards...)}, nil, nil, nil
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
		statements := []protocol.Statement{statement(strings.TrimSuffix(sql, ";")+";", "expand", "review", transactional, "index_build", "table_lock_possible")}
		if object.Comment != nil {
			statements = append(statements, statement("COMMENT ON INDEX "+qualified(object.Table.Schema, object.Name)+" IS "+literal(*object.Comment)+";", "expand", "safe", true))
		}
		return statements, nil, nil, nil
	case pgschema.Extension:
		if object.Name == "" || object.Schema == "" || object.Version == "" {
			return nil, nil, []string{"extension_create:" + object.ObjectID().String()}, nil
		}
		sql := "CREATE EXTENSION " + quote(object.Name) + " WITH SCHEMA " + quote(object.Schema) + " VERSION " + literal(object.Version) + ";"
		return []protocol.Statement{statement(sql, "expand", "review", true, "extension_install")}, nil, nil, nil
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
		return statements, nil, nil, nil
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
	if desired == nil || object.Parent != nil || (object.Type != pgschema.ConstraintPrimary && object.Type != pgschema.ConstraintUnique) {
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
// pg_get_indexdef compatibility field. This keeps adapter snapshots portable
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
	sql := prefix + " AS " + strings.TrimSpace(view.Definition)
	if view.Materialized && !view.Populated {
		sql += " WITH NO DATA"
	}
	return strings.TrimSuffix(sql, ";") + ";", ""
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

func renderTriggerDefinition(trigger pgschema.Trigger) (string, string) {
	if trigger.Table.Kind != pgschema.KindTable || trigger.Table.Schema == "" || trigger.Table.Name == "" || trigger.Name == "" {
		return "", "trigger_render:" + trigger.ObjectID().String()
	}
	definition := strings.TrimSpace(trigger.Definition)
	if !strings.HasPrefix(strings.ToUpper(definition), "CREATE TRIGGER ") {
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
	default:
		return nil
	}
	return []protocol.Statement{statement(sql, "contract", "dangerous", transactional, hazards...)}
}

func renderModify(before, after pgschema.Object, choices decisions, options Options, desired *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	switch before := before.(type) {
	case pgschema.Schema:
		next := after.(pgschema.Schema)
		return commentModification("SCHEMA", quote(next.Name), before.Comment, next.Comment), nil, nil, nil
	case pgschema.Table:
		next := after.(pgschema.Table)
		if !reflect.DeepEqual(before.Partition, next.Partition) {
			return nil, nil, []string{"partition_rewrite:" + next.ObjectID().String()}, nil
		}
		var statements []protocol.Statement
		if !reflect.DeepEqual(before.PartitionOf, next.PartitionOf) {
			switch {
			case before.PartitionOf == nil && next.PartitionOf != nil:
				if next.PartitionOf.Parent.Kind != pgschema.KindTable || next.PartitionOf.Parent.Schema == "" || next.PartitionOf.Parent.Name == "" || strings.TrimSpace(next.PartitionOf.Bound) == "" {
					return nil, nil, []string{"partition_attach_invalid:" + next.ObjectID().String()}, nil
				}
				sql := "ALTER TABLE " + qualified(next.PartitionOf.Parent.Schema, next.PartitionOf.Parent.Name) + " ATTACH PARTITION " + qualified(next.Schema, next.Name) + " " + next.PartitionOf.Bound + ";"
				statements = append(statements, statement(sql, "migrate", "review", true, "partition_attach", "table_scan_possible", "access_exclusive_lock"))
			case before.PartitionOf != nil && next.PartitionOf == nil:
				if before.PartitionOf.Parent.Kind != pgschema.KindTable || before.PartitionOf.Parent.Schema == "" || before.PartitionOf.Parent.Name == "" {
					return nil, nil, []string{"partition_detach_invalid:" + next.ObjectID().String()}, nil
				}
				sql := "ALTER TABLE " + qualified(before.PartitionOf.Parent.Schema, before.PartitionOf.Parent.Name) + " DETACH PARTITION " + qualified(before.Schema, before.Name) + ";"
				statements = append(statements, statement(sql, "migrate", "review", true, "partition_detach", "access_exclusive_lock", "application_contract"))
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
			statements = append(statements, statement("ALTER TABLE "+qualified(next.Schema, next.Name)+" "+mode+";", "migrate", "review", true, "table_rewrite_possible", "access_exclusive_lock"))
		}
		statements = append(statements, commentModification("TABLE", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Column:
		next := after.(pgschema.Column)
		return renderColumnModify(before, next, choices)
	case pgschema.Index:
		next := after.(pgschema.Index)
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
			statements := withPhase(renderDropWithOptions(before, options), "migrate", "review", "index_rebuild")
			created, _, unsupported, err := renderCreate(next, pgschema.New(), nil, options)
			if err != nil {
				return nil, nil, nil, err
			}
			return append(statements, withPhase(created, "migrate", "review", "index_rebuild")...), nil, unsupported, nil
		}
		return commentModification("INDEX", qualified(next.Table.Schema, next.Name), before.Comment, next.Comment), nil, nil, nil
	case pgschema.Enum:
		next := after.(pgschema.Enum)
		statements, err := addEnumValues(before, next)
		if err != nil {
			return nil, nil, []string{"enum_rewrite:" + next.ObjectID().String()}, nil
		}
		return statements, nil, nil, nil
	case pgschema.Extension:
		next := after.(pgschema.Extension)
		if before.Name == next.Name && before.Schema == next.Schema && before.Version != next.Version && next.Version != "" {
			sql := "ALTER EXTENSION " + quote(next.Name) + " UPDATE TO " + literal(next.Version) + ";"
			return []protocol.Statement{statement(sql, "migrate", "review", true, "extension_update")}, nil, nil, nil
		}
		return nil, nil, []string{"extension_change:" + after.ObjectID().String()}, nil
	case pgschema.Sequence:
		next := after.(pgschema.Sequence)
		if before.Schema != next.Schema || before.Name != next.Name || next.Type == "" {
			return nil, nil, []string{"sequence_modify:" + after.ObjectID().String()}, nil
		}
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
		sql := "ALTER SEQUENCE " + qualified(next.Schema, next.Name) + " " + strings.Join(parts, " ") + ";"
		return []protocol.Statement{statement(sql, "migrate", "review", true, "sequence_parameter_change")}, nil, nil, nil
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
			drop := withPhase(renderDrop(before), "migrate", "dangerous", "materialized_view_rebuild", "data_loss", "blocking_lock")
			created, _, unsupported, err := renderCreate(next, pgschema.New(), nil, options)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(unsupported) > 0 {
				return nil, nil, unsupported, nil
			}
			statements := append(drop, withPhase(created, "migrate", "review", "materialized_view_rebuild", "data_loss", "blocking_lock")...)
			if desired != nil {
				for _, index := range indexesForRelation(desired, next.ObjectID()) {
					indexes, _, indexUnsupported, err := renderCreate(index, desired, nil, options)
					if err != nil {
						return nil, nil, nil, err
					}
					if len(indexUnsupported) > 0 {
						return nil, nil, indexUnsupported, nil
					}
					statements = append(statements, withPhase(indexes, "migrate", "review", "materialized_view_index_rebuild", "index_build", "table_lock_possible")...)
				}
			}
			return statements, nil, nil, nil
		}
		sql, unsupported := renderViewCreate(next, true)
		if unsupported != "" {
			return nil, nil, []string{unsupported}, nil
		}
		statements := []protocol.Statement{statement(sql, "migrate", "review", true, "view_replace")}
		statements = append(statements, commentModification("VIEW", qualified(next.Schema, next.Name), before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Routine:
		next := after.(pgschema.Routine)
		if before.Kind != next.Kind || before.Signature != next.Signature {
			return nil, nil, []string{"routine_identity_change:" + next.ObjectID().String()}, nil
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
		statements := []protocol.Statement{statement(sql, "migrate", "review", true, "routine_replace", "application_contract")}
		statements = append(statements, commentModification(strings.ToUpper(next.Kind), qualified(next.Schema, next.Name)+"("+next.Signature+")", before.Comment, next.Comment)...)
		return statements, nil, nil, nil
	case pgschema.Trigger:
		next := after.(pgschema.Trigger)
		beforeDefinition, nextDefinition := strings.TrimSpace(before.Definition), strings.TrimSpace(next.Definition)
		var statements []protocol.Statement
		if before.Table != next.Table || before.Routine != next.Routine || beforeDefinition != nextDefinition {
			created, unsupported := renderTriggerDefinition(next)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements,
				statement("DROP TRIGGER "+quote(before.Name)+" ON "+qualified(before.Table.Schema, before.Table.Name)+";", "migrate", "review", true, "trigger_recreate", "table_lock", "application_contract"),
				statement(created, "migrate", "review", true, "trigger_recreate", "table_lock", "application_contract"),
			)
		}
		if before.Enabled != next.Enabled {
			enabled, unsupported := renderTriggerEnabled(next)
			if unsupported != "" {
				return nil, nil, []string{unsupported}, nil
			}
			statements = append(statements, statement(enabled, "migrate", "review", true, "trigger_enable_state", "table_lock", "application_contract"))
		}
		return statements, nil, nil, nil
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
			if !next.Validated {
				return nil, nil, []string{"constraint_unvalidate:" + next.ObjectID().String()}, nil
			}
			sql := "ALTER TABLE " + qualified(next.Table.Schema, next.Table.Name) + " VALIDATE CONSTRAINT " + quote(next.Name) + ";"
			return []protocol.Statement{statement(sql, "migrate", "review", true, "table_scan", "share_update_exclusive_lock")}, nil, nil, nil
		}
		if !reflect.DeepEqual(beforeNoComment, nextNoComment) {
			drop := withPhase(renderDrop(before), "migrate", "review", "constraint_rebuild", "blocking_lock")
			add := withPhase(renderConstraintCreate(next), "migrate", "review", "constraint_rebuild")
			return append(drop, add...), nil, nil, nil
		}
		return renderConstraintComment(next, before.Comment, next.Comment), nil, nil, nil
	default:
		return nil, nil, []string{"modify:" + after.ObjectID().String()}, nil
	}
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

func constraintsEqualIgnoringValidation(before, after pgschema.Constraint) bool {
	before.Validated, after.Validated = false, false
	before.Definition = strings.ReplaceAll(before.Definition, " NOT VALID", "")
	after.Definition = strings.ReplaceAll(after.Definition, " NOT VALID", "")
	return reflect.DeepEqual(before, after)
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

func mutationQuestions(changes []change.Change, current, desired *pgschema.Snapshot, resolver *protocol.Resolver, currentFingerprint, desiredFingerprint string) ([]protocol.Question, decisions, error) {
	decisions := decisions{typeUsing: make(map[pgschema.ID]string), notNullStrategy: make(map[pgschema.ID]string), matViewRebuild: make(map[pgschema.ID]bool), matViewRefresh: make(map[pgschema.ID]protocol.ManualWork), partitionManual: make(map[pgschema.ID]protocol.ManualWork)}
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
			if partitionReconfiguration(before.PartitionOf, after.PartitionOf) {
				question := protocol.Question{
					ID: "partition_reconfiguration:" + item.ID.String(), Kind: "partition_reconfiguration", Key: item.ID.String(),
					Message:            "Partition " + item.ID.String() + " changes parent or bound. Supply an explicit, reviewed manual work contract (including data movement and verification SQL); onwardpg will not infer it.",
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
		if before.Serial == nil && after.Serial == nil && before.Type != after.Type {
			question := protocol.Question{
				ID: "type_change:" + item.ID.String(), Kind: "type_change", Key: item.ID.String(),
				Message: "Column " + item.ID.String() + " changes from " + before.Type + " to " + after.Type + ". Supply a PostgreSQL USING expression or choose direct.",
				Choices: []string{"direct"}, AllowsFreeform: true,
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
				Message:            "Column " + item.ID.String() + " becomes NOT NULL. Choose a direct lock/scan or a staged NOT VALID check, validation, and contract.",
				Choices:            []string{"direct", "staged"},
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

func renderColumnModify(before, after pgschema.Column, choices decisions) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if before.Collation != after.Collation {
		return nil, nil, []string{"column_collation_modify:" + after.ObjectID().String()}, nil
	}
	var statements []protocol.Statement
	table := qualified(after.Table.Schema, after.Table.Name)
	column := quote(after.Name)
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
		case !reflect.DeepEqual(before.Generated, after.Generated):
			return nil, nil, []string{"generated_column_rewrite:" + after.ObjectID().String()}, nil
		}
	}
	if !reflect.DeepEqual(before.Identity, after.Identity) {
		if before.Identity == nil || after.Identity == nil {
			return nil, nil, []string{"identity_add_drop:" + after.ObjectID().String()}, nil
		}
		if before.Identity.Generation != after.Identity.Generation {
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET GENERATED "+after.Identity.Generation+";", "migrate", "review", true, "table_lock"))
		}
		if before.Identity.Start != after.Identity.Start || before.Identity.Increment != after.Identity.Increment {
			sql := "ALTER TABLE " + table + " ALTER COLUMN " + column + " SET START WITH " + fmt.Sprint(after.Identity.Start) + " SET INCREMENT BY " + fmt.Sprint(after.Identity.Increment) + ";"
			statements = append(statements, statement(sql, "migrate", "review", true, "sequence_state"))
		}
		if !reflect.DeepEqual(before.Identity.Min, after.Identity.Min) || !reflect.DeepEqual(before.Identity.Max, after.Identity.Max) || before.Identity.Cache != after.Identity.Cache || before.Identity.Cycle != after.Identity.Cycle {
			return nil, nil, []string{"identity_option_modify:" + after.ObjectID().String()}, nil
		}
	}
	if before.Serial == nil && after.Serial == nil && before.Type != after.Type {
		using, exists := choices.typeUsing[after.ObjectID()]
		if !exists {
			return nil, nil, nil, fmt.Errorf("missing type-change decision for %s", after.ObjectID())
		}
		sql := "ALTER TABLE " + table + " ALTER COLUMN " + column + " TYPE " + after.Type
		if using != "direct" {
			sql += " USING " + using
		}
		statements = append(statements, statement(sql+";", "migrate", "review", true, "table_rewrite_possible", "access_exclusive_lock"))
	}
	if reflect.DeepEqual(before.Serial, after.Serial) && !reflect.DeepEqual(before.Default, after.Default) {
		phase := "expand"
		// PostgreSQL validates/coerces a default against the column's current
		// type. Keep it in the same ordered migration phase after TYPE so a
		// combined type/default change is executable in one plan.
		if before.Type != after.Type {
			phase = "migrate"
		}
		if after.Default == nil {
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT;", phase, "safe", true))
		} else {
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET DEFAULT "+*after.Default+";", phase, "safe", true))
		}
	}
	if before.NotNull != after.NotNull {
		if !after.NotNull {
			statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP NOT NULL;", "expand", "safe", true))
		} else {
			strategy, exists := choices.notNullStrategy[after.ObjectID()]
			if !exists {
				return nil, nil, nil, fmt.Errorf("missing NOT NULL strategy for %s", after.ObjectID())
			}
			switch strategy {
			case "direct":
				statements = append(statements, statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "table_scan", "access_exclusive_lock"))
			case "staged":
				check := notNullCheckName(after.ObjectID())
				statements = append(statements,
					statement("ALTER TABLE "+table+" ADD CONSTRAINT "+quote(check)+" CHECK ("+column+" IS NOT NULL) NOT VALID;", "expand", "review", true, "share_row_exclusive_lock"),
					statement("ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(check)+";", "migrate", "review", true, "table_scan"),
					statement("ALTER TABLE "+table+" ALTER COLUMN "+column+" SET NOT NULL;", "contract", "review", true, "access_exclusive_lock"),
					statement("ALTER TABLE "+table+" DROP CONSTRAINT "+quote(check)+";", "contract", "review", true, "access_exclusive_lock"),
				)
			default:
				return nil, nil, nil, fmt.Errorf("invalid NOT NULL strategy %q", strategy)
			}
		}
	}
	statements = append(statements, commentModification("COLUMN", table+"."+column, before.Comment, after.Comment)...)
	return statements, nil, nil, nil
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
	if object.Parent != nil || constraintPropagatedByPartitionParent(object, desired) {
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
		if item.Kind != change.Drop || coveredByParent(item.ID, schemas, tables) || implicitConstraintIndexDrop(item, changes) || propagatedPartitionChildDrop(item, changes) || propagatedPartitionChildRebuild(item, changes) {
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
		parts = append(parts, "GENERATED ALWAYS AS ("+column.Generated.Expression+") STORED")
	}
	if column.Default != nil && column.Serial == nil {
		parts = append(parts, "DEFAULT "+*column.Default)
	}
	if column.NotNull {
		parts = append(parts, "NOT NULL")
	}
	return strings.Join(parts, " ")
}

func commentModification(kind, identifier string, before, after *string) []protocol.Statement {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	value := "NULL"
	if after != nil {
		value = literal(*after)
	}
	return []protocol.Statement{statement("COMMENT ON "+kind+" "+identifier+" IS "+value+";", "expand", "safe", true)}
}

func statement(sql, phase, safety string, transactional bool, hazards ...string) protocol.Statement {
	return protocol.Statement{SQL: sql, Phase: phase, Safety: safety, Hazards: hazards, NonTransactional: !transactional}
}

func manualWorkStatement(work protocol.ManualWork, hazards ...string) protocol.Statement {
	lines := []string{"-- MANUAL CONTRACT: " + work.Summary}
	for _, verification := range work.VerificationSQL {
		lines = append(lines, "-- Verify: "+verification)
	}
	lines = append(lines, work.Statements...)
	return protocol.Statement{
		SQL: strings.Join(lines, "\n"), Phase: "manual", Safety: "manual",
		Hazards: hazards, Manual: &work, NonTransactional: work.ExecutionMode == "non_transactional",
	}
}

func appendStatement(result *protocol.Result, item protocol.Statement) {
	result.Statements = append(result.Statements, item)
}

func rebuildBatches(result *protocol.Result) error {
	orderedPhases := []string{"expand", "migrate", "contract", "manual"}
	byPhase := make(map[string][]protocol.Statement, len(orderedPhases))
	for _, item := range result.Statements {
		if item.Phase != "expand" && item.Phase != "migrate" && item.Phase != "contract" && item.Phase != "manual" {
			return fmt.Errorf("unknown statement phase %q", item.Phase)
		}
		byPhase[item.Phase] = append(byPhase[item.Phase], item)
	}
	result.Statements, result.Batches = nil, nil
	for _, phase := range orderedPhases {
		for _, item := range byPhase[phase] {
			result.Statements = append(result.Statements, item)
			transactional := !item.NonTransactional
			if len(result.Batches) > 0 {
				last := &result.Batches[len(result.Batches)-1]
				if last.Phase == phase && last.Transactional == transactional {
					last.Statements = append(last.Statements, item)
					continue
				}
			}
			result.Batches = append(result.Batches, protocol.Batch{
				ID: fmt.Sprintf("batch-%03d", len(result.Batches)+1), Phase: phase,
				Transactional: transactional, Statements: []protocol.Statement{item},
			})
		}
	}
	return nil
}

func rebuildUnsortedBatches(result *protocol.Result) error {
	statements := append([]protocol.Statement(nil), result.Statements...)
	result.Statements, result.Batches = nil, nil
	for _, item := range statements {
		result.Statements = append(result.Statements, item)
		transactional := !item.NonTransactional
		if len(result.Batches) > 0 {
			last := &result.Batches[len(result.Batches)-1]
			if last.Transactional == transactional {
				last.Statements = append(last.Statements, item)
				continue
			}
		}
		result.Batches = append(result.Batches, protocol.Batch{
			ID: fmt.Sprintf("batch-%03d", len(result.Batches)+1), Phase: item.Phase,
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
