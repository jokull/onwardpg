package source

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/pgschema"
)

// liveIgnorePrefixes are the only unsupported-state families a project may
// acknowledge for live catalogs. Each is state a managed PostgreSQL provider
// owns in its own cluster: an extension or a schema owned by the provider's
// administrative role, and a grant on a server parameter to that role.
// Ownership selectors end in "=ROLE", so a change of owner is a new selector
// and blocks again.
var liveIgnorePrefixes = []string{"ownership:extension:", "ownership:schema:", "parameter_acl:"}

// ValidateLiveIgnoreSelectors checks the syntax of a target's live_ignore
// list. A selector is the exact text of an unsupported-state selector as the
// catalog queries build it, with every identifier written the way PostgreSQL's
// quote_ident writes it:
//
//	ownership:extension:IDENT=IDENT    ownership:extension:earthdistance=pscale_admin
//	ownership:schema:IDENT=IDENT       ownership:schema:pscale_extensions=pscale_admin
//	parameter_acl:IDENT                parameter_acl:session_replication_role
//
// IDENT is either an unquoted run of [a-z_][a-z0-9_]* or a double-quoted
// non-empty string in which a quote is doubled ("extwlist.extensions"). Both
// halves of an ownership selector are required, the separator is the single
// "=" outside quotes, and nothing may follow. Wildcards are rejected, and no
// selector is required to match anything: the state exists only in the live
// cluster, never in exported DDL or a development catalog. Because an
// unmatched selector acknowledges nothing and is not reported, a selector the
// generator could never emit (a typo, an unquoted capital letter) is rejected.
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
	return fmt.Errorf("invalid live_ignore selector %q; expected ownership:extension:NAME=ROLE, ownership:schema:NAME=ROLE, or parameter_acl:NAME", selector)
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

// ProjectLiveIgnored removes acknowledged unsupported-state selectors from a
// live catalog snapshot and returns the selectors it removed. It never removes
// or alters a typed object: acknowledged state is not modeled, so every
// difference in the modeled graph stays visible, and no ignore receipt is
// added. The markers are part of a snapshot's canonical form, so the returned
// snapshot has a different fingerprint than the input when anything was
// removed; callers compare and report the projected one, so a catalog that
// differs from the expected graph only by acknowledged state compares equal,
// and report the input's fingerprint separately. Selectors absent from the
// snapshot are ignored without error because one project configuration serves
// clusters that differ in what their provider installs.
func ProjectLiveIgnored(snapshot *pgschema.Snapshot, selectors []string) (*pgschema.Snapshot, []string, error) {
	if len(selectors) == 0 {
		return snapshot, nil, nil
	}
	if err := ValidateLiveIgnoreSelectors(selectors); err != nil {
		return nil, nil, err
	}
	acknowledged := make(map[string]bool, len(selectors))
	for _, selector := range selectors {
		acknowledged[selector] = true
	}
	var removed []string
	projected, err := snapshot.Project(nil, func(selector string) bool {
		if acknowledged[selector] {
			removed = append(removed, selector)
			return false
		}
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(removed)
	return projected, removed, nil
}
