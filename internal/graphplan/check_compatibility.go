package graphplan

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

type checkAcceptanceRelation int

const (
	checkAcceptanceUnknown checkAcceptanceRelation = iota
	checkAcceptanceEqual
	checkAcceptanceWider
	checkAcceptanceNarrower
)

var (
	checkInPattern             = regexp.MustCompile(`(?is)^((?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*)(?:\.(?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*))?)\s+in\s*\((.*)\)$`)
	checkAnyPattern            = regexp.MustCompile(`(?is)^((?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*)(?:\.(?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*))?)\s*=\s*any\s*\(\s*array\[(.*)\](?:::[a-z0-9_\."\[\] ]+)?\s*\)$`)
	checkCastPattern           = regexp.MustCompile(`(?is)^::[a-z0-9_\."\[\] ]+$`)
	checkRegexPattern          = regexp.MustCompile(`(?is)^((?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*)(?:\.(?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*))?)\s*~\s*(.+)$`)
	checkSafeASCIIRangePattern = regexp.MustCompile(`^\^\[([a-zA-Z0-9])-([a-zA-Z0-9])\]\{([0-9]+)\}\$$`)
	checkNotNullPattern        = regexp.MustCompile(`(?is)^((?:"(?:[^"]|"")+"|[a-z_][a-z0-9_$]*))\s+is\s+not\s+null$`)
)

// renderCheckConstraintReplacement builds an acceptance-compatible envelope
// for a same-identity CHECK mutation. A proven widening reaches the desired
// schema in expand. A proven narrowing keeps the old CHECK until contract. An
// unknown relationship removes the old CHECK during overlap so neither
// application version is rejected, then restores desired enforcement after
// legacy writers drain.
func renderCheckConstraintReplacement(before, after pgschema.Constraint, current, desired *pgschema.Snapshot) ([]protocol.Statement, []string) {
	if before.Type != pgschema.ConstraintCheck || after.Type != pgschema.ConstraintCheck || before.Table != after.Table {
		return nil, []string{"check_constraint_identity_transition:" + after.ObjectID().String()}
	}
	if before.Parent != nil || after.Parent != nil {
		return nil, []string{"partitioned_constraint_modify:" + after.ObjectID().String()}
	}
	if !checkPredicateTransitionShape(before, after) {
		return nil, []string{"check_constraint_shape_transition:" + after.ObjectID().String()}
	}

	relation := classifyCheckAcceptanceWithSnapshots(before, after, current, desired)
	if relation == checkAcceptanceWider && before.Validated {
		return renderWideningCheckReplacement(before, after), nil
	}
	if relation == checkAcceptanceNarrower {
		if before.Name != after.Name {
			return renderCrossNameNarrowingCheckReplacement(before, after), nil
		}
		temporary := checkReplacementName(before, after)
		if constraintNameExists(current, before.Table, temporary) || constraintNameExists(desired, after.Table, temporary) {
			return nil, []string{"check_constraint_temporary_name_collision:" + after.ObjectID().String()}
		}
		return renderNarrowingCheckReplacement(before, after, temporary), nil
	}
	return renderUnknownCheckReplacement(before, after), nil
}

func checkPredicateTransitionShape(before, after pgschema.Constraint) bool {
	left, right := before, after
	left.Name, right.Name = "", ""
	left.Definition, right.Definition = "", ""
	left.CheckExpression, right.CheckExpression = "", ""
	left.Validated, right.Validated = false, false
	left.Comment, right.Comment = nil, nil
	return left == right
}

func renderCrossNameNarrowingCheckReplacement(before, after pgschema.Constraint) []protocol.Statement {
	table := qualified(after.Table.Schema, after.Table.Name)
	add := withTimeoutGuidance(statement(
		renderCheckConstraintAdd(after, after.Name, true),
		protocol.PhaseContract, "review", true,
		"check_acceptance_narrowed", "cross_name_enforcement_family", "post_drain_writers_required", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{add}
	if after.Validated {
		validate := withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(after.Name)+";",
			protocol.PhaseContract, "review", true,
			"check_acceptance_narrowed", "cross_name_enforcement_family", "constraint_validation", "table_scan", "share_update_exclusive_lock",
		), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseContract, "review", true,
		"check_acceptance_narrowed", "cross_name_enforcement_family", "constraint_rebuild", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	if after.Validated {
		drop.BatchBoundaryBefore = true
	}
	statements = append(statements, drop)
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "constraint_rebuild", "cross_name_enforcement_family",
		))
	}
	return statements
}

