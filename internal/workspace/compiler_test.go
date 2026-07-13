package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTargetCompilerReadsSchemaFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("CREATE TABLE users (id bigint);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := Target{
		SchemaFile:     "schema.sql",
		DevDatabaseEnv: "DEV_DATABASE_URL",
	}
	artifact, err := CompileDDL(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.DDL) != "CREATE TABLE users (id bigint);\n" || artifact.Provenance != "schema_file:schema.sql" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestTargetCompilerCapturesCommandStdout(t *testing.T) {
	root := t.TempDir()
	target := Target{
		SchemaCommand:  []string{"printf", "CREATE TABLE users (id bigint);\\n"},
		DevDatabaseEnv: "DEV_DATABASE_URL",
	}
	artifact, err := CompileDDL(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifact.DDL), "CREATE TABLE users") || artifact.Provenance != "schema_command" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestTargetCompilerAcceptsAnEmptyDesiredSchema(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fileArtifact, err := CompileDDL(context.Background(), root, "file", Target{
		SchemaFile: "schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL",
	})
	if err != nil {
		t.Fatal(err)
	}
	commandArtifact, err := CompileDDL(context.Background(), root, "command", Target{
		SchemaCommand: []string{"sh", "-c", "true"}, DevDatabaseEnv: "DEV_DATABASE_URL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fileArtifact.DDL) != 0 || len(commandArtifact.DDL) != 0 {
		t.Fatalf("file=%q command=%q", fileArtifact.DDL, commandArtifact.DDL)
	}
}

func TestTargetCompilerRejectsUndeclaredCommandOutputs(t *testing.T) {
	root := t.TempDir()
	target := Target{
		SchemaCommand:  []string{"sh", "-c", "mkdir cache && printf 'CREATE TABLE users (id bigint);'"},
		DevDatabaseEnv: "DEV_DATABASE_URL",
	}
	_, err := CompileDDL(context.Background(), root, "primary", target)
	if err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("expected undeclared output rejection, got %v", err)
	}
}
