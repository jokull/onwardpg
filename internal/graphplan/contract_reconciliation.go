package graphplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

type reconciliationSpec struct {
	ChangeID           pgschema.ID
	TransitionID       string
	ScopeObjects       []string
	Reason             string
	BooleanSQL         string
	DesiredObject      pgschema.ID
	RequiresWriteFence bool
}

type reconciliationDecision struct {
	Spec             reconciliationSpec
	Strategy         string
	Work             *protocol.ManualWork
	ScopeFingerprint string
	GateIDs          []string
}

// reconciliationSpecForChange identifies desired enforcement which can reject
// rows admitted before contract. It deliberately covers both explicit
// expand-relax mutations and wholly new enforcement on an existing table.
func reconciliationSpecForChange(item change.Change, current, desired *pgschema.Snapshot) (reconciliationSpec, bool) {
	spec := reconciliationSpec{ChangeID: item.ID}
	beforeID, afterID := item.ID, item.ID
	if item.Before != nil {
		beforeID = item.Before.ObjectID()
	}
	if item.After != nil {
		afterID = item.After.ObjectID()
		spec.DesiredObject = afterID
	}
	spec.TransitionID = beforeID.String()
	if beforeID != afterID {
		spec.TransitionID += "->" + afterID.String()
	}
	spec.ScopeObjects = unionStrings([]string{beforeID.String()}, []string{afterID.String()})

	switch after := item.After.(type) {
	case pgschema.Constraint:
		if after.Parent != nil || constraintPropagatedByPartitionParent(after, desired) || !tableExistedBefore(current, after.Table) {
			return reconciliationSpec{}, false
		}
		if (after.Type == pgschema.ConstraintCheck || after.Type == pgschema.ConstraintForeign) && !after.Validated {
			return reconciliationSpec{}, false
		}
		if before, ok := item.Before.(pgschema.Constraint); ok {
			switch {
			case before.Type == pgschema.ConstraintCheck && after.Type == pgschema.ConstraintCheck:
				if classifyCheckAcceptanceWithSnapshots(before, after, current, desired) == checkAcceptanceWider && before.Validated {
					return reconciliationSpec{}, false
				}
			case (before.Type == pgschema.ConstraintUnique || before.Type == pgschema.ConstraintPrimary) && before.Type == after.Type:
				if uniqueConstraintAcceptanceWider(before, after, current, desired) {
					return reconciliationSpec{}, false
				}
			}
		}
		spec.Reason = "rows admitted before contract may violate desired enforcement " + after.ObjectID().String()
		spec.BooleanSQL = constraintReadinessBoolean(after, desired)
		spec.RequiresWriteFence = after.Type == pgschema.ConstraintUnique || after.Type == pgschema.ConstraintPrimary || after.Type == pgschema.ConstraintExclusion
		return spec, true

	case pgschema.Index:
		if !after.Unique || after.Constraint != "" || after.Parent != nil || !tableExistedBefore(current, after.Table) {
			return reconciliationSpec{}, false
		}
		if object, exists := desired.Object(after.Table); exists {
			if view, ok := object.(pgschema.View); ok && view.Materialized && !view.Populated {
				// An unpopulated materialized view has no readable or writable
				// rows to reconcile. Its index is catalog-derived contract work.
				return reconciliationSpec{}, false
			}
		}
		if before, ok := item.Before.(pgschema.Index); ok && uniqueIndexAcceptanceWider(before, after) {
			return reconciliationSpec{}, false
		}
		spec.Reason = "rows admitted before contract may conflict with desired unique index " + after.ObjectID().String()
		spec.BooleanSQL, _ = uniqueIndexReadinessBoolean(after, false)
		spec.RequiresWriteFence = true
		return spec, true

	case pgschema.Column:
		if item.Kind == change.Create && tableExistedBefore(current, after.Table) && after.NotNull && after.Default == nil && after.Identity == nil && after.Serial == nil && after.Generated == nil {
			spec = notNullReconciliationSpec(pgschema.Column{}, after)
			spec.ChangeID = item.ID
			return spec, true
		}
		// NOT NULL already has a rollout-strategy question. It is converted to
		// the same reconciliation model after that existing choice resolves so
		// developers are not asked two questions about one invariant.
		return reconciliationSpec{}, false
	}
	return reconciliationSpec{}, false
}

