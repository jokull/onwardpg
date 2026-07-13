// Package prflow composes prepared source trees, declarative compilation,
// migration replay, and graph planning for the PR comparison boundary. It is
// deliberately unaware of how callers obtained those trees.
package prflow

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"github.com/jokull/onwardpg/adapter"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/workspace"
	"github.com/jokull/onwardpg/pgschema"
)

const Version = "onwardpg.pr-analysis/v1"

type Input struct {
	BaseRoot       string
	HeadRoot       string
	BaseRevision   string
	HeadRevision   string
	TargetName     string
	Target         workspace.Target
	DevDatabaseURL string
	Ignores        []string
	Answers        protocol.Answers
	PlannerOptions graphplan.Options
}

type InputReceipt struct {
	BaselineRevision string `json:"baseline_revision"`
	DesiredRevision  string `json:"desired_revision"`
}

type SchemaSquare struct {
	BaseCodeFingerprint    string `json:"base_code_fingerprint,omitempty"`
	BaseHistoryFingerprint string `json:"base_history_fingerprint,omitempty"`
	HeadCodeFingerprint    string `json:"head_code_fingerprint,omitempty"`
	HeadHistoryFingerprint string `json:"head_history_fingerprint,omitempty"`
	BaseIntegrity          string `json:"base_integrity"`
	HeadHistoryFidelity    string `json:"head_history_fidelity"`
}

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BundleOutput struct {
	Path       string `json:"path"`
	BundleID   string `json:"bundle_id"`
	Generation int    `json:"generation"`
	State      string `json:"state"`
}

type Analysis struct {
	ProtocolVersion string           `json:"protocol_version"`
	Outcome         string           `json:"outcome"`
	Inputs          InputReceipt     `json:"inputs"`
	SchemaSquare    SchemaSquare     `json:"schema_square"`
	Problems        []Problem        `json:"problems,omitempty"`
	Plan            *protocol.Result `json:"plan,omitempty"`
	BaseProvenance  string           `json:"base_provenance,omitempty"`
	HeadProvenance  string           `json:"head_provenance,omitempty"`
	HistoryDigest   string           `json:"history_digest,omitempty"`
	HistoryFiles    []string         `json:"history_files,omitempty"`
	Bundle          *BundleOutput    `json:"bundle,omitempty"`
}

