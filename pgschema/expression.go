package pgschema

import "strings"

// CanonicalExpression flattens nested AND and OR chains in an expression
// that PostgreSQL deparsed without pretty printing, such as a CHECK
// definition, an index predicate or expression, or a column default.
//
// PostgreSQL stores an expression as a tree and deparses the tree it stored.
// The grammar flattens a AND b AND c into one node, but other routes keep a
// nested node: BETWEEN becomes its own AND node, so
// CHECK (x BETWEEN 1 AND 5 AND y > 0) deparses as
// (((x >= 1) AND (x <= 5)) AND (y > 0)). A dump restores that text, the
// grammar flattens it, and the restored catalog deparses
// ((x >= 1) AND (x <= 5) AND (y > 0)). The two expressions are the same, so
// inspection keeps the flat form, which is what PostgreSQL deparses for the
// flat tree.
//
// Only a parenthesized operand of an AND chain whose own top level is an AND
// chain loses its parentheses, and likewise for OR. Everything else, the
// separators PostgreSQL printed included, is kept byte for byte. Quoted
// literals and identifiers are never read as operators.
func CanonicalExpression(expression string) string {
	return canonicalBooleanGroups(expression, false)
}

// canonicalBooleanGroups canonicalizes every parenthesized group in text and,
// when text is itself the inside of a group, flattens its own chain.
func canonicalBooleanGroups(text string, group bool) string {
	var builder strings.Builder
	depth, open := 0, -1
	singleQuoted, doubleQuoted := false, false
	for index := 0; index < len(text); index++ {
		character := text[index]
		switch {
		case character == '\'' && !doubleQuoted:
			singleQuoted = !singleQuoted
		case character == '"' && !singleQuoted:
			doubleQuoted = !doubleQuoted
		case singleQuoted || doubleQuoted:
		case character == '(':
			depth++
			if depth == 1 {
				open = index
				builder.WriteByte(character)
			}
			continue
		case character == ')':
			depth--
			if depth < 0 {
				return text
			}
			if depth == 0 {
				builder.WriteString(canonicalBooleanGroups(text[open+1:index], true))
				builder.WriteByte(character)
			}
			continue
		}
		if depth == 0 {
			builder.WriteByte(character)
		}
	}
	if depth != 0 || singleQuoted || doubleQuoted {
		return text
	}
	result := builder.String()
	if group {
		result = flattenBooleanChain(result)
	}
	return result
}

// flattenBooleanChain removes the parentheses of same-keyword operand groups
// in the chain that is the top level of one group's contents. Its operand
// groups are already canonical, so one level of splicing reaches the flat form.
func flattenBooleanChain(inside string) string {
	if topLevelWord(inside, "BETWEEN") || topLevelWord(inside, ",") {
		return inside
	}
	for _, keyword := range []string{"AND", "OR"} {
		other := "OR"
		if keyword == "OR" {
			other = "AND"
		}
		operands := topLevelOperandSpans(inside, keyword)
		if len(operands) < 2 || topLevelWord(inside, other) {
			continue
		}
		var builder strings.Builder
		last := 0
		for _, span := range operands {
			if !sameChainGroup(inside[span[0]:span[1]], keyword, other) {
				continue
			}
			builder.WriteString(inside[last:span[0]])
			builder.WriteString(inside[span[0]+1 : span[1]-1])
			last = span[1]
		}
		builder.WriteString(inside[last:])
		return builder.String()
	}
	return inside
}

// sameChainGroup reports whether operand is one parenthesized group whose top
// level is a chain of keyword alone.
func sameChainGroup(operand, keyword, other string) bool {
	if len(operand) < 2 || operand[0] != '(' || operand[len(operand)-1] != ')' {
		return false
	}
	inside := operand[1 : len(operand)-1]
	if !balancedGroup(inside) || topLevelWord(inside, "BETWEEN") || topLevelWord(inside, ",") {
		return false
	}
	return topLevelWord(inside, keyword) && !topLevelWord(inside, other)
}

// balancedGroup reports whether text never closes more parentheses than it
// opened, so (text) is one group rather than (a) AND (b).
func balancedGroup(text string) bool {
	depth := 0
	singleQuoted, doubleQuoted := false, false
	for index := 0; index < len(text); index++ {
		switch character := text[index]; {
		case character == '\'' && !doubleQuoted:
			singleQuoted = !singleQuoted
		case character == '"' && !singleQuoted:
			doubleQuoted = !doubleQuoted
		case singleQuoted || doubleQuoted:
		case character == '(':
			depth++
		case character == ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !singleQuoted && !doubleQuoted
}

func topLevelWord(text, word string) bool {
	return len(topLevelOperandSpans(text, word)) > 1
}

// topLevelOperandSpans returns the [start, end) spans of the operands that
// word separates outside quotes and parentheses, without surrounding
// whitespace. A word made of letters must stand alone; deparsed keywords are
// upper case, and an identifier spelled like one is quoted.
func topLevelOperandSpans(text, word string) [][2]int {
	var spans [][2]int
	depth, start := 0, 0
	singleQuoted, doubleQuoted := false, false
	for index := 0; index < len(text); index++ {
		switch character := text[index]; {
		case character == '\'' && !doubleQuoted:
			singleQuoted = !singleQuoted
		case character == '"' && !singleQuoted:
			doubleQuoted = !doubleQuoted
		case singleQuoted || doubleQuoted:
		case character == '(':
			depth++
		case character == ')':
			depth--
		case depth == 0 && strings.HasPrefix(text[index:], word) && standsAlone(text, index, len(word)):
			spans = append(spans, trimmedSpan(text, start, index))
			index += len(word) - 1
			start = index + 1
		}
	}
	return append(spans, trimmedSpan(text, start, len(text)))
}

func trimmedSpan(text string, start, end int) [2]int {
	for start < end && isSpace(text[start]) {
		start++
	}
	for end > start && isSpace(text[end-1]) {
		end--
	}
	return [2]int{start, end}
}

func isSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

func standsAlone(text string, index, length int) bool {
	if !isWordByte(text[index]) {
		return true
	}
	return (index == 0 || !isWordByte(text[index-1])) && (index+length == len(text) || !isWordByte(text[index+length]))
}

func isWordByte(value byte) bool {
	return value == '_' || value == '$' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
