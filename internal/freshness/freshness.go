// Package freshness compares a validated bundle with a newly observed source
// contract. It is read-only and independent of Git, schema compilers, and
// migration runners.
package freshness

import (
	"fmt"
	"reflect"

	"github.com/jokull/onwardpg/internal/bundle"
)

const Version = "onwardpg.freshness/v1"

type Observation struct {
	BaselineRef         string
	BaselineRevision    string
	DesiredRevision     string
	BaselineFingerprint string
	DesiredFingerprint  string
	Planner             bundle.PlannerReceipt
	ResultDigest        string
	Executed            bool
}

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

type Report struct {
	ProtocolVersion string    `json:"protocol_version"`
	Outcome         string    `json:"outcome"`
	Findings        []Finding `json:"findings,omitempty"`
}

func ArtifactStale(err error) Report {
	message := "the bundle cannot be validated"
	if err != nil {
		message += ": " + err.Error()
	}
	return Report{ProtocolVersion: Version, Outcome: "stale", Findings: []Finding{{
		Code: "artifact_stale", Message: message,
		Remediation: "restore the reviewed artifact or explicitly remove the unexecuted draft and regenerate it",
	}}}
}

func Assess(artifact bundle.Artifact, observation Observation) (Report, error) {
	if err := artifact.Validate(); err != nil {
		return Report{}, fmt.Errorf("validate bundle before freshness assessment: %w", err)
	}
	if observation.BaselineRef == "" || observation.BaselineRevision == "" || observation.DesiredRevision == "" ||
		observation.BaselineFingerprint == "" || observation.DesiredFingerprint == "" ||
		observation.ResultDigest == "" || observation.Planner.Version == "" {
		return Report{}, fmt.Errorf("freshness observation is incomplete")
	}
	manifest := artifact.Manifest
	var findings []Finding
	add := func(code, message, remediation string) {
		findings = append(findings, Finding{Code: code, Message: message, Remediation: remediation})
	}
	if manifest.BaselineSource.Fingerprint != observation.BaselineFingerprint || manifest.DesiredSource.Fingerprint != observation.DesiredFingerprint {
		add("schema_stale", "the current or desired schema fingerprint changed", "regenerate the draft from the newly observed schemas")
	} else if manifest.BaseRef != observation.BaselineRef || manifest.BaseCommit != observation.BaselineRevision || manifest.HeadRevision != observation.DesiredRevision {
		add("provenance_stale", "tree provenance changed while the observed schemas remained equivalent", "refresh the draft receipts against the current prepared trees")
	}
	if !reflect.DeepEqual(manifest.Planner, observation.Planner) || manifest.ResultDigest != observation.ResultDigest {
		add("decision_stale", "planner version, options, questions, answers, or resulting plan changed", "rerun planning and answer only the invalidated questions")
	}
	if len(findings) == 0 {
		return Report{ProtocolVersion: Version, Outcome: "fresh"}, nil
	}
	if observation.Executed {
		findings = append(findings, Finding{
			Code: "immutable_successor_required", Message: "the stale generation has execution evidence and cannot be replaced",
			Remediation: "create a new forward successor generation from the recorded post-execution state",
		})
		return Report{ProtocolVersion: Version, Outcome: "successor_required", Findings: findings}, nil
	}
	return Report{ProtocolVersion: Version, Outcome: "stale", Findings: findings}, nil
}
