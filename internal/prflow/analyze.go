// Package prflow composes Git provenance, declarative compilation, migration
// replay, and graph planning for the PR comparison boundary.
package prflow

import (
	"bytes"
	"context"
	"fmt"

	"github.com/jokull/onwardpg/adapter"
	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/workspace"
	"github.com/jokull/onwardpg/pgschema"
)

const Version = "onwardpg.pr-analysis/v1"

type Input struct {
	Repository         gitbase.Repository
	TargetName         string
	Target             workspace.Target
	BaseRef            string
	HeadRef            string
	IncludeWorkingTree bool
	ExcludePaths       []string
	DevDatabaseURL     string
	Ignores            []string
	Answers            protocol.Answers
	PlannerOptions     graphplan.Options
}

type SchemaSquare struct {
	BaseCodeFingerprint    string `json:"base_code_fingerprint,omitempty"`
	BaseHistoryFingerprint string `json:"base_history_fingerprint,omitempty"`
	HeadCodeFingerprint    string `json:"head_code_fingerprint,omitempty"`
	BaseIntegrity          string `json:"base_integrity"`
	HeadArtifactFidelity   string `json:"head_artifact_fidelity"`
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
	Git             gitbase.Status   `json:"git"`
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
	if input.Repository.Root == "" || input.TargetName == "" {
		return Analysis{}, fmt.Errorf("repository and target name are required")
	}
	status, err := input.Repository.Inspect(ctx, gitbase.Options{
		BaseRef: input.BaseRef, HeadRef: input.HeadRef, MigrationPath: input.Target.MigrationPath,
		IncludeWorkingTree: input.IncludeWorkingTree, ExcludePaths: input.ExcludePaths,
	})
	if err != nil {
		return Analysis{}, err
	}
	analysis := Analysis{
		ProtocolVersion: Version, Outcome: "blocked", Git: status,
		SchemaSquare: SchemaSquare{BaseIntegrity: "unchecked", HeadArtifactFidelity: "unchecked"},
	}
	if status.Outcome == "blocked" {
		for _, problem := range status.Problems {
			analysis.Problems = append(analysis.Problems, Problem{Code: problem.Code, Message: problem.Message + ": " + problem.Path})
		}
		return analysis, nil
	}

	baseTree, err := input.Repository.PrepareCommitTree(ctx, status.BaseCommit)
	if err != nil {
		return Analysis{}, fmt.Errorf("prepare base tree: %w", err)
	}
	defer baseTree.Close()
	headTree, err := input.Repository.PreparePRTree(ctx, status)
	if err != nil {
		return Analysis{}, fmt.Errorf("prepare synthetic head tree: %w", err)
	}
	defer headTree.Close()

	compiler := workspace.TargetCompiler{TargetName: input.TargetName, Target: input.Target}
	baseArtifact, err := compileDeterministic(ctx, compiler, adapter.CompileRequest{Root: baseTree.Root, Target: input.TargetName, Revision: status.BaseCommit})
	if err != nil {
		return Analysis{}, fmt.Errorf("compile base declarative schema: %w", err)
	}
	headArtifact, err := compileDeterministic(ctx, compiler, adapter.CompileRequest{Root: headTree.Root, Target: input.TargetName, Revision: status.HeadRevision})
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
	history, err := workspace.LoadMigrationHistory(baseTree.Root, input.Target)
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
		HeadCodeFingerprint: headCodeFingerprint, BaseIntegrity: "matched", HeadArtifactFidelity: "not_generated",
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
	if first.Provenance != second.Provenance || !bytes.Equal(first.DDL, second.DDL) || !equivalentSnapshots(first.Snapshot, second.Snapshot) {
		return adapter.Artifact{}, fmt.Errorf("schema compiler is nondeterministic for revision %s", request.Revision)
	}
	return first, nil
}

func equivalentSnapshots(first, second *pgschema.Snapshot) bool {
	if first == nil || second == nil {
		return first == second
	}
	firstFingerprint, firstErr := first.Fingerprint()
	secondFingerprint, secondErr := second.Fingerprint()
	return firstErr == nil && secondErr == nil && firstFingerprint == secondFingerprint
}

func artifactGraph(ctx context.Context, artifact adapter.Artifact, devURL string, ignores []string) (*pgschema.Snapshot, error) {
	if artifact.Snapshot != nil {
		return artifact.Snapshot, nil
	}
	return source.LoadDDLGraphForComparison(ctx, artifact.DDL, artifact.Provenance, devURL, ignores)
}