func renderWideningCheckReplacement(before, after pgschema.Constraint) []protocol.Statement {
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"check_acceptance_widened", "constraint_rebuild", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	add := withTimeoutGuidance(statement(
		renderCheckConstraintAdd(after, after.Name, after.Validated),
		protocol.PhaseExpand, "review", true,
		"check_acceptance_widened", "constraint_rebuild", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{drop, add}
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseExpand, "safe", true, "constraint_rebuild",
		))
	}
	if after.Validated {
		validate := withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(after.Name)+";",
			protocol.PhaseExpand, "review", true,
			"check_acceptance_widened", "constraint_validation", "table_scan", "share_update_exclusive_lock",
		), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	return statements
}

func renderNarrowingCheckReplacement(before, after pgschema.Constraint, temporary string) []protocol.Statement {
	table := qualified(after.Table.Schema, after.Table.Name)
	add := withTimeoutGuidance(statement(
		renderCheckConstraintAdd(after, temporary, true),
		protocol.PhaseContract, "review", true,
		"check_acceptance_narrowed", "post_drain_writers_required", "constraint_rebuild", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{add}
	if after.Validated {
		validate := withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(temporary)+";",
			protocol.PhaseContract, "review", true,
			"check_acceptance_narrowed", "constraint_validation", "table_scan", "share_update_exclusive_lock",
		), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseContract, "review", true,
		"check_acceptance_narrowed", "constraint_rebuild", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	if after.Validated {
		drop.BatchBoundaryBefore = true
	}
	statements = append(statements,
		drop,
		withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" RENAME CONSTRAINT "+quote(temporary)+" TO "+quote(after.Name)+";",
			protocol.PhaseContract, "review", true,
			"check_acceptance_narrowed", "constraint_rebuild", "brief_lock", "access_exclusive_lock",
		), 30000, 3000),
	)
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "constraint_rebuild",
		))
	}
	return statements
}

func renderUnknownCheckReplacement(before, after pgschema.Constraint) []protocol.Statement {
	table := qualified(after.Table.Schema, after.Table.Name)
	drop := withTimeoutGuidance(statement(
		"ALTER TABLE "+table+" DROP CONSTRAINT "+quote(before.Name)+";",
		protocol.PhaseExpand, "review", true,
		"temporary_check_enforcement_removed", "check_acceptance_unknown", "brief_lock", "access_exclusive_lock",
	), 30000, 3000)
	add := withTimeoutGuidance(statement(
		renderCheckConstraintAdd(after, after.Name, after.Validated),
		protocol.PhaseContract, "review", true,
		"restore_check_enforcement", "post_drain_writers_required", "possible_overlap_window_invalid_rows", "access_exclusive_lock",
	), 30000, 3000)
	statements := []protocol.Statement{drop, add}
	if after.Validated {
		validate := withTimeoutGuidance(statement(
			"ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quote(after.Name)+";",
			protocol.PhaseContract, "review", true,
			"restore_check_enforcement", "constraint_validation", "table_scan", "share_update_exclusive_lock",
		), 1200000, 3000)
		validate.BatchBoundaryBefore = true
		statements = append(statements, validate)
	}
	if after.Comment != nil {
		statements = append(statements, statement(
			"COMMENT ON CONSTRAINT "+quote(after.Name)+" ON "+table+" IS "+literal(*after.Comment)+";",
			protocol.PhaseContract, "safe", true, "restore_check_enforcement",
		))
	}
	return statements
}

func renderCheckConstraintAdd(object pgschema.Constraint, name string, forceNotValid bool) string {
	definition := strings.TrimSpace(object.Definition)
	if forceNotValid && !containsNotValid(definition) {
		definition += " NOT VALID"
	}
	return "ALTER TABLE " + qualified(object.Table.Schema, object.Table.Name) + " ADD CONSTRAINT " + quote(name) + " " + definition + ";"
}

