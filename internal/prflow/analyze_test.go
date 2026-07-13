package prflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/adapter"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/workspace"
)

func TestCompileDeterministicRejectsChangingOutput(t *testing.T) {
	calls := 0
	compiler := adapter.CompilerFunc(func(context.Context, adapter.CompileRequest) (adapter.Artifact, error) {
		calls++
		return adapter.DDL("test", []byte(fmt.Sprintf("SELECT %d;", calls))), nil
	})
	if _, err := compileDeterministic(context.Background(), compiler, adapter.CompileRequest{Revision: "dirty"}); err == nil || !strings.Contains(err.Error(), "nondeterministic") {
		t.Fatalf("expected nondeterminism rejection, got %v", err)
	}
}

func TestAnalyzeProvesBaseIntegrityAndPlansSyntheticHeadOnPostgreSQL(t *testing.T) {
	devURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if devURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	baseRoot, headRoot := t.TempDir(), t.TempDir()
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);\n"
	write(t, baseRoot, "schema.sql", baseDDL)
	writeHistoryBundle(t, baseRoot, devURL, "genesis", baseDDL)
	write(t, headRoot, "schema.sql", baseDDL+"CREATE TABLE app.projects (id bigint PRIMARY KEY);\n")
	target := workspace.Target{
		SchemaFile:     "schema.sql",
		DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16,
	}
	analysis, err := Analyze(context.Background(), Input{
		BaseRoot: baseRoot, HeadRoot: headRoot, BaseRevision: "base-tree", HeadRevision: "head-tree",
		TargetName: "primary", Target: target, BundleRoot: "onward-bundles", DevDatabaseURL: devURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Outcome != "ready" || analysis.SchemaSquare.BaseIntegrity != "matched" || analysis.SchemaSquare.BaseCodeFingerprint != analysis.SchemaSquare.BaseHistoryFingerprint {
		t.Fatalf("analysis = %#v", analysis)
	}
	if analysis.Plan == nil || analysis.Plan.Status != "planned" || !strings.Contains(planSQL(analysis), `CREATE TABLE "app"."projects"`) {
		t.Fatalf("plan = %#v", analysis.Plan)
	}
}

func TestAnalyzeBlocksUnhealthyBaseOnPostgreSQL(t *testing.T) {
	devURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if devURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	baseRoot, headRoot := t.TempDir(), t.TempDir()
	write(t, baseRoot, "schema.sql", "CREATE TABLE users (id bigint);\n")
	writeHistoryBundle(t, baseRoot, devURL, "genesis", "CREATE TABLE accounts (id bigint);\n")
	write(t, headRoot, "schema.sql", "CREATE TABLE users (id bigint);\n")
	target := workspace.Target{SchemaFile: "schema.sql", DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16}
	analysis, err := Analyze(context.Background(), Input{
		BaseRoot: baseRoot, HeadRoot: headRoot, BaseRevision: "base-tree", HeadRevision: "head-tree",
		TargetName: "primary", Target: target, BundleRoot: "onward-bundles", DevDatabaseURL: devURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Outcome != "blocked" || analysis.SchemaSquare.BaseIntegrity != "mismatched" || len(analysis.Problems) != 1 || analysis.Problems[0].Code != "base_integrity_mismatch" {
		t.Fatalf("analysis = %#v", analysis)
	}
}

func TestAnalyzeRebindsRenameAcrossUnrelatedBaseErosionOnPostgreSQL(t *testing.T) {
	devURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if devURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	oldBase, oldHead := t.TempDir(), t.TempDir()
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.old_users (id bigint PRIMARY KEY);\n"
	desiredDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);\n"
	write(t, oldBase, "schema.sql", baseDDL)
	writeHistoryBundle(t, oldBase, devURL, "genesis", baseDDL)
	write(t, oldHead, "schema.sql", desiredDDL)
	target := workspace.Target{SchemaFile: "schema.sql", DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16}
	previous, err := Analyze(context.Background(), Input{
		BaseRoot: oldBase, HeadRoot: oldHead, BaseRevision: "old-base", HeadRevision: "old-head",
		TargetName: "primary", Target: target, BundleRoot: "onward-bundles", DevDatabaseURL: devURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if previous.Plan == nil || previous.Plan.Status != protocol.NeedsInput || len(previous.Plan.Questions) != 1 || previous.Plan.Questions[0].ScopeFingerprint == "" {
		t.Fatalf("previous analysis = %#v", previous)
	}
	question := previous.Plan.Questions[0]
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: previous.Plan.CurrentFingerprint, DesiredFingerprint: previous.Plan.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: question.Choices[0], QuestionFingerprint: question.ScopeFingerprint}},
	}

	newBase, newHead := t.TempDir(), t.TempDir()
	erodedBaseDDL := baseDDL + "CREATE TABLE app.audit_log (id bigint PRIMARY KEY);\n"
	erodedHeadDDL := desiredDDL + "CREATE TABLE app.audit_log (id bigint PRIMARY KEY);\n"
	write(t, newBase, "schema.sql", erodedBaseDDL)
	writeHistoryBundle(t, newBase, devURL, "genesis", erodedBaseDDL)
	write(t, newHead, "schema.sql", erodedHeadDDL)
	rebound, err := AnalyzeWithReboundAnswers(context.Background(), Input{
		BaseRoot: newBase, HeadRoot: newHead, BaseRevision: "new-base", HeadRevision: "new-head",
		TargetName: "primary", Target: target, BundleRoot: "onward-bundles", DevDatabaseURL: devURL,
	}, answers, previous.Plan.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Outcome != "ready" || rebound.Rebind == nil || len(rebound.Rebind.Carried) != 1 || len(rebound.Rebind.Invalidated) != 0 {
		t.Fatalf("rebound analysis = %#v", rebound)
	}
}

func planSQL(analysis Analysis) string {
	var statements []string
	for _, statement := range analysis.Plan.Statements {
		statements = append(statements, statement.SQL)
	}
	return strings.Join(statements, "\n")
}

func write(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeHistoryBundle(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	current, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-history", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature", Mode: "pr",
		BaseRef: "origin/main", BaseCommit: strings.Repeat("a", 40), HeadRevision: strings.Repeat("b", 40),
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "empty history", Fingerprint: result.CurrentFingerprint, PostgresMajor: 16},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: 16},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}