func notNullReconciliationSpec(before, after pgschema.Column) reconciliationSpec {
	id := after.ObjectID()
	return reconciliationSpec{
		ChangeID: id, TransitionID: id.String(), ScopeObjects: []string{id.String()}, DesiredObject: id,
		Reason:     "rows admitted before contract may leave " + id.String() + " NULL",
		BooleanSQL: "SELECT NOT EXISTS (SELECT 1 FROM " + qualified(after.Table.Schema, after.Table.Name) + " WHERE " + quote(after.Name) + " IS NULL);",
	}
}

func tableExistedBefore(current *pgschema.Snapshot, table pgschema.ID) bool {
	if current == nil {
		return false
	}
	_, exists := current.Object(table)
	return exists
}

func constraintReadinessBoolean(constraint pgschema.Constraint, desired *pgschema.Snapshot) string {
	table := qualified(constraint.Table.Schema, constraint.Table.Name)
	switch constraint.Type {
	case pgschema.ConstraintCheck:
		expression := strings.TrimSpace(checkExpression(constraint))
		if expression == "" {
			return ""
		}
		return "SELECT NOT EXISTS (SELECT 1 FROM " + table + " WHERE (" + expression + ") IS FALSE);"
	case pgschema.ConstraintUnique, pgschema.ConstraintPrimary:
		index, exists := backingIndexForConstraint(desired, constraint)
		if !exists {
			return ""
		}
		query, _ := uniqueIndexReadinessBoolean(index, constraint.Type == pgschema.ConstraintPrimary)
		return query
	case pgschema.ConstraintForeign:
		query, _ := foreignKeyReadinessBoolean(constraint)
		return query
	default:
		// Exclusion probes require exact pairwise operator semantics and remain
		// manual until that catalog state is typed.
		return ""
	}
}

func foreignKeyReadinessBoolean(constraint pgschema.Constraint) (string, bool) {
	if constraint.Type != pgschema.ConstraintForeign || constraint.Reference == nil ||
		len(constraint.ForeignKeyColumns) == 0 ||
		len(constraint.ForeignKeyColumns) != len(constraint.ReferencedColumns) ||
		len(constraint.ForeignKeyColumns) != len(constraint.ForeignKeyEqualityOperators) ||
		constraint.ForeignKeyMatch == pgschema.ForeignKeyMatchPartial {
		return "", false
	}
	if constraint.ForeignKeyMatch != pgschema.ForeignKeyMatchSimple && constraint.ForeignKeyMatch != pgschema.ForeignKeyMatchFull {
		return "", false
	}
	localNull, comparisons := make([]string, 0, len(constraint.ForeignKeyColumns)), make([]string, 0, len(constraint.ForeignKeyColumns))
	for index, localColumn := range constraint.ForeignKeyColumns {
		referencedColumn := constraint.ReferencedColumns[index]
		operator := constraint.ForeignKeyEqualityOperators[index]
		if localColumn == "" || referencedColumn == "" || operator.Schema == "" || operator.Name == "" {
			return "", false
		}
		localNull = append(localNull, "local_row."+quote(localColumn)+" IS NULL")
		comparisons = append(comparisons, "referenced_row."+quote(referencedColumn)+" OPERATOR("+quote(operator.Schema)+"."+operator.Name+") local_row."+quote(localColumn))
	}
	match := "EXISTS (SELECT 1 FROM " + qualified(constraint.Reference.Schema, constraint.Reference.Name) + " AS referenced_row WHERE " + strings.Join(comparisons, " AND ") + ")"
	var invalid string
	if constraint.ForeignKeyMatch == pgschema.ForeignKeyMatchSimple {
		invalid = "(" + strings.Join(localNull, " OR ") + ") IS FALSE AND NOT " + match
	} else {
		allNull := "(" + strings.Join(localNull, " AND ") + ")"
		anyNull := "(" + strings.Join(localNull, " OR ") + ")"
		invalid = "(" + anyNull + " AND NOT " + allNull + ") OR (NOT " + anyNull + " AND NOT " + match + ")"
	}
	return "SELECT NOT EXISTS (SELECT 1 FROM " + qualified(constraint.Table.Schema, constraint.Table.Name) + " AS local_row WHERE " + invalid + ");", true
}