func containsNotValid(definition string) bool {
	words := strings.Fields(strings.ToUpper(definition))
	for index := 0; index+1 < len(words); index++ {
		if words[index] == "NOT" && words[index+1] == "VALID" {
			return true
		}
	}
	return false
}

func checkReplacementName(before, after pgschema.Constraint) string {
	digest := sha256.Sum256([]byte(before.ObjectID().String() + "\x00" + before.Definition + "\x00" + after.Definition))
	return "onwardpg_tmpcheck_" + hex.EncodeToString(digest[:8])
}

func constraintNameExists(snapshot *pgschema.Snapshot, table pgschema.ID, name string) bool {
	if snapshot == nil {
		return false
	}
	_, exists := snapshot.Object((pgschema.Constraint{Table: table, Name: name}).ObjectID())
	return exists
}

func classifyCheckAcceptance(before, after pgschema.Constraint) checkAcceptanceRelation {
	return classifyCheckAcceptanceWithSnapshots(before, after, nil, nil)
}

func classifyCheckAcceptanceWithSnapshots(before, after pgschema.Constraint, current, desired *pgschema.Snapshot) checkAcceptanceRelation {
	oldOperand, oldValues, oldOK := finiteCheckDomain(checkExpression(before))
	newOperand, newValues, newOK := finiteCheckDomain(checkExpression(after))
	if oldOK && newOK && oldOperand == newOperand {
		oldInNew := stringSetSubset(oldValues, newValues)
		newInOld := stringSetSubset(newValues, oldValues)
		switch {
		case oldInNew && newInOld:
			return checkAcceptanceEqual
		case oldInNew:
			return checkAcceptanceWider
		case newInOld:
			return checkAcceptanceNarrower
		}
	}
	invariants := preservedNotNullColumns(before.Table, current, desired)
	oldImpliesNew := checkExpressionImplies(checkExpression(before), checkExpression(after), invariants)
	newImpliesOld := checkExpressionImplies(checkExpression(after), checkExpression(before), invariants)
	switch {
	case oldImpliesNew && newImpliesOld:
		return checkAcceptanceEqual
	case oldImpliesNew:
		return checkAcceptanceWider
	case newImpliesOld:
		return checkAcceptanceNarrower
	default:
		return checkAcceptanceUnknown
	}
}

// checkExpressionImplies is deliberately a bounded proof, not a general SQL
// theorem prover. It converts top-level AND/OR structure into a small DNF and
// proves each old conjunction has a desired conjunction whose atoms are a
// subset. That is sound for CHECK acceptance under SQL three-valued logic: if
// an atom in the desired conjunction were FALSE, the old conjunction that
// contains it would also be FALSE. Unknown atoms simply prevent a proof.
func checkExpressionImplies(before, after string, notNullColumns map[string]bool) bool {
	beforeTerms, beforeOK := checkDNF(before)
	afterTerms, afterOK := checkDNF(after)
	if !beforeOK || !afterOK {
		return false
	}
	for _, beforeTerm := range beforeTerms {
		implied := false
		for _, afterTerm := range afterTerms {
			if checkConjunctionImplies(beforeTerm, afterTerm, notNullColumns) {
				implied = true
				break
			}
		}
		if !implied {
			return false
		}
	}
	return true
}

const (
	maxCheckDNFTerms = 64
	maxCheckDNFAtoms = 32
)

