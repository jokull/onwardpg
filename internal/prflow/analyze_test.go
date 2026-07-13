package prflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/adapter"
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
	write(t, baseRoot, "migrations/0001.sql", baseDDL)
	write(t, headRoot, "schema.sql", baseDDL+"CREATE TABLE app.projects (id bigint PRIMARY KEY);\n")
	target := workspace.Target{
		SchemaFile: "schema.sql", MigrationPath: "migrations",
		DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16,
	}
	analysis, err := Analyze(context.Background(), Input{
		BaseRoot: baseRoot, HeadRoot: headRoot, BaseRevision: "base-tree", HeadRevision: "head-tree",
		TargetName: "primary", Target: target, DevDatabaseURL: devURL,
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
	write(t, baseRoot, "migrations/0001.sql", "CREATE TABLE accounts (id bigint);\n")
	write(t, headRoot, "schema.sql", "CREATE TABLE users (id bigint);\n")
	target := workspace.Target{SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "ONWARDPG_TEST_DATABASE_URL", PostgresMajor: 16}
	analysis, err := Analyze(context.Background(), Input{
		BaseRoot: baseRoot, HeadRoot: headRoot, BaseRevision: "base-tree", HeadRevision: "head-tree",
		TargetName: "primary", Target: target, DevDatabaseURL: devURL,
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
