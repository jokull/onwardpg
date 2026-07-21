package testkit

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type WriterEvidence struct {
	Target             string           `json:"target"`
	Environment        string           `json:"environment"`
	PlanID             string           `json:"plan_id"`
	BundleEntryDigest  string           `json:"bundle_entry_digest"`
	DesiredFingerprint string           `json:"desired_fingerprint"`
	Generation         int              `json:"generation"`
	Release            string           `json:"release"`
	ObservedAt         string           `json:"observed_at"`
	ExpiresAt          string           `json:"expires_at"`
	Cohorts            []EvidenceCohort `json:"cohorts"`
}

type EvidenceCohort struct {
	Category   string `json:"category"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	SourceKind string `json:"source_kind"`
	Source     string `json:"source"`
}

type evidenceManifest struct {
	Target     string `json:"target"`
	PlanID     string `json:"plan_id"`
	Generation int    `json:"generation"`
	Desired    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"desired_source"`
	History struct {
		EntryDigest string `json:"entry_digest"`
	} `json:"history"`
}

// BuildWriterEvidence binds an acceptance attestation to the current public
// manifest bytes. Call it only after verification has installed the expand
// checkpoint, because that receipt changes the history entry digest.
func BuildWriterEvidence(manifestPath, environment, release string, now time.Time) (WriterEvidence, error) {
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return WriterEvidence{}, fmt.Errorf("read evidence manifest: %w", err)
	}
	var manifest evidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return WriterEvidence{}, fmt.Errorf("decode evidence manifest: %w", err)
	}
	if manifest.Target == "" || manifest.PlanID == "" || manifest.Generation < 1 || manifest.Desired.Fingerprint == "" || manifest.History.EntryDigest == "" {
		return WriterEvidence{}, fmt.Errorf("manifest lacks complete evidence binding")
	}
	if environment == "" || release == "" {
		return WriterEvidence{}, fmt.Errorf("evidence environment and release are required")
	}
	evidence := WriterEvidence{
		Target: manifest.Target, Environment: environment, PlanID: manifest.PlanID,
		BundleEntryDigest: manifest.History.EntryDigest, DesiredFingerprint: manifest.Desired.Fingerprint,
		Generation: manifest.Generation, Release: release,
		ObservedAt: now.Add(-time.Minute).UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(20 * time.Minute).UTC().Format(time.RFC3339),
	}
	statuses := map[string]string{"previews": "isolated", "ad_hoc_writers": "read_only"}
	for _, category := range []string{"web", "workers", "scheduled_jobs", "queues", "connection_pools", "previews", "ad_hoc_writers"} {
		status := statuses[category]
		if status == "" {
			status = "drained"
		}
		evidence.Cohorts = append(evidence.Cohorts, EvidenceCohort{
			Category: category, Name: category, Status: status,
			SourceKind: "manual", Source: "native-acceptance/" + release,
		})
	}
	return evidence, nil
}

func WriteWriterEvidence(path string, evidence WriterEvidence) error {
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("encode writer evidence: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write writer evidence: %w", err)
	}
	return nil
}
