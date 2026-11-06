package source

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/pgschema"
)

// liveIgnorePrefixes are the unsupported-state families a project may
// acknowledge for live catalogs. Each is state a managed PostgreSQL provider
// owns in its own cluster: an extension or a schema owned by the provider's
// administrative role, and a grant on a server parameter to that role.
// Ownership selectors end in "=ROLE", so a change of owner is a new selector
// and blocks again.
var liveIgnorePrefixes = []string{"ownership:extension:", "ownership:schema:", "parameter_acl:"}

// ValidateLiveIgnoreSelectors checks the syntax of a target's ignore.live
// list. Unsupported-state selectors use the exact text emitted by the catalog
// queries, with identifiers written as PostgreSQL's quote_ident writes them:
//
//	ownership:extension:IDENT=IDENT    ownership:extension:earthdistance=pscale_admin
//	ownership:schema:IDENT=IDENT       ownership:schema:pscale_extensions=pscale_admin
//	parameter_acl:IDENT                parameter_acl:session_replication_role
//
// IDENT is either an unquoted run of [a-z_][a-z0-9_]* or a double-quoted
// non-empty string in which a quote is doubled ("extwlist.extensions"). Both
// halves of an ownership selector are required, the separator is the single
// "=" outside quotes, and nothing may follow. Provider-state wildcards are
// rejected. Object selectors allow * as a whole name segment. No
// selector is required to match anything: the state or object may exist only
// in one live cluster, never in exported DDL or a development catalog.
// Unsupported-state forms are checked more strictly because their generator
// can never emit a malformed selector.
func ValidateLiveIgnoreSelectors(selectors []string) error {
	for _, selector := range selectors {
		if err := validateLiveIgnoreSelector(selector); err != nil {
			return err
		}
	}
	return nil
}

func validateLiveIgnoreSelector(selector string) error {
	for _, prefix := range liveIgnorePrefixes {
		rest, found := strings.CutPrefix(selector, prefix)
		if !found {
			continue
		}
		if err := validateLiveIgnoreOperands(rest, prefix != "parameter_acl:"); err != nil {
			return fmt.Errorf("invalid live_ignore selector %q: %v; expected ownership:extension:NAME=ROLE, ownership:schema:NAME=ROLE, or parameter_acl:NAME with quote_ident-style identifiers", selector, err)
		}
		return nil
	}
	if strings.TrimSpace(selector) != selector {
		return fmt.Errorf("invalid live_ignore selector %q: surrounding whitespace is not allowed", selector)
	}
	kind, value, found := strings.Cut(selector, ":")
	if !found || kind == "" || value == "" {
		return fmt.Errorf("invalid live_ignore selector %q: expected kind:name", selector)
	}
	for _, segment := range selectorSegments(value) {
		if strings.Contains(segment, "*") && segment != "*" {
			return fmt.Errorf("invalid live_ignore selector %q: * must be a whole name segment", selector)
		}
	}
	if kind != "schema" && kind != "extension" && kind != "table_owner" && !schemaScopedKinds[kind] {
		return fmt.Errorf("invalid live_ignore selector %q: unsupported object kind %q", selector, kind)
	}
	return nil
}

func validateLiveIgnoreOperands(rest string, owner bool) error {
	name, rest, err := cutQuotedIdentifier(rest)
	if err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if !owner {
		if rest != "" {
			return fmt.Errorf("unexpected text %q after the name", rest)
		}
		return nil
	}
	rest, found := strings.CutPrefix(rest, "=")
	if !found {
		return fmt.Errorf("expected \"=\" after the name %s", name)
	}
	role, rest, err := cutQuotedIdentifier(rest)
	if err != nil {
		return fmt.Errorf("role: %w", err)
	}
	if rest != "" {
		return fmt.Errorf("unexpected text %q after the role %s", rest, role)
	}
	return nil
}

// cutQuotedIdentifier splits one identifier, as quote_ident writes it, off the
// front of text and returns it with the remainder.
func cutQuotedIdentifier(text string) (identifier, rest string, err error) {
	if text == "" {
		return "", "", fmt.Errorf("missing identifier")
	}
	if text[0] != '"' {
		end := 0
		for end < len(text) && (text[end] == '_' || text[end] >= 'a' && text[end] <= 'z' || end > 0 && text[end] >= '0' && text[end] <= '9') {
			end++
		}
		if end == 0 {
			return "", "", fmt.Errorf("identifier %q must be double-quoted unless it is lower-case letters, digits, and underscores that do not start with a digit", firstRune(text))
		}
		return text[:end], text[end:], nil
	}
	for index := 1; index < len(text); index++ {
		if text[index] != '"' {
			continue
		}
		if index+1 < len(text) && text[index+1] == '"' {
			index++
			continue
		}
		if index == 1 {
			return "", "", fmt.Errorf("quoted identifier is empty")
		}
		return text[:index+1], text[index+1:], nil
	}
	return "", "", fmt.Errorf("quoted identifier is not terminated")
}

func firstRune(text string) string {
	for _, r := range text {
		return string(r)
	}
	return ""
}

