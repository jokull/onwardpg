package contractcheck

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/bundle"
)

func TestWriterEvidenceRequiresEveryCohortAndExactBinding(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	manifest := bundle.Manifest{
		BundleID: "feature", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 2, Target: "primary",
		History: &bundle.HistoryReceipt{EntryDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	evidence := Evidence{
		Target: "primary", Environment: "production", PlanID: manifest.PlanID,
		BundleEntryDigest: manifest.History.EntryDigest, DesiredFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Generation: 2, Release: "release-42",
		ObservedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Cohorts: []Cohort{{Category: "previews", Name: "vercel previews", Status: "isolated", SourceKind: "provider", Source: "vercel/deployment-policy/42"}},
	}
	manifest.DesiredSource.Fingerprint = evidence.DesiredFingerprint
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeEvidence(encoded)
	if err != nil {
		t.Fatal(err)
	}
	status, finding := validateEvidence(decoded, manifest, "production", now, []string{"previews", "web"})
	if status != "blocked" || finding == nil || finding.Code != "writer_cohort_missing" {
		t.Fatalf("missing cohort status=%q finding=%#v", status, finding)
	}
	evidence.Cohorts = append(evidence.Cohorts, Cohort{Category: "web", Name: "api", Status: "upgraded", SourceKind: "manual", Source: "deploy-review-42"})
	status, finding = validateEvidence(evidence, manifest, "production", now, []string{"previews", "web"})
	if status != "ready" || finding != nil {
		t.Fatalf("complete evidence status=%q finding=%#v", status, finding)
	}
	evidence.Environment = "staging"
	status, finding = validateEvidence(evidence, manifest, "production", now, []string{"previews", "web"})
	if status != "stale" || finding == nil || finding.Code != "writer_evidence_binding_mismatch" {
		t.Fatalf("wrong environment status=%q finding=%#v", status, finding)
	}
}

func TestWriterEvidenceUnknownAndExpiredBlockDifferently(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	manifest := bundle.Manifest{PlanID: "plan_x", Generation: 1, Target: "primary", History: &bundle.HistoryReceipt{EntryDigest: "head"}}
	evidence := Evidence{
		Target: "primary", Environment: "production", PlanID: "plan_x", BundleEntryDigest: "head", Generation: 1,
		DesiredFingerprint: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ObservedAt:         now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
		Cohorts: []Cohort{{Category: "previews", Name: "preview fleet", Status: "unknown", SourceKind: "manual", Source: "review"}},
	}
	manifest.DesiredSource.Fingerprint = evidence.DesiredFingerprint
	status, finding := validateEvidence(evidence, manifest, "production", now, []string{"previews"})
	if status != "blocked" || finding.Code != "writer_cohort_unknown" {
		t.Fatalf("unknown status=%q finding=%#v", status, finding)
	}
	evidence.Cohorts[0].Status = "drained"
	status, finding = validateEvidence(evidence, manifest, "production", now.Add(2*time.Minute), []string{"previews"})
	if status != "stale" || finding.Code != "writer_evidence_expired" {
		t.Fatalf("expired status=%q finding=%#v", status, finding)
	}
}
