package source

import (
	"strings"
	"unicode/utf8"
)

// postgresNameDataLen is PostgreSQL's NAMEDATALEN. Identifiers, and therefore
// generated constraint names, are at most NAMEDATALEN-1 bytes.
const postgresNameDataLen = 64

const notNullConstraintLabel = "not_null"

// isGeneratedNotNullConstraintName reports whether constraint is a name that
// PostgreSQL 18 itself would generate for a NOT NULL constraint on
// relation.column. Such a name carries no information beyond the table and
// column it sits on, so the typed column (Column.NotNull) represents it
// completely. Any other name, including the default name of a different table
// or column left behind by a rename, is custom as far as the graph can tell.
//
// PostgreSQL builds the name in heap.c with
//
//	ChooseConstraintName(relname, attname, "not_null", namespace, ...)
//
// which calls makeObjectName(relname, attname, label) with label "not_null",
// then "not_null1", "not_null2", ... while the candidate collides with a
// constraint name in the same schema. The collision counter therefore depends
// on creation order and on other tables, which is why the accepted names are
// recomputed from the real table and column rather than compared as text.
// Any positive counter is accepted: the colliding constraint may since have
// been dropped.
//
// The recomputation assumes a UTF-8 server encoding, where PostgreSQL clips
// names on character boundaries (pg_mbcliplen). A name that was clipped under
// another multibyte encoding can fail to match and is then reported as a
// blocker, which is the conservative outcome.
func isGeneratedNotNullConstraintName(relation, column, constraint string) bool {
	digits := len(constraint)
	for digits > 0 && constraint[digits-1] >= '0' && constraint[digits-1] <= '9' {
		digits--
	}
	counter := constraint[digits:]
	// The counter is formatted with %d starting at 1: no sign, no zero padding.
	if strings.HasPrefix(counter, "0") {
		return false
	}
	if !strings.HasSuffix(constraint[:digits], "_"+notNullConstraintLabel) {
		return false
	}
	want, ok := postgresMakeObjectName(relation, column, notNullConstraintLabel+counter)
	return ok && want == constraint
}

// postgresMakeObjectName mirrors makeObjectName in
// src/backend/commands/indexcmds.c for name1_name2_label. It reports false
// where PostgreSQL would assert, which cannot happen for a real NOT NULL name.
func postgresMakeObjectName(name1, name2, label string) (string, bool) {
	overhead := 1 + len(label) + 1
	availchars := postgresNameDataLen - 1 - overhead
	if availchars <= 0 {
		return "", false
	}
	name1chars, name2chars := len(name1), len(name2)
	// Shorten the longer name, one byte at a time, until the result fits.
	for name1chars+name2chars > availchars {
		if name1chars > name2chars {
			name1chars--
		} else {
			name2chars--
		}
	}
	name1 = clipToCharacterBoundary(name1, name1chars)
	name2 = clipToCharacterBoundary(name2, name2chars)
	return name1 + "_" + name2 + "_" + label, true
}

// clipToCharacterBoundary returns the longest prefix of value that is at most
// limit bytes and does not split a UTF-8 character, like pg_mbcliplen.
func clipToCharacterBoundary(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if end+size > limit {
			break
		}
		end += size
	}
	return value[:end]
}
