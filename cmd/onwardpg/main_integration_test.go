package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/prflow"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
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

func TestDevPlanQuestionsNeverApplyAndConvergesAfterDeliberateApplication(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	t.Setenv("ONWARDPG_DEV_TEST_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_DEV_TEST_URL"
postgres_major = 16
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint);\n")
	connection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);"); err != nil {
		t.Fatal(err)
	}

	pendingOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("pending exit = %d, stdout = %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending protocol.Result
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("pending = %#v", pending)
	}
	question := pending.Questions[0]
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint,
		DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: question.Kind, Key: question.Key, Value: question.Choices[0],
			QuestionFingerprint: question.ScopeFingerprint,
		}},
	}
	answerData, err := json.MarshalIndent(answers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	answerPath := filepath.Join(repository, "answers.json")
	if err := os.WriteFile(answerPath, append(answerData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary", "--answers", answerPath}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("planned exit = %d, stdout = %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned protocol.Result
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) == 0 || planned.Statements[0].Phase != "contract" {
		t.Fatalf("planned = %#v", planned)
	}
	var oldExists, newExists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass('app.accounts') IS NOT NULL, to_regclass('app.customers') IS NOT NULL`).Scan(&oldExists, &newExists); err != nil {
		t.Fatal(err)
	}
	if !oldExists || newExists {
		t.Fatalf("dev plan applied SQL: old=%t new=%t", oldExists, newExists)
	}
	for _, statement := range planned.Statements {
		if _, err := connection.Exec(context.Background(), statement.SQL); err != nil {
			t.Fatalf("deliberate local application: %v", err)
		}
	}
	residualOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if residualOutput.code != 0 {
		t.Fatalf("residual exit = %d, stdout = %s", residualOutput.code, residualOutput.stdout)
	}
	var residual protocol.Result
	if err := json.Unmarshal([]byte(residualOutput.stdout), &residual); err != nil {
		t.Fatal(err)
	}
	if residual.Status != protocol.Planned || len(residual.Statements) != 0 || residual.CurrentFingerprint != residual.DesiredFingerprint {
		t.Fatalf("residual = %#v", residual)
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

	git(t, repository, "add", "onward-bundles/primary/projects")
	git(t, repository, "commit", "-m", "add projects migration bundle")
	ciOutput := captureStdout(t, func() int {
		return runCIAt([]string{"check", "--base", "main", "--target", "primary", "--bundle", "projects"}, repository)
	})
	if ciOutput.code != 0 {
		t.Fatalf("CI exit = %d, stdout = %s", ciOutput.code, ciOutput.stdout)
	}
	var ci ciReport
	if err := json.Unmarshal([]byte(ciOutput.stdout), &ci); err != nil {
		t.Fatal(err)
	}
	if ci.ProtocolVersion != ciVersion || ci.Outcome != "passed" || ci.PRStatus == nil || ci.PRStatus.Freshness == nil || ci.PRStatus.Freshness.Outcome != "fresh" || ci.Verification == nil || ci.Verification.Outcome != "verified" {
		t.Fatalf("CI report = %#v", ci)
	}
	if len(ci.PRStatus.Freshness.Notices) != 1 || ci.PRStatus.Freshness.Notices[0].Code != "provenance_changed" {
		t.Fatalf("CI no-op receipt commit notice = %#v", ci.PRStatus.Freshness)
	}

}

func TestCICheckRejectsStackedBundleDirectoriesBeforeDatabaseUse(t *testing.T) {
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
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE example (id bigint);\n")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base")
	git(t, repository, "checkout", "-b", "feature")
	writeTestFile(t, repository, "onward-bundles/primary/feature-a/receipt", "a\n")
	writeTestFile(t, repository, "onward-bundles/primary/feature-b/receipt", "b\n")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "stacked migrations")

	output := captureStdout(t, func() int {
		return runCIAt([]string{"check", "--base", "main", "--target", "primary", "--bundle", "feature-a"}, repository)
	})
	if output.code != 4 {
		t.Fatalf("CI exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report ciReport
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Code != "incorrect_bundle_stack" {
		t.Fatalf("CI report = %#v", report)
	}
}

func TestPRStatusClassifiesWouldBeMergeConflict(t *testing.T) {
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
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE example (value text);\n")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base")
	git(t, repository, "checkout", "-b", "feature")
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE example (value bigint);\n")
	git(t, repository, "add", "schema.sql")
	git(t, repository, "commit", "-m", "feature type")
	git(t, repository, "checkout", "main")
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE example (value integer);\n")
	git(t, repository, "add", "schema.sql")
	git(t, repository, "commit", "-m", "main type")
	git(t, repository, "checkout", "feature")

	output := captureStdout(t, func() int {
		return runPRAt([]string{"status", "--base", "main", "--target", "primary"}, repository)
	})
	if output.code != 4 {
		t.Fatalf("status exit = %d, stdout = %s", output.code, output.stdout)
	}
	var status gitbase.Status
	if err := json.Unmarshal([]byte(output.stdout), &status); err != nil {
		t.Fatal(err)
	}
	if status.Outcome != "conflicting" || len(status.Problems) != 1 || status.Problems[0].Code != "merge_conflict" {
		t.Fatalf("status = %#v", status)
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

func TestDeveloperPreviewLifecycleRestacksOneBundleAcrossTwoBaseMigrations(t *testing.T) {
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
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.events (id bigint, occurred_on date);\n"
	writeTestFile(t, repository, "schema/00_base.sql", baseDDL)
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base schema")

	git(t, repository, "checkout", "-b", "feature")
	featureDDL := "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.events (id bigint, occurred_on date NOT NULL);\n"
	writeTestFile(t, repository, "schema/00_base.sql", featureDDL)
	git(t, repository, "add", "schema/00_base.sql")
	git(t, repository, "commit", "-m", "feature schema")
	bundleArguments := []string{"regenerate", "--base", "main", "--target", "primary", "--bundle", "customer-dates"}
	regenerate := func(extra ...string) (captured, prflow.Analysis) {
		arguments := append(append([]string(nil), bundleArguments...), extra...)
		output := captureStdout(t, func() int { return runPRAt(arguments, repository) })
		var analysis prflow.Analysis
		if err := json.Unmarshal([]byte(output.stdout), &analysis); err != nil {
			t.Fatalf("decode regenerate output %s: %v", output.stdout, err)
		}
		return output, analysis
	}
	answerPath := filepath.Join(t.TempDir(), "answers.json")
	var answers protocol.Answers
	writeAnswers := func() {
		data, err := json.MarshalIndent(answers, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(answerPath, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, pendingRename := regenerate()
	if first.code != 2 || pendingRename.Plan == nil || len(pendingRename.Plan.Questions) != 1 || pendingRename.Plan.Questions[0].Kind != "rename_table" {
		t.Fatalf("rename decision = code %d, %#v", first.code, pendingRename)
	}
	answers = protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pendingRename.Plan.CurrentFingerprint, DesiredFingerprint: pendingRename.Plan.DesiredFingerprint}
	rename := pendingRename.Plan.Questions[0]
	answers.Answers = append(answers.Answers, protocol.Answer{Kind: rename.Kind, Key: rename.Key, Value: rename.Choices[0], QuestionFingerprint: rename.ScopeFingerprint})
	writeAnswers()
	second, pendingStrategy := regenerate("--answers", answerPath, "--replace-draft")
	if second.code != 2 || pendingStrategy.Plan == nil || len(pendingStrategy.Plan.Questions) != 1 || pendingStrategy.Plan.Questions[0].Kind != "set_not_null" {
		t.Fatalf("strategy decision = code %d, %#v", second.code, pendingStrategy)
	}
	strategy := pendingStrategy.Plan.Questions[0]
	answers.Answers = append(answers.Answers, protocol.Answer{Kind: strategy.Kind, Key: strategy.Key, Value: "staged_with_backfill", QuestionFingerprint: strategy.ScopeFingerprint})
	writeAnswers()
	third, pendingBackfill := regenerate("--answers", answerPath, "--replace-draft")
	if third.code != 2 || pendingBackfill.Plan == nil || len(pendingBackfill.Plan.Questions) != 1 || pendingBackfill.Plan.Questions[0].Kind != "backfill_not_null" {
		t.Fatalf("backfill decision = code %d, %#v", third.code, pendingBackfill)
	}
	backfill := pendingBackfill.Plan.Questions[0]
	answers.Answers = append(answers.Answers, protocol.Answer{
		Kind: backfill.Kind, Key: backfill.Key, Value: "provided", QuestionFingerprint: backfill.ScopeFingerprint,
		Manual: &protocol.ManualWork{
			Summary: "fill missing event dates from reviewed application policy", ExecutionMode: "transactional",
			Statements:      []string{`UPDATE "app"."events" SET "occurred_on" = CURRENT_DATE WHERE "occurred_on" IS NULL;`},
			VerificationSQL: []string{`SELECT count(*) = 0 FROM "app"."events" WHERE "occurred_on" IS NULL;`},
		},
	})
	writeAnswers()
	readyOutput, ready := regenerate("--answers", answerPath, "--replace-draft")
	if readyOutput.code != 0 || ready.Outcome != "ready" || ready.Plan == nil {
		t.Fatalf("ready = code %d, %#v", readyOutput.code, ready)
	}

	// The feature keeps evolving, but regeneration replaces the same logical
	// bundle and carries only still-valid scoped decisions.
	writeTestFile(t, repository, "schema/20_feature.sql", "CREATE TABLE app.customer_notes (id bigint, body text);\n")
	regeneratedOutput, regenerated := regenerate("--replace-draft")
	if regeneratedOutput.code != 0 || regenerated.Rebind == nil || len(regenerated.Rebind.Carried) != 3 {
		t.Fatalf("feature regeneration = code %d, %#v", regeneratedOutput.code, regenerated)
	}
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "feature schema and logical migration")

	// Two unrelated migrations land on main while the feature remains open.
	for index, erosion := range []struct {
		id   string
		file string
		ddl  string
	}{{"add-audit", "schema/10_audit.sql", "CREATE TABLE app.audit_log (id bigint);\n"}, {"add-teams", "schema/11_teams.sql", "CREATE TABLE app.teams (id bigint);\n"}} {
		git(t, repository, "checkout", "main")
		writeTestFile(t, repository, erosion.file, erosion.ddl)
		mainDDL := baseDDL + "CREATE TABLE app.audit_log (id bigint);\n"
		if index == 1 {
			mainDDL += "CREATE TABLE app.teams (id bigint);\n"
		}
		writeHistoryTransitionFixture(t, repository, url, erosion.id, mainDDL)
		git(t, repository, "add", "-A")
		git(t, repository, "commit", "-m", erosion.id)
		git(t, repository, "checkout", "feature")
		git(t, repository, "merge", "main", "--no-edit")
		restackedOutput, restacked := regenerate("--replace-draft")
		if restackedOutput.code != 0 || restacked.Outcome != "ready" || restacked.Rebind == nil || len(restacked.Rebind.Carried) != 3 || len(restacked.Rebind.Invalidated) != 0 {
			t.Fatalf("restack %d = code %d, output %s", index+1, restackedOutput.code, restackedOutput.stdout)
		}
		git(t, repository, "add", "-A")
		git(t, repository, "commit", "-m", "restack logical migration")
	}

	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 4 || chain.Entries[len(chain.Entries)-1].Directory != "customer-dates" {
		t.Fatalf("history = %#v", chain)
	}
	artifact := chain.Entries[len(chain.Entries)-1].Artifact
	if artifact.Manifest.Generation < 4 || artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest != chain.Entries[len(chain.Entries)-2].Artifact.Manifest.History.EntryDigest {
		t.Fatalf("final bundle manifest = %#v", artifact.Manifest)
	}
	if _, exists := artifact.Manifest.Phases["manual"]; !exists {
		t.Fatalf("final bundle has no manual backfill phase: %#v", artifact.Manifest.Phases)
	}
	if _, exists := artifact.Manifest.Phases["contract"]; !exists {
		t.Fatalf("final bundle has no contract phase: %#v", artifact.Manifest.Phases)
	}

	verificationOutput := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "customer-dates"}, repository)
	})
	if verificationOutput.code != 0 {
		t.Fatalf("clone verification = %d, %s", verificationOutput.code, verificationOutput.stdout)
	}
	ciOutput := captureStdout(t, func() int {
		return runCIAt([]string{"check", "--base", "main", "--target", "primary", "--bundle", "customer-dates"}, repository)
	})
	if ciOutput.code != 0 {
		t.Fatalf("CI = %d, %s", ciOutput.code, ciOutput.stdout)
	}
	writeTestFile(t, repository, "schema/99_uncommitted.sql", "CREATE TABLE app.uncommitted_work (id bigint);\n")
	dirtyCI := captureStdout(t, func() int {
		return runCIAt([]string{"check", "--base", "main", "--target", "primary", "--bundle", "customer-dates"}, repository)
	})
	if dirtyCI.code != 4 || !strings.Contains(dirtyCI.stdout, `"code":"dirty_working_tree"`) {
		t.Fatalf("dirty CI = %d, %s", dirtyCI.code, dirtyCI.stdout)
	}
}

func TestBundleVerifyReportsPartialResidualAndFullConvergenceOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
postgres_major = 16
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.obsolete (id bigint PRIMARY KEY);\n"
	writeTestFile(t, repository, "schema.sql", "")
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	writeHistoryContractDropFixture(t, repository, url, "remove-obsolete")

	before := disposableDatabaseCount(t, url)
	partial := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete", "--through", "expand"}, repository)
	})
	if partial.code != 4 {
		t.Fatalf("partial exit = %d, stdout = %s", partial.code, partial.stdout)
	}
	var partialReport verify.Report
	if err := json.Unmarshal([]byte(partial.stdout), &partialReport); err != nil {
		t.Fatal(err)
	}
	if partialReport.Outcome != "residual" || partialReport.Residual == nil || partialReport.Residual.Status != protocol.NeedsInput {
		t.Fatalf("partial report = %#v", partialReport)
	}

	full := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete"}, repository)
	})
	if full.code != 0 {
		t.Fatalf("full exit = %d, stdout = %s", full.code, full.stdout)
	}
	var fullReport verify.Report
	if err := json.Unmarshal([]byte(full.stdout), &fullReport); err != nil {
		t.Fatal(err)
	}
	if fullReport.ProtocolVersion != verify.Version || fullReport.Outcome != "verified" || fullReport.ObservedFingerprint != fullReport.DesiredFingerprint || fullReport.Residual == nil || len(fullReport.Residual.Statements) != 0 {
		t.Fatalf("full report = %#v", fullReport)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("disposable database count = %d, want %d", after, before)
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

func writeHistoryContractDropFixture(t *testing.T, root, devURL, id string) {
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
	desired, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-desired", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) == 0 {
		t.Fatalf("drop planning did not ask for destructive intent: %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint,
		DesiredFingerprint: pending.DesiredFingerprint,
	}
	for _, question := range pending.Questions {
		if len(question.Choices) == 0 {
			t.Fatalf("drop question has no explicit choice: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{
			Kind: question.Kind, Key: question.Key, Value: question.Choices[0],
			QuestionFingerprint: question.ScopeFingerprint,
		})
	}
	result, err := graphplan.Build(current, desired, answers, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned {
		t.Fatalf("drop result = %#v", result)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "contract", Mode: "pr",
		BaseRef: "origin/main", BaseCommit: strings.Repeat("e", 40), HeadRevision: strings.Repeat("f", 40),
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: 16},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "empty fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: 16},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result, Answers: &answers})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func disposableDatabaseCount(t *testing.T, url string) int {
	t.Helper()
	connection, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var count int
	if err := connection.QueryRow(context.Background(), `SELECT count(*) FROM pg_database WHERE datname LIKE 'onwardpg_verify_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func createTestDatabase(t *testing.T, adminURL string) (string, func()) {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "onwardpg_dev_test_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+`"`+name+`"`); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String(), func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+`"`+name+`" WITH (FORCE)`)
		_ = admin.Close(context.Background())
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
