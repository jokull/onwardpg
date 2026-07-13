package prflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/adapter"
	"github.com/jokull/onwardpg/internal/gitbase"
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
	repositoryRoot := t.TempDir()
	git(t, repositoryRoot, "init", "-b", "main")
	git(t, repositoryRoot, "config", "user.name", "Onward Test")
	git(t, repositoryRoot, "config", "user.email", "onward@example.test")
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint PRIMARY KEY);\n"
	write(t, repositoryRoot, "schema.sql", baseDDL)
	write(t, repositoryRoot, "migrations/0001.sql", baseDDL)
	commit(t, repositoryRoot, "base")
	git(t, repositoryRoot, "checkout", "-b", "feature")
	write(t, repositoryRoot, "schema.sql", baseDDL+"CREATE TABLE app.projects (id bigint PRIMARY KEY);\n")
	commit(t, repositoryRoot, "feature schema")

	repository, err := gitbase.Open(context.Background(), repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	target := workspace.Target{
		Adapter: "ddl", SchemaFile: "schema.sql", MigrationPath: "migrations",
		DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16,
	}
	analysis, err := Analyze(context.Background(), Input{
		Repository: repository, TargetName: "primary", Target: target,
		BaseRef: "main", HeadRef: "HEAD", IncludeWorkingTree: true, DevDatabaseURL: devURL,
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
	repositoryRoot := t.TempDir()
	git(t, repositoryRoot, "init", "-b", "main")
	git(t, repositoryRoot, "config", "user.name", "Onward Test")
	git(t, repositoryRoot, "config", "user.email", "onward@example.test")
	write(t, repositoryRoot, "schema.sql", "CREATE TABLE users (id bigint);\n")
	write(t, repositoryRoot, "migrations/0001.sql", "CREATE TABLE accounts (id bigint);\n")
	commit(t, repositoryRoot, "inconsistent base")

	repository, err := gitbase.Open(context.Background(), repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	target := workspace.Target{Adapter: "ddl", SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16}
	analysis, err := Analyze(context.Background(), Input{
		Repository: repository, TargetName: "primary", Target: target,
		BaseRef: "main", HeadRef: "HEAD", IncludeWorkingTree: true, DevDatabaseURL: devURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Outcome != "blocked" || analysis.SchemaSquare.BaseIntegrity != "mismatched" || len(analysis.Problems) != 1 || analysis.Problems[0].Code != "base_integrity_mismatch" {
		t.Fatalf("analysis = %#v", analysis)
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

func commit(t *testing.T, root, message string) {
	t.Helper()
	git(t, root, "add", "-A")
	git(t, root, "commit", "-m", message)
}

func git(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}