func uniqueIndexReadinessBoolean(index pgschema.Index, primary bool) (string, bool) {
	if !index.Unique || index.Exclusion || index.Method != "btree" || len(index.Parts) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(index.Parts))
	for _, part := range index.Parts {
		if part.OpClass != nil && (!part.OpClass.IsDefault || len(part.OpClass.Parameters) > 0) {
			return "", false
		}
		var key string
		switch {
		case part.Column != "" && part.Expression == "":
			key = quote(part.Column)
		case part.Expression != "" && part.Column == "":
			key = "(" + part.Expression + ")"
		default:
			return "", false
		}
		if part.Collation != "" {
			key += " COLLATE " + part.Collation
		}
		keys = append(keys, key)
	}
	table := qualified(index.Table.Schema, index.Table.Name)
	filters := make([]string, 0, len(keys)+1)
	if strings.TrimSpace(index.Predicate) != "" {
		filters = append(filters, "("+index.Predicate+")")
	}
	if !index.NullsNotDistinct && !primary {
		for _, key := range keys {
			filters = append(filters, "("+key+") IS NOT NULL")
		}
	}
	where := ""
	if len(filters) > 0 {
		where = " WHERE " + strings.Join(filters, " AND ")
	}
	duplicateFree := "NOT EXISTS (SELECT 1 FROM " + table + where + " GROUP BY " + strings.Join(keys, ", ") + " HAVING count(*) > 1)"
	if !primary {
		return "SELECT " + duplicateFree + ";", true
	}
	nullFilters := make([]string, 0, len(keys)+1)
	if strings.TrimSpace(index.Predicate) != "" {
		nullFilters = append(nullFilters, "("+index.Predicate+")")
	}
	var nullParts []string
	for _, key := range keys {
		nullParts = append(nullParts, "("+key+") IS NULL")
	}
	nullFilters = append(nullFilters, "("+strings.Join(nullParts, " OR ")+")")
	nullFree := "NOT EXISTS (SELECT 1 FROM " + table + " WHERE " + strings.Join(nullFilters, " AND ") + ")"
	return "SELECT (" + nullFree + " AND " + duplicateFree + ");", true
}

func reconciliationQuestion(spec reconciliationSpec, currentFingerprint, desiredFingerprint string) protocol.Question {
	choices := []string{"manual_sql", "split_plan"}
	if spec.BooleanSQL != "" {
		choices = []string{"assert_only", "manual_sql", "split_plan"}
	}
	return protocol.Question{
		ID: "reconcile_contract:" + spec.TransitionID, Kind: "reconcile_contract", Key: spec.ChangeID.String(),
		Message: spec.Reason + ". Choose an exact post-drain assertion, reviewed cleanup SQL, or a later plan.",
		Choices: choices, ScopeObjects: spec.ScopeObjects,
		CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
	}
}

func reconciliationManualQuestion(spec reconciliationSpec, currentFingerprint, desiredFingerprint string) protocol.Question {
	return protocol.Question{
		ID: "reconcile_contract_sql:" + spec.TransitionID, Kind: "reconcile_contract_sql", Key: spec.ChangeID.String(),
		Message: "Supply reviewed post-drain cleanup/backfill SQL and at least one read-only Boolean verification query for " + spec.TransitionID + ".",
		Choices: []string{"provided"}, ScopeObjects: spec.ScopeObjects,
		CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
	}
}