func Analyze(ctx context.Context, input Input) (Analysis, error) {
	if input.TargetName == "" {
		return Analysis{}, fmt.Errorf("target name is required")
	}
	if input.BaseRoot == "" || !filepath.IsAbs(input.BaseRoot) || input.HeadRoot == "" || !filepath.IsAbs(input.HeadRoot) {
		return Analysis{}, fmt.Errorf("prepared base and head roots must be absolute paths")
	}
	if input.BaseRevision == "" || input.HeadRevision == "" {
		return Analysis{}, fmt.Errorf("base and head revision receipts are required")
	}
	analysis := Analysis{
		ProtocolVersion: Version, Outcome: "blocked",
		Inputs:       InputReceipt{BaselineRevision: input.BaseRevision, DesiredRevision: input.HeadRevision},
		SchemaSquare: SchemaSquare{BaseIntegrity: "unchecked", HeadHistoryFidelity: "unchecked"},
	}

	compiler := workspace.TargetCompiler{TargetName: input.TargetName, Target: input.Target}
	baseArtifact, err := compileDeterministic(ctx, compiler, adapter.CompileRequest{Root: input.BaseRoot, Target: input.TargetName, Revision: input.BaseRevision})
	if err != nil {
		return Analysis{}, fmt.Errorf("compile base declarative schema: %w", err)
	}
	headArtifact, err := compileDeterministic(ctx, compiler, adapter.CompileRequest{Root: input.HeadRoot, Target: input.TargetName, Revision: input.HeadRevision})
	if err != nil {
		return Analysis{}, fmt.Errorf("compile head declarative schema: %w", err)
	}
	baseCode, err := artifactGraph(ctx, baseArtifact, input.DevDatabaseURL, input.Ignores)
	if err != nil {
		return Analysis{}, fmt.Errorf("materialize base declarative schema: %w", err)
	}
	headCode, err := artifactGraph(ctx, headArtifact, input.DevDatabaseURL, input.Ignores)
	if err != nil {
		return Analysis{}, fmt.Errorf("materialize head declarative schema: %w", err)
	}
	history, err := workspace.LoadMigrationHistory(input.BaseRoot, input.Target)
	if err != nil {
		return Analysis{}, fmt.Errorf("load base migration history: %w", err)
	}
	baseHistory, err := source.LoadDDLGraphForComparison(ctx, history.DDL, history.Provenance, input.DevDatabaseURL, input.Ignores)
	if err != nil {
		return Analysis{}, fmt.Errorf("replay base migration history: %w", err)
	}
	if err := source.ValidateIgnoreSelectors(input.Ignores, baseCode, baseHistory, headCode); err != nil {
		return Analysis{}, err
	}
	baseCodeFingerprint, err := baseCode.Fingerprint()
	if err != nil {
		return Analysis{}, err
	}
	baseHistoryFingerprint, err := baseHistory.Fingerprint()
	if err != nil {
		return Analysis{}, err
	}
	headCodeFingerprint, err := headCode.Fingerprint()
	if err != nil {
		return Analysis{}, err
	}
	analysis.SchemaSquare = SchemaSquare{
		BaseCodeFingerprint: baseCodeFingerprint, BaseHistoryFingerprint: baseHistoryFingerprint,
		HeadCodeFingerprint: headCodeFingerprint, BaseIntegrity: "matched", HeadHistoryFidelity: "not_replayed",
	}
	analysis.BaseProvenance, analysis.HeadProvenance = baseArtifact.Provenance, headArtifact.Provenance
	analysis.HistoryDigest, analysis.HistoryFiles = history.Digest, history.Files
	if baseCodeFingerprint != baseHistoryFingerprint {
		analysis.SchemaSquare.BaseIntegrity = "mismatched"
		analysis.Problems = append(analysis.Problems, Problem{
			Code:    "base_integrity_mismatch",
			Message: "base declarative schema does not equal replayed base migration history; repair the base lineage separately",
		})
		return analysis, nil
	}
	result, err := graphplan.Build(baseCode, headCode, input.Answers, input.PlannerOptions)
	if err != nil {
		return Analysis{}, err
	}
	analysis.Plan = &result
	switch result.Status {
	case protocol.Planned:
		analysis.Outcome = "ready"
	case protocol.NeedsInput:
		analysis.Outcome = "needs_input"
	case protocol.Unsupported:
		analysis.Outcome = "unsupported"
	default:
		return Analysis{}, fmt.Errorf("planner returned invalid status %q", result.Status)
	}
	return analysis, nil
}

func compileDeterministic(ctx context.Context, compiler adapter.Compiler, request adapter.CompileRequest) (adapter.Artifact, error) {
	first, err := compiler.Compile(ctx, request)
	if err != nil {
		return adapter.Artifact{}, err
	}
	if err := first.Validate(); err != nil {
		return adapter.Artifact{}, err
	}
	second, err := compiler.Compile(ctx, request)
	if err != nil {
		return adapter.Artifact{}, err
	}
	if err := second.Validate(); err != nil {
		return adapter.Artifact{}, err
	}
	snapshotsEqual, err := equivalentSnapshots(first.Snapshot, second.Snapshot)
	if err != nil {
		return adapter.Artifact{}, fmt.Errorf("fingerprint compiler snapshot for revision %s: %w", request.Revision, err)
	}
	if first.Provenance != second.Provenance || !bytes.Equal(first.DDL, second.DDL) || !snapshotsEqual {
		return adapter.Artifact{}, fmt.Errorf("schema compiler is nondeterministic for revision %s", request.Revision)
	}
	return first, nil
}

func equivalentSnapshots(first, second *pgschema.Snapshot) (bool, error) {
	if first == nil || second == nil {
		return first == second, nil
	}
	firstFingerprint, firstErr := first.Fingerprint()
	if firstErr != nil {
		return false, firstErr
	}
	secondFingerprint, secondErr := second.Fingerprint()
	if secondErr != nil {
		return false, secondErr
	}
	return firstFingerprint == secondFingerprint, nil
}

func artifactGraph(ctx context.Context, artifact adapter.Artifact, devURL string, ignores []string) (*pgschema.Snapshot, error) {
	if artifact.Snapshot != nil {
		if len(ignores) > 0 {
			return nil, fmt.Errorf("ignore selectors are unavailable for typed snapshot compiler output")
		}
		return artifact.Snapshot, nil
	}
	return source.LoadDDLGraphForComparison(ctx, artifact.DDL, artifact.Provenance, devURL, ignores)
}
