package contractcheck

import (
	"github.com/jokull/onwardpg/pgschema"
)

// withoutObserverIgnoreReceipts removes only evidence for the separately bound
// live-only policy from the comparison fingerprint. Scratch checkpoints cannot
// observe those exclusions. The objects, edges, unsupported state, and ordinary
// planning ignore receipts retain their exact fingerprint semantics.
func withoutObserverIgnoreReceipts(snapshot *pgschema.Snapshot, selectors []string) (*pgschema.Snapshot, error) {
	if len(selectors) == 0 {
		return snapshot, nil
	}
	result := pgschema.New()
	for _, object := range snapshot.Objects() {
		if err := result.Add(object); err != nil {
			return nil, err
		}
	}
	for _, id := range snapshot.IDs() {
		for _, dependency := range snapshot.Dependencies(id) {
			if err := result.AddDependency(id, dependency); err != nil {
				return nil, err
			}
		}
	}
	for _, selector := range snapshot.Unsupported() {
		if err := result.AddUnsupported(selector); err != nil {
			return nil, err
		}
	}
	for _, actual := range snapshot.Ignored() {
		observerOnly := false
		for _, selector := range selectors {
			if selector == actual {
				observerOnly = true
				break
			}
		}
		if !observerOnly {
			if err := result.AddIgnored(actual); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
