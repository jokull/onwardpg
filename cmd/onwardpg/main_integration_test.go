package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/prflow"
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
migration_path = "migrations"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
postgres_major = 16
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)
	writeTestFile(t, repository, "migrations/0001.sql", baseDDL)
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
	if artifact.Manifest.SchemaSquare == nil || artifact.Manifest.SchemaSquare.BaseCodeFingerprint != artifact.Manifest.SchemaSquare.BaseHistoryFingerprint || artifact.Manifest.SchemaSquare.HeadHistoryFidelity != "not_replayed" {
		t.Fatalf("bundle schema square = %#v", artifact.Manifest.SchemaSquare)
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
