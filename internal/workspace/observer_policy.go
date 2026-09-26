package workspace

import (
	"context"
	"fmt"
	"os"

	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// ObserverIgnoreSelectors receipts configured exclusions that occur only in the
// development catalog. Replay and desired snapshots still define the separate
// clone-verification boundary. No live policy is inferred when no development
// database is configured, and failed inspection never produces a receipt.
func ObserverIgnoreSelectors(ctx context.Context, target Target, planning ...*pgschema.Snapshot) ([]string, error) {
	if len(target.Ignore) == 0 || target.DevDatabaseEnv == "" {
		return nil, nil
	}
	url := os.Getenv(target.DevDatabaseEnv)
	if url == "" {
		return nil, fmt.Errorf("environment variable %s is required to validate observer ignore selectors", target.DevDatabaseEnv)
	}
	live, err := source.LoadGraphForComparison(ctx, source.Parse(url), "", target.Ignore)
	if err != nil {
		return nil, fmt.Errorf("inspect development observer policy: %w", err)
	}
	snapshots := append(append([]*pgschema.Snapshot(nil), planning...), live)
	if err := source.ValidateIgnoreSelectors(target.Ignore, snapshots...); err != nil {
		return nil, fmt.Errorf("validate observer ignore selectors: %w", err)
	}
	// Bind only exact objects observed in this catalog, even when configuration
	// used kind:*. A journal boundary must not authorize future application
	// objects that were absent when this bundle was planned.
	planned := make(map[string]bool)
	for _, snapshot := range planning {
		for _, actual := range snapshot.Ignored() {
			planned[actual] = true
		}
	}
	var observer []string
	for _, actual := range live.Ignored() {
		if !planned[actual] {
			observer = append(observer, actual)
		}
	}
	return observer, nil
}
