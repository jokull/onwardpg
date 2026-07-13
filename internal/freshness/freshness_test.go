package freshness

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
)

const (
	current = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	desired = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestAssessClassifiesFreshProvenanceAndSchemaChanges(t *testing.T) {
	artifact, result := testArtifact(t)
	digest, err := bundle.ResultDigest(result)
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{
		BaselineRef:      "main",
		BaselineRevision: strings.Repeat("a", 40), DesiredRevision: strings.Repeat("b", 40),
		BaselineFingerprint: current, DesiredFingerprint: desired,
		Planner: artifact.Manifest.Planner, ResultDigest: digest,
	}
	report, err := Assess(artifact, observation)
	if err != nil || report.Outcome != "fresh" {
		t.Fatalf("fresh report = %#v, %v", report, err)
	}
	observation.BaselineRevision = strings.Repeat("c", 40)
	report, err = Assess(artifact, observation)
	if err != nil || report.Outcome != "stale" || len(report.Findings) != 1 || report.Findings[0].Code != "provenance_stale" {
		t.Fatalf("provenance report = %#v, %v", report, err)
	}
	observation.BaselineFingerprint = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	report, err = Assess(artifact, observation)
	if err != nil || report.Findings[0].Code != "schema_stale" {
		t.Fatalf("schema report = %#v, %v", report, err)
	}
}

func TestAssessRequiresSuccessorForExecutedStaleBundle(t *testing.T) {
	artifact, result := testArtifact(t)
	digest, _ := bundle.ResultDigest(result)
	report, err := Assess(artifact, Observation{
		BaselineRef:      "main",
		BaselineRevision: strings.Repeat("a", 40), DesiredRevision: strings.Repeat("c", 40),
		BaselineFingerprint: current, DesiredFingerprint: desired, Planner: artifact.Manifest.Planner,
		ResultDigest: digest, Executed: true,
	})
	if err != nil || report.Outcome != "successor_required" || report.Findings[len(report.Findings)-1].Code != "immutable_successor_required" {
		t.Fatalf("report = %#v, %v", report, err)
	}
}

func testArtifact(t *testing.T) (bundle.Artifact, protocol.Result) {
	t.Helper()
	statement := protocol.Statement{SQL: "CREATE TABLE users (id bigint);", Phase: "expand", Safety: "safe"}
	statement.ID = protocol.StableStatementID(statement)
	result := protocol.Result{
		ProtocolVersion: protocol.Version, Status: protocol.Planned,
		CurrentFingerprint: current, DesiredFingerprint: desired,
		Statements: []protocol.Statement{statement},
		Batches:    []protocol.Batch{{ID: "batch-001", Phase: "expand", Transactional: true, Statements: []protocol.Statement{statement}}},
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: bundle.Metadata{
		BundleID: "feature", Generation: 1, Target: "primary", Purpose: "feature", Mode: "pr",
		BaseRef: "main", BaseCommit: strings.Repeat("a", 40), HeadRevision: strings.Repeat("b", 40),
		BaselineSource: bundle.SourceReceipt{Kind: "adapter", Description: "base tree", Fingerprint: current},
		DesiredSource:  bundle.SourceReceipt{Kind: "adapter", Description: "desired tree", Fingerprint: desired},
		Planner:        bundle.PlannerReceipt{Version: "test"},
	}, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	return artifact, result
}
