package graphplan

import (
	"reflect"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func uniqueIndexAcceptanceWider(before, after pgschema.Index) bool {
	if !before.Unique || !after.Unique || before.Table != after.Table || before.Name != after.Name ||
		before.Constraint != "" || after.Constraint != "" || before.Parent != nil || after.Parent != nil ||
		before.Method != after.Method || after.NullsNotDistinct && !before.NullsNotDistinct {
		return false
	}
	oldPosition := 0
	for _, nextPart := range after.Parts {
		if oldPosition < len(before.Parts) && reflect.DeepEqual(before.Parts[oldPosition], nextPart) {
			oldPosition++
		}
	}
	if oldPosition != len(before.Parts) {
		return false
	}
	switch {
	case before.Predicate == after.Predicate:
		return true
	case strings.TrimSpace(before.Predicate) == "":
		return true
	case strings.TrimSpace(after.Predicate) == "":
		return false
	default:
		return checkExpressionImplies(after.Predicate, before.Predicate, nil)
	}
}

func renderUniqueIndexAcceptanceReplacement(before, after pgschema.Index, current *pgschema.Snapshot, options Options) ([]protocol.Statement, []string, error) {
	if !before.Unique || before.Constraint != "" || after.Constraint != "" || before.Parent != nil || after.Parent != nil {
		return nil, []string{"unique_index_acceptance_transition:" + after.ObjectID().String()}, nil
	}
	if hasExternalDependentExcept(current, before.ObjectID(), before.ObjectID()) {
		return nil, []string{"unique_index_acceptance_dependent:" + before.ObjectID().String()}, nil
	}
	wider := !after.Unique || uniqueIndexAcceptanceWider(before, after)
	createSQL, unsupported := renderIndex(after)
	if unsupported != "" {
		return nil, []string{unsupported}, nil
	}
	createPhase := protocol.PhaseContract
	createHazards := []string{"restore_unique_index_enforcement", "post_drain_writers_required", "duplicate_rows_possible", "index_build"}
	if wider {
		createPhase = protocol.PhaseExpand
		createHazards = []string{"unique_acceptance_widened", "index_build"}
	}
	var statements []protocol.Statement
	if options.ConcurrentIndexes {
		drop := withTimeoutGuidance(statement(
			"DROP INDEX CONCURRENTLY "+qualified(before.Table.Schema, before.Name)+";",
			protocol.PhaseExpand, "review", false,
			"unique_acceptance_relaxed", "temporary_uniqueness_unenforced", "duplicate_rows_possible", "concurrent_index_drop",
		), 1200000, 3000)
		createSQL = strings.Replace(createSQL, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
		createSQL = strings.Replace(createSQL, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1)
		create := withTimeoutGuidance(statement(createSQL, createPhase, "review", false, createHazards...), 1200000, 3000)
		statements = append(statements, drop, create)
	} else {
		statements = append(statements, statement(
			"DROP INDEX "+qualified(before.Table.Schema, before.Name)+";",
			protocol.PhaseExpand, "review", true,
			"unique_acceptance_relaxed", "temporary_uniqueness_unenforced", "duplicate_rows_possible", "blocking_lock",
		))
		statements = append(statements, statement(createSQL, createPhase, "review", true, createHazards...))
	}
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON INDEX "+qualified(after.Table.Schema, after.Name)+" IS "+literal(*after.Comment)+";",
			createPhase, "safe", true,
		))
	}
	return statements, nil, nil
}

func renderForeignKeyAcceptanceReplacement(before, after pgschema.Constraint, desired *pgschema.Snapshot) []protocol.Statement {
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"foreign_key_acceptance_relaxed", "referential_enforcement_removed", "orphan_rows_possible", "referential_action_behavior_change", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	definition := strings.TrimSpace(after.Definition)
	partitioned := tableIsPartitioned(desired, after.Table)
	if !partitioned && !containsNotValid(definition) {
		definition += " NOT VALID"
	}
	add := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" ADD CONSTRAINT "+quote(after.Name)+" "+definition+";",
		protocol.PhaseContract, "review", true,
		"restore_foreign_key_enforcement", "new_writes_fenced", "post_drain_writers_required", "orphan_rows_possible", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{drop, add}
	if after.Validated && !partitioned {
		validate := withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(after.Name)+";",
			protocol.PhaseContract, "review", true,
			"restore_foreign_key_enforcement", "constraint_validation", "table_scan", "share_update_exclusive_lock",
		), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "restore_foreign_key_enforcement",
		))
	}
	return statements
}

