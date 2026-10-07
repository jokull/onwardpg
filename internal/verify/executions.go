package verify

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// Executions keeps the results of disposable executions for one command.
//
// A verification compares two separate executions. One command can ask for
// the same pair several times: a plan verifies the expand checkpoint and then
// the complete bundle, and the two checks run the same SQL when the bundle has
// no work after expand. An execution is identified by the exact inputs that
// decide what runs, and by an instance number. Two instances of one identity
// are two separate databases, so a check that needs separate executions gets
// them. A third execution of an identity that already ran twice adds no
// evidence and is not started.
//
// Results are held in memory for the life of the value. Nothing is stored on
// the scratch server or on disk, so there is no state that can go stale.
type Executions struct {
	mu      sync.Mutex
	results map[executionSlot]*execution
	started int
}

type executionSlot struct {
	identity string
	instance int
}

type execution struct {
	done     chan struct{}
	snapshot *pgschema.Snapshot
	batches  int
	failure  *Failure
	err      error
}

func NewExecutions() *Executions {
	return &Executions{results: make(map[executionSlot]*execution)}
}

// Started reports how many executions this value has started. Each one is a
// replay in its own scratch database.
func (e *Executions) Started() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started
}

// start returns the execution for slot. It starts the execution when the slot
// is empty. A result that ended with an error is not kept: an error reports
// the scratch server, not the SQL, and the next caller must try again.
func (e *Executions) start(slot executionSlot, run func() (*pgschema.Snapshot, int, *Failure, error)) *execution {
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, exists := e.results[slot]; exists {
		return existing
	}
	result := &execution{done: make(chan struct{})}
	e.results[slot] = result
	e.started++
	go func() {
		defer close(result.done)
		result.snapshot, result.batches, result.failure, result.err = run()
		if result.err != nil {
			e.mu.Lock()
			if e.results[slot] == result {
				delete(e.results, slot)
			}
			e.mu.Unlock()
		}
	}()
	return result
}

// executionIdentity is a digest of every input that executeDisposable reads.
// Two calls with one identity run the same SQL, in the same order and the
// same transaction boundaries, with the same checks, against the same server,
// and read the catalog with the same ignore selectors.
//
// The selected phase is part of the identity only when it changes what runs.
// A bundle with no work after expand runs the same SQL for "expand" and for
// "contract".
func executionIdentity(adminURL string, chain history.Chain, targetBundle, throughPhase string, ignores []string) (string, error) {
	hash := sha256.New()
	frame := func(value []byte) {
		var length [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:n])
		_, _ = hash.Write(value)
	}
	frame([]byte("onwardpg-execution-v1"))
	frame([]byte(adminURL))
	frame([]byte(targetBundle))
	limit := throughPhase
	for _, entry := range chain.Entries {
		manifest := entry.Artifact.Manifest
		if manifest.BundleID != targetBundle {
			continue
		}
		later, err := workAfterExpand(entry)
		if err != nil {
			return "", err
		}
		if !later {
			limit = protocol.PhaseContract
		}
	}
	frame([]byte(limit))
	frame([]byte(fmt.Sprint(len(ignores))))
	for _, selector := range ignores {
		frame([]byte(selector))
	}
	frame([]byte(fmt.Sprint(len(chain.Entries))))
	for _, entry := range chain.Entries {
		manifest := entry.Artifact.Manifest
		frame([]byte(entry.Directory))
		frame([]byte(manifest.BundleID))
		frame([]byte(manifest.PhaseSource))
		frame([]byte(manifest.VerificationDigest))
		frame(entry.Artifact.Files["plan.json"])
		frame(entry.Artifact.Files["verify.sql"])
		names := make([]string, 0, len(manifest.Phases))
		for name := range manifest.Phases {
			names = append(names, name)
		}
		sort.Strings(names)
		frame([]byte(fmt.Sprint(len(names))))
		for _, name := range names {
			receipt := manifest.Phases[name]
			frame([]byte(name))
			frame([]byte(receipt.Path))
			switch {
			case receipt.Transactional == nil:
				frame([]byte("unset"))
			case *receipt.Transactional:
				frame([]byte("transactional"))
			default:
				frame([]byte("non-transactional"))
			}
			frame(entry.Artifact.Files[receipt.Path])
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// workAfterExpand reports whether executeDisposable does anything for entry
// with the limit "contract" that it does not do with the limit "expand": a
// contract phase, or the assertions of an edited bundle. It answers true when
// it cannot tell.
func workAfterExpand(entry history.Entry) (bool, error) {
	manifest := entry.Artifact.Manifest
	if manifest.PhaseSource == "edited" {
		_, contract := manifest.Phases[protocol.PhaseContract]
		return contract || manifest.VerificationDigest != "", nil
	}
	var plan protocol.Result
	if err := json.Unmarshal(entry.Artifact.Files["plan.json"], &plan); err != nil {
		return false, fmt.Errorf("decode bundle %s plan: %w", entry.Directory, err)
	}
	for _, batch := range plan.Batches {
		if batch.Phase != protocol.PhaseExpand {
			return true, nil
		}
	}
	return false, nil
}

// pair runs the observed execution and the separate full execution at the
// same time and waits for both. It always waits: each execution owns a
// scratch database that it must drop before the command ends.
func (e *Executions) pair(ctx context.Context, adminURL string, chain history.Chain, targetBundle, throughPhase string, ignores []string) (observed, desired *execution, err error) {
	observedIdentity, err := executionIdentity(adminURL, chain, targetBundle, throughPhase, ignores)
	if err != nil {
		return nil, nil, err
	}
	desiredIdentity, err := executionIdentity(adminURL, chain, targetBundle, protocol.PhaseContract, ignores)
	if err != nil {
		return nil, nil, err
	}
	desiredSlot := executionSlot{identity: desiredIdentity}
	if desiredIdentity == observedIdentity {
		desiredSlot.instance = 1
	}
	observed = e.start(executionSlot{identity: observedIdentity}, func() (*pgschema.Snapshot, int, *Failure, error) {
		return executeDisposable(ctx, adminURL, chain, targetBundle, throughPhase, ignores)
	})
	desired = e.start(desiredSlot, func() (*pgschema.Snapshot, int, *Failure, error) {
		return executeDisposable(ctx, adminURL, chain, targetBundle, protocol.PhaseContract, ignores)
	})
	<-observed.done
	<-desired.done
	return observed, desired, nil
}