func checkDNF(expression string) ([][]string, bool) {
	expression = stripOuterParentheses(strings.TrimSpace(expression))
	if expression == "" {
		return nil, false
	}
	if parts := splitTopLevelCheckBoolean(expression, "OR"); len(parts) > 1 {
		var result [][]string
		for _, part := range parts {
			terms, ok := checkDNF(part)
			if !ok || len(result)+len(terms) > maxCheckDNFTerms {
				return nil, false
			}
			result = append(result, terms...)
		}
		return result, true
	}
	if parts := splitTopLevelCheckBoolean(expression, "AND"); len(parts) > 1 {
		result := [][]string{{}}
		for _, part := range parts {
			terms, ok := checkDNF(part)
			if !ok {
				return nil, false
			}
			var product [][]string
			for _, left := range result {
				for _, right := range terms {
					if len(left)+len(right) > maxCheckDNFAtoms || len(product) >= maxCheckDNFTerms {
						return nil, false
					}
					term := append(append([]string(nil), left...), right...)
					product = append(product, term)
				}
			}
			result = product
		}
		return result, true
	}
	return [][]string{{stripOuterParentheses(expression)}}, true
}

func splitTopLevelCheckBoolean(expression, keyword string) []string {
	var parts []string
	depth := 0
	start := 0
	singleQuoted, doubleQuoted := false, false
	for index := 0; index < len(expression); index++ {
		switch expression[index] {
		case '\'':
			if !doubleQuoted {
				if singleQuoted && index+1 < len(expression) && expression[index+1] == '\'' {
					index++
					continue
				}
				singleQuoted = !singleQuoted
			}
		case '"':
			if !singleQuoted {
				if doubleQuoted && index+1 < len(expression) && expression[index+1] == '"' {
					index++
					continue
				}
				doubleQuoted = !doubleQuoted
			}
		case '(':
			if !singleQuoted && !doubleQuoted {
				depth++
			}
		case ')':
			if !singleQuoted && !doubleQuoted {
				depth--
			}
		default:
			if depth == 0 && !singleQuoted && !doubleQuoted && checkKeywordAt(expression, index, keyword) {
				parts = append(parts, strings.TrimSpace(expression[start:index]))
				index += len(keyword) - 1
				start = index + 1
			}
		}
	}
	if len(parts) == 0 {
		return []string{expression}
	}
	parts = append(parts, strings.TrimSpace(expression[start:]))
	return parts
}

func checkKeywordAt(expression string, index int, keyword string) bool {
	if index+len(keyword) > len(expression) || !strings.EqualFold(expression[index:index+len(keyword)], keyword) {
		return false
	}
	if index > 0 && isCheckIdentifierByte(expression[index-1]) {
		return false
	}
	return index+len(keyword) == len(expression) || !isCheckIdentifierByte(expression[index+len(keyword)])
}

func isCheckIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func checkConjunctionImplies(before, after []string, notNullColumns map[string]bool) bool {
	for _, wanted := range after {
		wanted = stripOuterParentheses(strings.TrimSpace(wanted))
		matched := false
		for _, available := range before {
			available = stripOuterParentheses(strings.TrimSpace(available))
			if available == wanted || finiteAtomImpliesFinite(available, wanted) || finiteAtomImpliesRegex(available, wanted) {
				matched = true
				break
			}
		}
		if !matched {
			match := checkNotNullPattern.FindStringSubmatch(wanted)
			matched = match != nil && notNullColumns[unquoteCheckIdentifier(match[1])]
		}
		if !matched {
			return false
		}
	}
	return true
}

func finiteAtomImpliesFinite(before, after string) bool {
	beforeOperand, beforeValues, beforeOK := finiteCheckDomain(before)
	afterOperand, afterValues, afterOK := finiteCheckDomain(after)
	return beforeOK && afterOK && beforeOperand == afterOperand && stringSetSubset(beforeValues, afterValues)
}

func finiteAtomImpliesRegex(before, after string) bool {
	operand, values, ok := finiteCheckDomain(before)
	if !ok {
		return false
	}
	match := checkRegexPattern.FindStringSubmatch(after)
	if match == nil || strings.TrimSpace(match[1]) != operand {
		return false
	}
	pattern, ok := parseCheckStringLiteral(match[2])
	if !ok {
		return false
	}
	rangeMatch := checkSafeASCIIRangePattern.FindStringSubmatch(pattern)
	if rangeMatch == nil || len(rangeMatch[1]) != 1 || len(rangeMatch[2]) != 1 {
		return false
	}
	wantedLength := 0
	for _, value := range rangeMatch[3] {
		wantedLength = wantedLength*10 + int(value-'0')
	}
	minimum, maximum := rangeMatch[1][0], rangeMatch[2][0]
	for value := range values {
		if len(value) != wantedLength {
			return false
		}
		for index := 0; index < len(value); index++ {
			if value[index] < minimum || value[index] > maximum {
				return false
			}
		}
	}
	return true
}

