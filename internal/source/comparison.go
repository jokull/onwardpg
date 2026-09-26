package source

import (
	"fmt"
	"sort"

	"github.com/jokull/onwardpg/pgschema"
)

// AlignIgnoreReceipts compares already inspected catalogs under the union of
// their observed ignore policy. An ignored object can exist on only one side,
// such as a framework journal omitted from exported DDL. Only receipt metadata
// is aligned: objects, dependencies, and unsupported markers are preserved.
// Inputs are never modified, so stored snapshot fingerprints remain unchanged.
// Validate explicit selectors before calling this; this does not validate or
// apply selectors and must not manufacture evidence for an unused selector.
func AlignIgnoreReceipts(left, right *pgschema.Snapshot) (*pgschema.Snapshot, *pgschema.Snapshot, error) {
	if left == nil || right == nil {
		return nil, nil, fmt.Errorf("ignore receipt comparison requires both snapshots")
	}
	seen := make(map[string]bool)
	for _, snapshot := range []*pgschema.Snapshot{left, right} {
		for _, selector := range snapshot.Ignored() {
			seen[selector] = true
		}
	}
	selectors := make([]string, 0, len(seen))
	for selector := range seen {
		selectors = append(selectors, selector)
	}
	sort.Strings(selectors)
	align := func(snapshot *pgschema.Snapshot) (*pgschema.Snapshot, error) {
		if len(snapshot.Ignored()) == len(selectors) {
			return snapshot, nil
		}
		projected, err := snapshot.Project(nil, nil)
		if err != nil {
			return nil, err
		}
		for _, selector := range selectors {
			if err := projected.AddIgnored(selector); err != nil {
				return nil, err
			}
		}
		return projected, nil
	}
	left, err := align(left)
	if err != nil {
		return nil, nil, err
	}
	right, err = align(right)
	return left, right, err
}