// ProjectLiveIgnored removes acknowledged unsupported state and modeled objects
// from a live snapshot without adding planner ignore receipts. The returned
// snapshot has a different fingerprint than the input when anything was
// removed; callers compare and report the projected one, so a catalog that
// differs from the expected graph only by acknowledged state compares equal,
// and report the input's fingerprint separately. Selectors absent from the
// snapshot are ignored without error because one project configuration serves
// clusters that differ in what their provider installs; they are returned as
// unmatched so that they stay visible.
func ProjectLiveIgnored(snapshot *pgschema.Snapshot, selectors []string) (projected *pgschema.Snapshot, removed, unmatched []string, err error) {
	if len(selectors) == 0 {
		return snapshot, nil, nil, nil
	}
	if err := ValidateLiveIgnoreSelectors(selectors); err != nil {
		return nil, nil, nil, err
	}
	acknowledged := make(map[string]bool, len(selectors))
	var objects []string
	for _, selector := range selectors {
		if strings.HasPrefix(selector, "ownership:") || strings.HasPrefix(selector, "parameter_acl:") {
			acknowledged[selector] = true
		} else {
			objects = append(objects, selector)
		}
	}
	excluded := make(map[pgschema.ID]bool)
	excludedSelectors := make(map[string]bool)
	for _, id := range snapshot.IDs() {
		actual := objectSelector(id)
		for _, selector := range objects {
			if id.Kind == pgschema.KindTable && liveSelectorMatches(selector, "table_owner:"+strings.TrimPrefix(actual, "table:")) {
				if table, ok := snapshot.Object(id); ok && table.(pgschema.Table).Owner != "" {
					removed = append(removed, selector)
				}
				continue
			}
			if liveSelectorMatches(selector, actual) {
				removed = append(removed, selector)
				excluded[id] = true
				excludedSelectors[actual] = true
			}
		}
	}
	// Remove graph dependents along with their excluded parent, as catalog
	// inspection does for child objects of an ignored table or schema.
	for changed := true; changed; {
		changed = false
		for _, id := range snapshot.IDs() {
			if excluded[id] {
				continue
			}
			for _, dependency := range snapshot.Dependencies(id) {
				if excluded[dependency] {
					excluded[id] = true
					excludedSelectors[objectSelector(id)] = true
					changed = true
					break
				}
			}
		}
	}
	projected, err = snapshot.Project(func(object pgschema.Object) (pgschema.Object, bool) {
		if excluded[object.ObjectID()] {
			return object, false
		}
		if table, ok := object.(pgschema.Table); ok && table.Owner != "" {
			for _, selector := range objects {
				if liveSelectorMatches(selector, "table_owner:"+table.Schema+"."+table.Name) {
					table.Owner = ""
					return table, true
				}
			}
		}
		return object, true
	}, func(selector string) bool {
		if acknowledged[selector] {
			removed = append(removed, selector)
			return false
		}
		for _, owner := range markerOwners(selector, nil) {
			if excludedSelectors[owner] {
				return false
			}
		}
		return true
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Strings(removed)
	removed = uniqueSorted(removed)
	return projected, removed, UnmatchedLiveIgnore(selectors, removed), nil
}

func objectSelector(id pgschema.ID) string {
	kind := string(id.Kind)
	if id.Kind == pgschema.KindRange {
		kind = "range_type"
	}
	if id.Kind == pgschema.KindExtension {
		return "extension:" + id.Name
	}
	name := id.Name
	if id.Schema != "" {
		name = id.Schema + "." + name
	}
	if id.Part != "" {
		name += "." + strings.ReplaceAll(id.Part, ":", ".")
	}
	if id.Signature != "" {
		name += "(" + id.Signature + ")"
	}
	return kind + ":" + name
}

func liveSelectorMatches(requested, actual string) bool {
	if !strings.Contains(requested, "*") {
		return SelectorMatches(requested, actual)
	}
	kind, pattern, _ := strings.Cut(requested, ":")
	actualKind, name, _ := strings.Cut(actual, ":")
	if kind != actualKind {
		return false
	}
	parts, names := selectorSegments(pattern), selectorSegments(name)
	if len(parts) != len(names) {
		return false
	}
	for index, part := range parts {
		if part != "*" && part != names[index] {
			return false
		}
	}
	return true
}

// selectorSegments keeps dots inside quoted identifiers and routine signatures.
func selectorSegments(name string) []string {
	var parts []string
	start, depth, quoted := 0, 0, false
	for index := 0; index < len(name); index++ {
		switch name[index] {
		case '"':
			if quoted && index+1 < len(name) && name[index+1] == '"' {
				index++
			} else {
				quoted = !quoted
			}
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted && depth > 0 {
				depth--
			}
		case '.':
			if !quoted && depth == 0 {
				parts = append(parts, name[start:index])
				start = index + 1
			}
		}
	}
	return append(parts, name[start:])
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

// UnmatchedLiveIgnore returns the configured selectors, sorted and without
// duplicates, that are not in matched. An entry that acknowledged nothing is
// information, not an error: one configuration serves clusters that differ in
// what their provider installs. It is also how a typo, or a keyword that
// quote_ident would have quoted, becomes visible.
func UnmatchedLiveIgnore(selectors, matched []string) []string {
	seen := make(map[string]bool, len(selectors)+len(matched))
	for _, selector := range matched {
		seen[selector] = true
	}
	var unmatched []string
	for _, selector := range selectors {
		if !seen[selector] {
			seen[selector] = true
			unmatched = append(unmatched, selector)
		}
	}
	sort.Strings(unmatched)
	return unmatched
}