func preservedNotNullColumns(table pgschema.ID, current, desired *pgschema.Snapshot) map[string]bool {
	result := make(map[string]bool)
	if current == nil || desired == nil {
		return result
	}
	for _, object := range current.Objects() {
		before, ok := object.(pgschema.Column)
		if !ok || before.Table != table || !before.NotNull {
			continue
		}
		afterObject, exists := desired.Object(before.ObjectID())
		after, ok := afterObject.(pgschema.Column)
		if exists && ok && after.NotNull {
			result[before.Name] = true
		}
	}
	return result
}

func unquoteCheckIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
	}
	return value
}

func finiteCheckDomain(expression string) (string, map[string]bool, bool) {
	expression = stripOuterParentheses(strings.TrimSpace(expression))
	match := checkAnyPattern.FindStringSubmatch(expression)
	if match == nil {
		match = checkInPattern.FindStringSubmatch(expression)
	}
	if match == nil {
		return "", nil, false
	}
	values, ok := parseCheckStringLiterals(match[2])
	if !ok {
		return "", nil, false
	}
	return strings.TrimSpace(match[1]), values, true
}

func checkExpression(constraint pgschema.Constraint) string {
	if strings.TrimSpace(constraint.CheckExpression) != "" {
		return constraint.CheckExpression
	}
	definition := strings.TrimSpace(constraint.Definition)
	if len(definition) < len("CHECK") || !strings.EqualFold(definition[:len("CHECK")], "CHECK") {
		return ""
	}
	open := strings.Index(definition, "(")
	if open < 0 {
		return ""
	}
	depth, quoted := 0, false
	for index := open; index < len(definition); index++ {
		switch definition[index] {
		case '\'':
			if quoted && index+1 < len(definition) && definition[index+1] == '\'' {
				index++
				continue
			}
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
				if depth == 0 {
					return definition[open+1 : index]
				}
			}
		}
	}
	return ""
}

func stripOuterParentheses(value string) string {
	for len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' && outerParenthesesCover(value) {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func outerParenthesesCover(value string) bool {
	depth, quoted := 0, false
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\'':
			if quoted && index+1 < len(value) && value[index+1] == '\'' {
				index++
				continue
			}
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
				if depth == 0 && index != len(value)-1 {
					return false
				}
			}
		}
	}
	return depth == 0 && !quoted
}

func parseCheckStringLiterals(value string) (map[string]bool, bool) {
	items := splitCheckLiteralList(value)
	if len(items) == 0 {
		return nil, false
	}
	result := make(map[string]bool, len(items))
	for _, item := range items {
		literalValue, ok := parseCheckStringLiteral(item)
		if !ok {
			return nil, false
		}
		result[literalValue] = true
	}
	return result, true
}

func splitCheckLiteralList(value string) []string {
	var result []string
	start, quoted := 0, false
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\'':
			if quoted && index+1 < len(value) && value[index+1] == '\'' {
				index++
				continue
			}
			quoted = !quoted
		case ',':
			if !quoted {
				result = append(result, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	if quoted {
		return nil
	}
	result = append(result, strings.TrimSpace(value[start:]))
	return result
}

func parseCheckStringLiteral(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '\'' {
		return "", false
	}
	var result strings.Builder
	index := 1
	closed := false
	for index < len(value) {
		if value[index] != '\'' {
			result.WriteByte(value[index])
			index++
			continue
		}
		if index+1 < len(value) && value[index+1] == '\'' {
			result.WriteByte('\'')
			index += 2
			continue
		}
		index++
		closed = true
		break
	}
	if !closed {
		return "", false
	}
	rest := strings.TrimSpace(value[index:])
	if rest != "" && !checkCastPattern.MatchString(rest) {
		return "", false
	}
	return result.String(), true
}

func stringSetSubset(left, right map[string]bool) bool {
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}
