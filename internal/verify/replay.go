package verify

import (
	"context"
	"fmt"

	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// ReplayHistory executes every bundle of chain, through contract, in one
// disposable database and returns the catalog that results.
//
// It is the only history replay. Verification uses the same execution, so a
// command that reads replayed history (drift check, and plan or draft for its
// base) runs each batch in the mode that the bundle declares: a
// non-transactional batch is never put in a transaction, an edited phase is
// split only at its batch directives, and the checks that verification runs
// are run here too. One connection executes the complete chain, so session
// state that a bundle sets (for example search_path) stays set for the bundles
// after it.
//
// A chain without entries gives the catalog of an empty database.
func ReplayHistory(ctx context.Context, adminURL string, chain history.Chain, ignores []string) (*pgschema.Snapshot, error) {
	if adminURL == "" {
		return nil, fmt.Errorf("disposable database admin URL is required")
	}
	if err := requirePostgresMajor(ctx, adminURL, chain); err != nil {
		return nil, err
	}
	// No bundle is selected: each entry runs through contract.
	snapshot, _, failure, err := executeDisposable(ctx, adminURL, chain, "", protocol.PhaseContract, ignores)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, replayFailure(failure)
	}
	return snapshot, nil
}

// requirePostgresMajor rejects a scratch server whose major version is not the
// one that the history was receipted on. The same SQL can give a different
// catalog on another major.
func requirePostgresMajor(ctx context.Context, adminURL string, chain history.Chain) error {
	postgresMajor, err := source.PostgresMajor(ctx, adminURL)
	if err != nil {
		return err
	}
	for _, entry := range chain.Entries {
		recorded := entry.Artifact.Manifest.DesiredSource.PostgresMajor
		if recorded != 0 && recorded != postgresMajor {
			return fmt.Errorf("history bundle %s targets PostgreSQL %d but the scratch server is PostgreSQL %d", entry.Directory, recorded, postgresMajor)
		}
	}
	return nil
}

// replayFailure reports SQL of accepted history that failed in the disposable
// database. It names the bundle and the batch or check, which a caller cannot
// get from the PostgreSQL message.
func replayFailure(failure *Failure) error {
	if failure.CheckID != "" {
		return fmt.Errorf("history bundle %s check %s failed (%s): %s", failure.BundleID, failure.CheckID, failure.Code, failure.Message)
	}
	return fmt.Errorf("history bundle %s %s batch %s (%s) failed: %s", failure.BundleID, failure.Phase, failure.BatchID, failure.ExecutionMode, failure.Message)
}