func contractGateScope(currentFingerprint, desiredFingerprint, purpose string) string {
	sum := sha256.Sum256([]byte(currentFingerprint + "\x00" + desiredFingerprint + "\x00" + purpose))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func contractGateID(prefix, transitionID, scope string, position int) string {
	sum := sha256.Sum256([]byte(transitionID + "\x00" + scope + fmt.Sprintf("\x00%d", position)))
	return prefix + ":" + hex.EncodeToString(sum[:8])
}

func buildReconciliationGates(decision reconciliationDecision) ([]protocol.ContractGate, protocol.Reconciliation) {
	var queries []string
	kind := "data_assertion"
	if decision.Strategy == "assert_only" {
		queries = []string{decision.Spec.BooleanSQL}
	} else if decision.Work != nil {
		queries = append(queries, decision.Work.VerificationSQL...)
		kind = "manual_reconciliation"
	}
	gates := make([]protocol.ContractGate, 0, len(queries))
	for index, query := range queries {
		id := contractGateID("data", decision.Spec.TransitionID, decision.ScopeFingerprint, index)
		gates = append(gates, protocol.ContractGate{
			ID: id, Kind: kind, ScopeFingerprint: decision.ScopeFingerprint, Reason: decision.Spec.Reason, BooleanSQL: strings.TrimSpace(query),
		})
	}
	if decision.Spec.RequiresWriteFence {
		id := contractGateID("writers", decision.Spec.TransitionID, decision.ScopeFingerprint, len(gates))
		gates = append(gates, protocol.ContractGate{
			ID: id, Kind: "writer_attestation", ScopeFingerprint: decision.ScopeFingerprint,
			Reason:           "active desired-version writers must not reopen conflicts while contract restores " + decision.Spec.TransitionID,
			RequiredEvidence: []string{"write_fence:" + decision.Spec.TransitionID},
		})
	}
	ids := make([]string, 0, len(gates))
	for _, gate := range gates {
		ids = append(ids, gate.ID)
	}
	reconciliation := protocol.Reconciliation{
		TransitionID: decision.Spec.TransitionID, Strategy: decision.Strategy, Work: decision.Work, GateIDs: ids,
	}
	return gates, reconciliation
}

func writerAttestationGate(currentFingerprint, desiredFingerprint string) protocol.ContractGate {
	required := []string{"web", "workers", "scheduled_jobs", "queues", "connection_pools", "previews", "ad_hoc_writers"}
	sort.Strings(required)
	return protocol.ContractGate{
		ID: "writers:legacy", Kind: "writer_attestation",
		ScopeFingerprint: contractGateScope(currentFingerprint, desiredFingerprint, "legacy-writers"),
		Reason:           "pre-deployment write-capable cohorts must be upgraded, drained, isolated, or read-only before contract",
		RequiredEvidence: required,
	}
}

func inlineBooleanAssertion(gate protocol.ContractGate, transitionID string) protocol.Statement {
	query := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(gate.BooleanSQL), ";"))
	message := literal("onwardpg contract gate failed: " + gate.ID)
	sql := "DO $onwardpg$ BEGIN IF NOT COALESCE((" + query + "), false) THEN RAISE EXCEPTION " + message + "; END IF; END $onwardpg$;"
	return protocol.Statement{
		SQL: sql, Phase: protocol.PhaseContract, Safety: "review", Hazards: []string{"contract_data_assertion", "table_scan_possible"},
		NonTransactional: false, TransitionID: transitionID, StatementTimeoutMS: 1200000, LockTimeoutMS: 3000,
	}
}

func applyReconciliation(statements []protocol.Statement, decision reconciliationDecision, gates []protocol.ContractGate) []protocol.Statement {
	if len(gates) == 0 {
		return statements
	}
	for index := range statements {
		if restoresContractEnforcement(strings.ToUpper(statements[index].SQL)) {
			statements[index].Phase = protocol.PhaseContract
		}
	}
	insertion := -1
	targetConstraint := ""
	if decision.Spec.DesiredObject.Kind == pgschema.KindConstraint {
		targetConstraint = "ADD CONSTRAINT " + quote(decision.Spec.DesiredObject.Part)
	}
	for index, item := range statements {
		if item.Phase != protocol.PhaseContract {
			continue
		}
		if insertion == -1 {
			insertion = index
		}
		if strings.Contains(item.SQL, "ADD CONSTRAINT") && strings.Contains(item.SQL, "NOT VALID") && (targetConstraint == "" || strings.Contains(item.SQL, targetConstraint)) {
			insertion = index + 1
			break
		}
	}
	if insertion == -1 {
		return statements
	}
	inserted := make([]protocol.Statement, 0, 1+len(gates))
	var externalGateIDs []string
	for _, gate := range gates {
		if gate.Kind == "writer_attestation" {
			externalGateIDs = append(externalGateIDs, gate.ID)
		}
	}
	if decision.Work != nil {
		manual := manualWorkStatementPhase(*decision.Work, protocol.PhaseContract, "contract_reconciliation", "data_movement", "post_drain_writers_required")
		manual.RequiresGates = unionStrings(manual.RequiresGates, externalGateIDs)
		inserted = append(inserted, manual)
	}
	for _, gate := range gates {
		if gate.BooleanSQL == "" {
			continue
		}
		assertion := inlineBooleanAssertion(gate, decision.Spec.TransitionID)
		assertion.RequiresGates = unionStrings(assertion.RequiresGates, externalGateIDs)
		inserted = append(inserted, assertion)
	}
	result := make([]protocol.Statement, 0, len(statements)+len(inserted))
	result = append(result, statements[:insertion]...)
	result = append(result, inserted...)
	result = append(result, statements[insertion:]...)
	for index := range result {
		if result[index].Phase != protocol.PhaseContract {
			continue
		}
		result[index].TransitionID = decision.Spec.TransitionID
		result[index].RequiresGates = unionStrings(result[index].RequiresGates, externalGateIDs)
		if index >= insertion+len(inserted) {
			result[index].RequiresGates = unionStrings(result[index].RequiresGates, decision.GateIDs)
		}
	}
	return result
}

