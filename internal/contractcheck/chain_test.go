package contractcheck

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
)

func fingerprintOf(letter string) string { return "sha256:" + strings.Repeat(letter, 64) }

// chainOfThree returns three linked history entries whose fingerprints are
// a (baseline of 1) -> b (desired of 1) -> c -> d, with post-expand
// checkpoints 1, 2 and 3.
func chainOfThree(t *testing.T) []bundle.Artifact {
	t.Helper()
	states := []struct{ id, baseline, expand, desired string }{
		{"first", "a", "1", "b"}, {"second", "b", "2", "c"}, {"third", "c", "3", "d"},
	}
	var entries []bundle.Artifact
	parent := bundle.HistoryRootDigest()
	for _, state := range states {
		result := chainResult(fingerprintOf(state.baseline), fingerprintOf(state.desired))
		metadata := bundle.Metadata{
			BundleID: state.id, PlanID: "plan_0123456789abcdef0123456789abcde" + string(state.id[0]), Generation: 1, Target: "primary", Purpose: "feature",
			BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "baseline", Fingerprint: result.CurrentFingerprint},
			DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "desired", Fingerprint: result.DesiredFingerprint},
			Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: parent,
		}
		artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
		if err != nil {
			t.Fatal(err)
		}
		artifact, err = bundle.WithExpandCheckpoint(artifact, fingerprintOf(state.expand))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, artifact)
		parent = artifact.Manifest.History.EntryDigest
	}
	return entries
}

func TestNonHeadBundleIsSelectedOnlyFromItsOwnChain(t *testing.T) {
	entries := chainOfThree(t)
	head := entries[2].Manifest.History.EntryDigest
	selection, err := selectChainEntry(Input{Artifact: entries[1], ExpectedHead: head, Chain: entries})
	if err != nil || selection.index != 1 || selection.head() {
		t.Fatalf("selection = %#v, err = %v", selection, err)
	}
	if _, err := selectChainEntry(Input{Artifact: entries[1], ExpectedHead: head}); err == nil {
		t.Fatal("without the chain, only the head may be checked")
	}
	if _, err := selectChainEntry(Input{Artifact: entries[1], ExpectedHead: entries[1].Manifest.History.EntryDigest, Chain: entries}); err == nil {
		t.Fatal("a chain that does not end at the expected head must be refused")
	}
	if _, err := selectChainEntry(Input{Artifact: entries[1], ExpectedHead: head, Chain: []bundle.Artifact{entries[0], entries[2]}}); err == nil {
		t.Fatal("a chain with a missing link must be refused")
	}
}

func TestCatalogPositionNamesOutOfOrderChainState(t *testing.T) {
	entries := chainOfThree(t)
	selection := chainSelection{index: 1, entries: entries}
	checkpoint, err := bundle.ReadCatalogCheckpoint(entries[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ observed, code string }{
		{"a", "predecessor_contract_pending"},
		{"1", "predecessor_contract_pending"},
		{"b", "expand_not_applied"},
		{"c", "contract_already_applied"},
		{"3", "successor_expand_applied"},
		{"d", "successor_expand_applied"},
		{"9", "catalog_drift"},
	} {
		if code, _ := selection.classifyCatalogPosition(fingerprintOf(test.observed), checkpoint); code != test.code {
			t.Errorf("observed %s: code = %s, want %s", test.observed, code, test.code)
		}
	}
}

func TestEvidenceArrayCoversAChainPrefix(t *testing.T) {
	entries := chainOfThree(t)
	var array []Evidence
	for _, entry := range entries[:2] {
		array = append(array, Evidence{
			Target: "primary", Environment: "production", PlanID: entry.Manifest.PlanID,
			BundleEntryDigest: entry.Manifest.History.EntryDigest, DesiredFingerprint: entry.Manifest.DesiredSource.Fingerprint,
			Generation: 1, Release: "cutover", ObservedAt: "2026-10-17T12:00:00Z", ExpiresAt: "2026-10-17T14:00:00Z",
			Cohorts: []Cohort{{Category: "web", Name: "write barrier", Status: "read_only", SourceKind: "manual", Source: "runbook/write-barrier"}},
		})
	}
	body, err := json.Marshal(array)
	if err != nil {
		t.Fatal(err)
	}
	selected, selectedBytes, err := selectEvidence(body, entries[1].Manifest)
	if err != nil || selected.PlanID != entries[1].Manifest.PlanID || len(selectedBytes) == 0 {
		t.Fatalf("selected = %#v, err = %v", selected, err)
	}
	if receipt := evidenceReceipt(selected, selectedBytes); receipt.Release != "cutover" || len(receipt.Cohorts) != 1 || !strings.HasPrefix(receipt.Digest, "sha256:") {
		t.Fatalf("receipt = %#v", receipt)
	}
	if _, _, err := selectEvidence(body, entries[2].Manifest); !errors.Is(err, errNoEvidenceBinding) {
		t.Fatalf("a bundle outside the attested prefix must not be covered: %v", err)
	}
	duplicate, err := json.Marshal(append(array, array[1]))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectEvidence(duplicate, entries[1].Manifest); err == nil || errors.Is(err, errNoEvidenceBinding) {
		t.Fatalf("two objects for one bundle must be refused: %v", err)
	}
	if _, _, err := selectEvidence([]byte("[]"), entries[1].Manifest); err == nil {
		t.Fatal("an empty evidence array must be refused")
	}
}

func chainResult(current, desired string) protocol.Result {
	statement := protocol.Statement{SQL: "SELECT 1;", Phase: protocol.PhaseContract, Safety: "review", TransitionID: "test:chain"}
	statement.ID = protocol.StableStatementID(statement)
	return protocol.Result{
		CurrentFingerprint: current, DesiredFingerprint: desired, Status: protocol.Planned,
		Statements: []protocol.Statement{statement},
		Batches:    []protocol.Batch{{ID: "batch-contract-001", Phase: protocol.PhaseContract, Transactional: true, Statements: []protocol.Statement{statement}}},
	}
}
