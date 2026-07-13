package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/adapter"
)

func TestTargetCompilerReadsSchemaFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("CREATE TABLE users (id bigint);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compiler := TargetCompiler{TargetName: "primary", Target: Target{
		Adapter: "ddl", SchemaFile: "schema.sql", MigrationPath: "migrations",
		DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16,
	}}
	artifact, err := compiler.Compile(context.Background(), adapter.CompileRequest{Root: root, Target: "primary", Revision: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.DDL) != "CREATE TABLE users (id bigint);\n" || artifact.Provenance != "ddl:schema.sql" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestTargetCompilerCapturesCommandStdout(t *testing.T) {
	root := t.TempDir()
	compiler := TargetCompiler{TargetName: "primary", Target: Target{
		Adapter: "custom", SchemaCommand: []string{"printf", "CREATE TABLE users (id bigint);\\n"},
		MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16,
	}}
	artifact, err := compiler.Compile(context.Background(), adapter.CompileRequest{Root: root, Target: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifact.DDL), "CREATE TABLE users") || artifact.Provenance != "custom:command" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestTargetCompilerRejectsUndeclaredCommandOutputs(t *testing.T) {
	root := t.TempDir()
	compiler := TargetCompiler{TargetName: "primary", Target: Target{
		Adapter: "custom", SchemaCommand: []string{"sh", "-c", "mkdir cache && printf 'CREATE TABLE users (id bigint);'"},
		MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16,
	}}
	_, err := compiler.Compile(context.Background(), adapter.CompileRequest{Root: root, Target: "primary"})
	if err == nil || !strings.Contains(err.Error(), "modified its isolated input tree") {
		t.Fatalf("expected undeclared output rejection, got %v", err)
	}
}