// uniqueConstraintAcceptanceWider proves the narrow structural case in which
// every state accepted by the old unique or primary-key constraint is accepted
// by the new one: the desired key adds key parts while retaining every old part in order.
// More involved predicate, expression, NULL, dependency, and partition cases
// stay on the conservative path until they have their own proof.
func uniqueConstraintAcceptanceWider(before, after pgschema.Constraint, current, desired *pgschema.Snapshot) bool {
	if before.Type != after.Type || (before.Type != pgschema.ConstraintUnique && before.Type != pgschema.ConstraintPrimary) ||
		before.Table != after.Table || before.Name != after.Name || before.Parent != nil || after.Parent != nil ||
		before.Deferrable != after.Deferrable || before.Deferred != after.Deferred || before.NoInherit != after.NoInherit {
		return false
	}
	oldIndex, oldExists := backingIndexForConstraint(current, before)
	newIndex, newExists := backingIndexForConstraint(desired, after)
	if !oldExists || !newExists || oldIndex.Parent != nil || newIndex.Parent != nil ||
		!oldIndex.Unique || !newIndex.Unique || oldIndex.Primary != (before.Type == pgschema.ConstraintPrimary) || newIndex.Primary != (after.Type == pgschema.ConstraintPrimary) || oldIndex.Exclusion || newIndex.Exclusion ||
		oldIndex.Predicate != "" || newIndex.Predicate != "" || oldIndex.Method != newIndex.Method ||
		len(newIndex.Parts) <= len(oldIndex.Parts) ||
		hasExternalDependentExcept(current, before.ObjectID(), oldIndex.ObjectID()) ||
		hasExternalDependentExcept(desired, after.ObjectID(), newIndex.ObjectID()) ||
		hasExternalDependentExcept(current, oldIndex.ObjectID(), before.ObjectID()) ||
		hasExternalDependentExcept(desired, newIndex.ObjectID(), after.ObjectID()) {
		return false
	}
	// Standard UNIQUE permits duplicate NULL-containing keys. Moving to NULLS
	// NOT DISTINCT would tighten that behavior and is therefore not widening.
	if before.Type == pgschema.ConstraintUnique && !oldIndex.NullsNotDistinct && newIndex.NullsNotDistinct {
		return false
	}
	oldPart := 0
	for _, candidate := range newIndex.Parts {
		if oldPart < len(oldIndex.Parts) && reflect.DeepEqual(oldIndex.Parts[oldPart], candidate) {
			oldPart++
		}
	}
	return oldPart == len(oldIndex.Parts)
}

func hasExternalDependentExcept(snapshot *pgschema.Snapshot, dependency pgschema.ID, allowed pgschema.ID) bool {
	if snapshot == nil {
		return true
	}
	for _, id := range snapshot.IDs() {
		if id == allowed || id == dependency {
			continue
		}
		if containsID(snapshot.Dependencies(id), dependency) {
			return true
		}
	}
	return false
}

func tableIsPartitioned(snapshot *pgschema.Snapshot, id pgschema.ID) bool {
	if snapshot == nil {
		return false
	}
	object, exists := snapshot.Object(id)
	table, ok := object.(pgschema.Table)
	return exists && ok && table.Partition != nil
}

func renderRelaxedUniqueConstraintReplacement(before, after pgschema.Constraint, current, desired *pgschema.Snapshot, options Options) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if options.ConcurrentIndexes {
		statements, consumed, unsupported, err := renderContinuousConstraintReplacement(before, after, current, desired)
		if err != nil || len(unsupported) > 0 {
			return nil, consumed, unsupported, err
		}
		return withPhase(statements, protocol.PhaseExpand, "review", "unique_acceptance_widened"), consumed, nil, nil
	}
	table := qualified(after.Table.Schema, after.Table.Name)
	statements := []protocol.Statement{
		statement(
			"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
			protocol.PhaseExpand, "review", true,
			"unique_acceptance_widened", "constraint_rebuild", "blocking_lock", "access_exclusive_lock",
		),
	}
	statements = append(statements, withPhase(renderConstraintCreate(after), protocol.PhaseExpand, "review", "unique_acceptance_widened", "constraint_rebuild")...)
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseExpand, "safe", true, "constraint_rebuild",
		))
	}
	return statements, nil, nil, nil
}