func sortedReconciliationIDs(values map[pgschema.ID]reconciliationDecision) []pgschema.ID {
	ids := make([]pgschema.ID, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// markContractEnforcementDispositions is the single registry for statements
// which restore row enforcement. A new renderer cannot silently add one: it
// must either reference an exact data gate or state a narrow catalog proof.
func markContractEnforcementDispositions(result *protocol.Result) []string {
	gateKinds := make(map[string]string, len(result.ContractGates))
	for _, gate := range result.ContractGates {
		gateKinds[gate.ID] = gate.Kind
	}
	var unsupported []string
	for index := range result.Statements {
		statement := &result.Statements[index]
		if statement.Phase != protocol.PhaseContract {
			continue
		}
		upper := strings.ToUpper(statement.SQL)
		if strings.Contains(upper, "ADD CONSTRAINT") && strings.Contains(upper, "NOT VALID") {
			statement.ContractDisposition = "write_fence"
			continue
		}
		if !restoresContractEnforcement(upper) {
			continue
		}
		if statement.Manual != nil {
			statement.ContractDisposition = "operator_owned_manual"
			continue
		}
		for _, gateID := range statement.RequiresGates {
			if gateKinds[gateID] == "data_assertion" || gateKinds[gateID] == "manual_reconciliation" {
				statement.ContractDisposition = "gated_restoration"
				break
			}
		}
		if statement.ContractDisposition != "" {
			continue
		}
		for _, hazard := range statement.Hazards {
			if strings.HasPrefix(hazard, "validated_check_reuse:") {
				statement.ContractDisposition = "catalog_proven_invariant"
				break
			}
			if hazard == "materialized_view_data_derived" {
				statement.ContractDisposition = "catalog_derived_relation"
				break
			}
			if hazard == "routine_return_type_change" {
				// The old enforcement is dropped and recreated inside one
				// transactional contract batch solely to replace its routine
				// dependency. No writer can observe or populate the gap.
				statement.ContractDisposition = "catalog_proven_invariant"
				break
			}
		}
		if statement.ContractDisposition == "" && strings.Contains(upper, " USING INDEX ") {
			statement.ContractDisposition = "catalog_proven_index"
		}
		if statement.ContractDisposition == "" && strings.HasPrefix(strings.TrimSpace(upper), "CREATE UNIQUE INDEX CONCURRENTLY \"ONWARDPG_TMPIDX_") {
			// Reserved replacement indexes are built while the old unique or
			// primary constraint is still live. PostgreSQL's existing
			// enforcement proves their input duplicate-free.
			statement.ContractDisposition = "catalog_proven_invariant"
		}
		if statement.ContractDisposition == "" && strings.HasPrefix(strings.TrimSpace(upper), "ALTER DOMAIN ") {
			statement.ContractDisposition = "postgres_atomic_validation"
		}
		if statement.ContractDisposition == "" {
			unsupported = append(unsupported, "contract_enforcement_missing_gate:"+statement.SQL)
		}
	}
	return unsupported
}

func restoresContractEnforcement(upperSQL string) bool {
	if strings.Contains(upperSQL, " NOT ENFORCED") {
		return false
	}
	if strings.Contains(upperSQL, "VALIDATE CONSTRAINT") || strings.Contains(upperSQL, " SET NOT NULL") ||
		strings.HasPrefix(strings.TrimSpace(upperSQL), "CREATE UNIQUE INDEX") {
		return true
	}
	if !strings.Contains(upperSQL, "ADD CONSTRAINT") {
		return false
	}
	return strings.Contains(upperSQL, " UNIQUE ") || strings.Contains(upperSQL, " PRIMARY KEY ") ||
		strings.Contains(upperSQL, " EXCLUDE ") || strings.Contains(upperSQL, " CHECK ") || strings.Contains(upperSQL, " FOREIGN KEY ")
}
