package contractcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jokull/onwardpg/internal/bundle"
)

// chainSelection is the place of the checked bundle in its target history.
// A bundle before the head can be checked only while the database sits
// exactly at its post-expand checkpoint: that state exists only after every
// earlier bundle's contract and this bundle's expand, and before any later
// bundle's expand. The checkpoint comparison therefore enforces chain order.
type chainSelection struct {
	index   int
	entries []bundle.Artifact
}

func selectChainEntry(input Input) (chainSelection, error) {
	manifest := input.Artifact.Manifest
	if manifest.History == nil || input.ExpectedHead == "" {
		return chainSelection{}, fmt.Errorf("selected bundle is not the expected history chain head")
	}
	if len(input.Chain) == 0 {
		if manifest.History.EntryDigest != input.ExpectedHead {
			return chainSelection{}, fmt.Errorf("selected bundle is not the expected history chain head")
		}
		return chainSelection{index: 0, entries: []bundle.Artifact{input.Artifact}}, nil
	}
	head := input.Chain[len(input.Chain)-1].Manifest.History
	if head == nil || head.EntryDigest != input.ExpectedHead {
		return chainSelection{}, fmt.Errorf("history chain does not end at the expected head")
	}
	for index, entry := range input.Chain {
		if entry.Manifest.History == nil {
			return chainSelection{}, fmt.Errorf("history entry %d has no history receipt", index+1)
		}
		if index > 0 && entry.Manifest.History.ParentDigest != input.Chain[index-1].Manifest.History.EntryDigest {
			return chainSelection{}, fmt.Errorf("history entry %s does not follow %s", entry.Manifest.BundleID, input.Chain[index-1].Manifest.BundleID)
		}
		if entry.Manifest.History.EntryDigest == manifest.History.EntryDigest {
			return chainSelection{index: index, entries: input.Chain}, nil
		}
	}
	return chainSelection{}, fmt.Errorf("selected bundle %s is not in the target history chain", manifest.BundleID)
}

func (s chainSelection) head() bool { return s.index == len(s.entries)-1 }

// classifyCatalogPosition names where an observed catalog that is not the
// selected post-expand checkpoint sits in the chain.
func (s chainSelection) classifyCatalogPosition(observed string, checkpoint bundle.CatalogCheckpoint) (string, string) {
	switch observed {
	case checkpoint.BaselineFingerprint:
		return "expand_not_applied", "production still matches the pre-expand baseline"
	case checkpoint.DesiredFingerprint:
		return "contract_already_applied", "production already matches the desired post-contract catalog"
	}
	for index := s.index - 1; index >= 0; index-- {
		if matchesEntry(s.entries[index], observed) {
			bundleID := s.entries[index].Manifest.BundleID
			return "predecessor_contract_pending", "production matches the catalog of earlier bundle " + bundleID + "; finish the earlier bundles' contracts and this bundle's expand in chain order first"
		}
	}
	for index := s.index + 1; index < len(s.entries); index++ {
		if matchesEntry(s.entries[index], observed) {
			bundleID := s.entries[index].Manifest.BundleID
			return "successor_expand_applied", "production matches the catalog of later bundle " + bundleID + "; a later bundle ran before this contract, out of chain order"
		}
	}
	return "catalog_drift", "production does not match the receipted post-expand catalog"
}

func matchesEntry(entry bundle.Artifact, observed string) bool {
	manifest := entry.Manifest
	if observed == manifest.BaselineSource.Fingerprint || observed == manifest.DesiredSource.Fingerprint {
		return true
	}
	checkpoint, err := bundle.ReadCatalogCheckpoint(entry)
	return err == nil && observed == checkpoint.ExpandFingerprint
}

// EvidenceReceipt records the writer evidence that satisfied the attestation
// gates, so a ready report shows what was attested and by which source.
type EvidenceReceipt struct {
	Digest     string   `json:"digest"`
	Release    string   `json:"release"`
	ObservedAt string   `json:"observed_at"`
	ExpiresAt  string   `json:"expires_at"`
	Cohorts    []Cohort `json:"cohorts"`
}

// selectEvidence decodes one evidence object, or a JSON array of evidence
// objects that covers several bundles of a chain, and returns the one bound
// to manifest. A single object is returned as is, so a wrong binding is still
// reported as a binding mismatch.
func selectEvidence(data []byte, manifest bundle.Manifest) (Evidence, []byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		evidence, err := DecodeEvidence(trimmed)
		return evidence, trimmed, err
	}
	var elements []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(&elements); err != nil {
		return Evidence{}, nil, err
	}
	if decoder.More() {
		return Evidence{}, nil, fmt.Errorf("writer evidence contains more than one JSON value")
	}
	if len(elements) == 0 {
		return Evidence{}, nil, fmt.Errorf("writer evidence array is empty")
	}
	var selected *Evidence
	var selectedBytes []byte
	for index, element := range elements {
		evidence, err := DecodeEvidence(element)
		if err != nil {
			return Evidence{}, nil, fmt.Errorf("writer evidence %d: %w", index+1, err)
		}
		if manifest.History == nil || evidence.PlanID != manifest.PlanID || evidence.BundleEntryDigest != manifest.History.EntryDigest {
			continue
		}
		if selected != nil {
			return Evidence{}, nil, fmt.Errorf("more than one writer evidence object binds bundle %s", manifest.BundleID)
		}
		selected, selectedBytes = &evidence, bytes.TrimSpace(element)
	}
	if selected == nil {
		return Evidence{}, nil, errNoEvidenceBinding
	}
	return *selected, selectedBytes, nil
}

var errNoEvidenceBinding = fmt.Errorf("no writer evidence object binds the selected bundle")

func evidenceReceipt(evidence Evidence, body []byte) *EvidenceReceipt {
	sum := sha256.Sum256(body)
	return &EvidenceReceipt{
		Digest: "sha256:" + hex.EncodeToString(sum[:]), Release: evidence.Release,
		ObservedAt: evidence.ObservedAt, ExpiresAt: evidence.ExpiresAt,
		Cohorts: append([]Cohort(nil), evidence.Cohorts...),
	}
}
