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
// list. A selector is the exact text of an unsupported-state selector, for
// example ownership:extension:earthdistance=pscale_admin,
// ownership:schema:pscale_extensions=pscale_admin, or
// parameter_acl:session_replication_role. Wildcards are rejected, and no
// selector is required to match anything: the state exists only in the live
// cluster, never in exported DDL or a development catalog.
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
		if rest == "" || strings.TrimSpace(selector) != selector || strings.Contains(selector, "*") ||
			prefix != "parameter_acl:" && !strings.Contains(rest, "=") {
			break
		}
		return nil
	}
	return fmt.Errorf("invalid live_ignore selector %q; expected ownership:extension:NAME=ROLE, ownership:schema:NAME=ROLE, or parameter_acl:NAME", selector)
}

// ProjectLiveIgnored removes acknowledged unsupported-state selectors from a
// live catalog snapshot and returns the selectors it removed. It never removes
// or alters a typed object: acknowledged state is not modeled, so every
// difference in the modeled graph stays visible, and no ignore receipt is
// added, so fingerprints are unaffected. Selectors absent from the snapshot are
// ignored without error because one project configuration serves clusters that
// differ in what their provider installs.
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
