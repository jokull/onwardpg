package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/prflow"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

func TestPlanCLIWritesVersionedBundleOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	directory := t.TempDir()
	current := filepath.Join(directory, "current.sql")
	desired := filepath.Join(directory, "desired.sql")
	if err := os.WriteFile(current, []byte("CREATE SCHEMA app;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desired, []byte("CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(directory, "bundle")
	output := captureStdout(t, func() int {
		return runPlan([]string{
			"--from", "file://" + current, "--to", "file://" + desired, "--dev-url", url,
			"--bundle", bundlePath, "--bundle-id", "users", "--target", "primary-postgres",
			"--base-ref", "origin/main", "--base-commit", strings.Repeat("a", 40), "--head-revision", strings.Repeat("b", 40),
		})
	})
	if output.code != 0 {
		t.Fatalf("runPlan exit = %d, stdout = %s", output.code, output.stdout)
	}
	data, err := os.ReadFile(filepath.Join(bundlePath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest bundle.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != bundle.Version || manifest.State != "planned" || manifest.Phases["expand"].Path != "phases/expand.sql" {
		t.Fatalf("manifest = %#v", manifest)
	}
	phase, err := os.ReadFile(filepath.Join(bundlePath, "phases", "expand.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(phase), `CREATE TABLE "app"."users"`) {
		t.Fatalf("expand phase = %s", phase)
	}
}

func TestPRRegenerateWritesVerifiedBaseToSyntheticHeadBundle(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	git(t, repository, "init", "-b", "main")
	git(t, repository, "config", "user.name", "Onward Test")
	git(t, repository, "config", "user.email", "onward@example.test")
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
postgres_major = 16
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base")
	git(t, repository, "checkout", "-b", "feature")
	writeTestFile(t, repository, "schema.sql", baseDDL+"CREATE TABLE app.projects (id bigint PRIMARY KEY);\n")
	git(t, repository, "add", "schema.sql")
	git(t, repository, "commit", "-m", "feature schema")

	output := captureStdout(t, func() int {
		return runPRAt([]string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "projects"}, repository)
	})
	if output.code != 0 {
		t.Fatalf("exit = %d, stdout = %s", output.code, output.stdout)
	}
	var analysis prflow.Analysis
	if err := json.Unmarshal([]byte(output.stdout), &analysis); err != nil {
		t.Fatal(err)
	}
	if analysis.Outcome != "ready" || analysis.Bundle == nil || analysis.Bundle.Generation != 1 || analysis.SchemaSquare.BaseIntegrity != "matched" {
		t.Fatalf("analysis = %#v", analysis)
	}
	bundlePath := filepath.Join(repository, filepath.FromSlash(analysis.Bundle.Path))
	artifact, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.SchemaSquare == nil || artifact.Manifest.SchemaSquare.BaseCodeFingerprint != artifact.Manifest.SchemaSquare.BaseHistoryFingerprint || artifact.Manifest.SchemaSquare.HeadHistoryFidelity != "matched" || artifact.Manifest.SchemaSquare.HeadCodeFingerprint != artifact.Manifest.SchemaSquare.HeadHistoryFingerprint {
		t.Fatalf("bundle schema square = %#v", artifact.Manifest.SchemaSquare)
	}
	if artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest == bundle.HistoryRootDigest() {
		t.Fatalf("bundle history receipt = %#v", artifact.Manifest.History)
	}
	if !strings.Contains(string(artifact.Files["phases/expand.sql"]), `CREATE TABLE "app"."projects"`) {
		t.Fatalf("expand phase = %s", artifact.Files["phases/expand.sql"])
	}

	// The existing receipt root is excluded from source revision provenance, so
	// an explicit draft regeneration remains on the same clean head revision.
	second := captureStdout(t, func() int {
		return runPRAt([]string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "projects", "--replace-draft"}, repository)
	})
	if second.code != 0 {
		t.Fatalf("second exit = %d, stdout = %s", second.code, second.stdout)
	}
	var regenerated prflow.Analysis
	if err := json.Unmarshal([]byte(second.stdout), &regenerated); err != nil {
		t.Fatal(err)
	}
	if regenerated.Inputs.DesiredRevision != analysis.Inputs.DesiredRevision || regenerated.Bundle == nil || regenerated.Bundle.Generation != 1 {
		t.Fatalf("regenerated analysis = %#v", regenerated)
	}

	statusOutput := captureStdout(t, func() int {
		return runPRAt([]string{"status", "--base", "main", "--target", "primary", "--bundle", "projects"}, repository)
	})
	if statusOutput.code != 0 {
		t.Fatalf("freshness status exit = %d, stdout = %s", statusOutput.code, statusOutput.stdout)
	}
	var status prStatusReport
	if err := json.Unmarshal([]byte(statusOutput.stdout), &status); err != nil {
		t.Fatal(err)
	}
	if status.ProtocolVersion != prStatusVersion || status.Outcome != "fresh" || status.Freshness == nil || status.Freshness.Outcome != "fresh" {
		t.Fatalf("freshness status = %#v", status)
	}

}

func TestPRRegenerateCarriesRenameAnswerAcrossUnrelatedBaseErosion(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	git(t, repository, "init", "-b", "main")
	git(t, repository, "config", "user.name", "Onward Test")
	git(t, repository, "config", "user.email", "onward@example.test")
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_command = ["sh", "-c", "cat schema/*.sql"]
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
postgres_major = 16
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.old_users (id bigint);\n"
	writeTestFile(t, repository, "schema/00_base.sql", baseDDL)
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base")

	git(t, repository, "checkout", "-b", "feature")
	featureDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint);\n"
	writeTestFile(t, repository, "schema/00_base.sql", featureDDL)
	git(t, repository, "add", "schema/00_base.sql")
	git(t, repository, "commit", "-m", "rename users")
	first := captureStdout(t, func() int {
		return runPRAt([]string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "rename-users"}, repository)
	})
	if first.code != 2 {
		t.Fatalf("first exit = %d, stdout = %s", first.code, first.stdout)
	}
	var pending prflow.Analysis
	if err := json.Unmarshal([]byte(first.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Plan == nil || len(pending.Plan.Questions) != 1 || pending.Plan.Questions[0].ScopeFingerprint == "" {
		t.Fatalf("pending = %#v", pending)
	}
	question := pending.Plan.Questions[0]
	pendingArtifact, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/rename-users"))
	if err != nil {
		t.Fatal(err)
	}
	answerPath := filepath.Join(repository, "rename.answers.json")
	answer := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.Plan.CurrentFingerprint, DesiredFingerprint: pending.Plan.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: question.Choices[0], QuestionFingerprint: question.ScopeFingerprint}},
	}
	answerData, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(answerPath, append(answerData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, err := gitbase.Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := opened.Inspect(context.Background(), gitbase.Options{
		BaseRef: "main", HeadRef: "HEAD", HistoryPath: "onward-bundles/primary", IncludeWorkingTree: true,
		ExcludePaths: append([]string{"onward-bundles"}, repositoryReceiptPaths(repository, answerPath)...),
	})
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Dirty {
		t.Fatalf("answer and draft exclusions still produced dirty revision: %#v", preflight)
	}
	if pendingArtifact.Manifest.HeadRevision != preflight.HeadRevision {
		t.Fatalf("pending head revision %s differs from excluded preflight %s", pendingArtifact.Manifest.HeadRevision, preflight.HeadRevision)
	}
	ready := captureStdout(t, func() int {
		return runPRAt([]string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "rename-users", "--answers", answerPath, "--replace-draft"}, repository)
	})
	if ready.code != 0 {
		t.Fatalf("ready exit = %d, stdout = %s", ready.code, ready.stdout)
	}
	prior, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/rename-users"))
	if err != nil {
		t.Fatal(err)
	}
	priorQuestions, err := bundle.DecisionQuestions(prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(priorQuestions) != 1 || priorQuestions[0].Kind != "rename_table" {
		t.Fatalf("preserved questions = %#v, manifest = %#v", priorQuestions, prior.Manifest)
	}
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "review rename migration")

	git(t, repository, "checkout", "main")
	auditDDL := "CREATE TABLE app.audit_log (id bigint);\n"
	writeTestFile(t, repository, "schema/10_audit.sql", auditDDL)
	writeHistoryTransitionFixture(t, repository, url, "add-audit", baseDDL+auditDDL)
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "add audit log")
	git(t, repository, "checkout", "feature")
	git(t, repository, "merge", "main", "--no-edit")

	restacked := captureStdout(t, func() int {
		return runPRAt([]string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "rename-users", "--replace-draft"}, repository)
	})
	if restacked.code != 0 {
		t.Fatalf("restacked exit = %d, stdout = %s", restacked.code, restacked.stdout)
	}
	var analysis prflow.Analysis
	if err := json.Unmarshal([]byte(restacked.stdout), &analysis); err != nil {
		t.Fatal(err)
	}
	if analysis.Rebind == nil || len(analysis.Rebind.Carried) != 1 || len(analysis.Rebind.Invalidated) != 0 || analysis.Outcome != "ready" {
		t.Fatalf("restacked analysis = %#v", analysis)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/rename-users"))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest == bundle.HistoryRootDigest() || artifact.Manifest.Generation < 2 || chain.HeadDigest != artifact.Manifest.History.EntryDigest {
		t.Fatalf("restacked manifest = %#v", artifact.Manifest)
	}
	var reboundAnswers protocol.Answers
	if err := json.Unmarshal(artifact.Files["answers.json"], &reboundAnswers); err != nil {
		t.Fatal(err)
	}
	if reboundAnswers.CurrentFingerprint != analysis.Plan.CurrentFingerprint || reboundAnswers.DesiredFingerprint != analysis.Plan.DesiredFingerprint || reboundAnswers.Answers[0].QuestionFingerprint != question.ScopeFingerprint {
		t.Fatalf("rebound answers = %#v", reboundAnswers)
	}
}

func writeHistoryFixture(t *testing.T, root, devURL, id, desiredDDL string) {
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

func writeHistoryTransitionFixture(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	chain, err := history.Load(root, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-transition-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature", Mode: "pr",
		BaseRef: "origin/main", BaseCommit: strings.Repeat("c", 40), HeadRevision: strings.Repeat("d", 40),
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: 16},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture transition", Fingerprint: result.DesiredFingerprint, PostgresMajor: 16},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

type captured struct {
	code   int
	stdout string
}

func captureStdout(t *testing.T, call func() int) captured {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	code := call()
	_ = writer.Close()
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	return captured{code: code, stdout: string(data)}
}