func renderUnknownKeyConstraintReplacement(before, after pgschema.Constraint, current, desired *pgschema.Snapshot, options Options) ([]protocol.Statement, []pgschema.ID, []string, error) {
	if options.ConcurrentIndexes {
		statements, consumed, unsupported, err := renderContinuousConstraintReplacement(before, after, current, desired)
		if err != nil || len(unsupported) > 0 {
			return nil, consumed, unsupported, err
		}
		return withPhase(statements, protocol.PhaseContract, "review", "post_drain_unique_replacement"), consumed, nil, nil
	}
	oldIndex, exists := backingIndexForConstraint(current, before)
	partitioned := tableIsPartitioned(current, before.Table)
	if !exists || (!partitioned && (hasExternalDependentExcept(current, before.ObjectID(), oldIndex.ObjectID()) ||
		hasExternalDependentExcept(current, oldIndex.ObjectID(), before.ObjectID()))) {
		return nil, nil, []string{"unique_constraint_acceptance_dependent:" + before.ObjectID().String()}, nil
	}
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"unique_acceptance_unknown", "temporary_uniqueness_unenforced", "duplicate_rows_possible", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{drop}
	statements = append(statements, withPhase(
		renderConstraintCreate(after), protocol.PhaseContract, "review",
		"restore_unique_enforcement", "post_drain_writers_required", "duplicate_rows_possible", "blocking_index_build",
	)...)
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "restore_unique_enforcement",
		))
	}
	return statements, nil, nil, nil
}

func renderUnknownExclusionConstraintReplacement(before, after pgschema.Constraint, current *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	oldIndex, exists := backingIndexForConstraint(current, before)
	if !exists || hasExternalDependentExcept(current, before.ObjectID(), oldIndex.ObjectID()) ||
		hasExternalDependentExcept(current, oldIndex.ObjectID(), before.ObjectID()) {
		return nil, nil, []string{"exclusion_constraint_acceptance_dependent:" + before.ObjectID().String()}, nil
	}
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"exclusion_acceptance_unknown", "temporary_exclusion_unenforced", "conflicting_rows_possible", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{drop}
	statements = append(statements, withPhase(
		renderConstraintCreate(after), protocol.PhaseContract, "review",
		"restore_exclusion_enforcement", "post_drain_writers_required", "conflicting_rows_possible", "blocking_index_build",
	)...)
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "restore_exclusion_enforcement",
		))
	}
	return statements, nil, nil, nil
}

func renderUnknownConstraintKindReplacement(before, after pgschema.Constraint, current *pgschema.Snapshot) ([]protocol.Statement, []pgschema.ID, []string, error) {
	allowed := before.ObjectID()
	if oldIndex, exists := backingIndexForConstraint(current, before); exists {
		allowed = oldIndex.ObjectID()
		if hasExternalDependentExcept(current, oldIndex.ObjectID(), before.ObjectID()) {
			return nil, nil, []string{"constraint_kind_acceptance_dependent:" + before.ObjectID().String()}, nil
		}
	}
	if hasExternalDependentExcept(current, before.ObjectID(), allowed) {
		return nil, nil, []string{"constraint_kind_acceptance_dependent:" + before.ObjectID().String()}, nil
	}
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"constraint_kind_acceptance_unknown", "temporary_enforcement_removed", "overlap_rows_may_violate_desired_constraint", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	var restore []protocol.Statement
	if after.Validated && (after.Type == pgschema.ConstraintCheck || after.Type == pgschema.ConstraintForeign) && !containsNotValid(after.Definition) {
		restore = renderStagedConstraintCreate(after, protocol.PhaseContract)
	} else {
		restore = withPhase(
			renderConstraintCreate(after), protocol.PhaseContract, "review",
			"restore_desired_enforcement", "post_drain_writers_required", "overlap_rows_may_violate_desired_constraint",
		)
		if after.Comment != nil {
			restore = append(restore, statement(
				"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
				protocol.PhaseContract, "safe", true, "restore_desired_enforcement",
			))
		}
	}
	return append([]protocol.Statement{drop}, restore...), nil, nil, nil
}
