package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSortedSQLHistoryIsDeterministic(t *testing.T) {
	root := t.TempDir()
	target := Target{SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
	writeWorkspaceFile(t, root, "migrations/002.sql", "SELECT 2;\n")
	writeWorkspaceFile(t, root, "migrations/001.sql", "SELECT 1;\n")
	history, err := LoadMigrationHistory(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(history.DDL), "SELECT 1") > strings.Index(string(history.DDL), "SELECT 2") {
		t.Fatalf("history order = %s", history.DDL)
	}
}

func TestLoadSortedSQLHistoryRejectsAmbiguousNumericOrder(t *testing.T) {
	root := t.TempDir()
	target := Target{SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
	writeWorkspaceFile(t, root, "migrations/2_second.sql", "SELECT 2;\n")
	writeWorkspaceFile(t, root, "migrations/10_tenth.sql", "SELECT 10;\n")
	if _, err := LoadMigrationHistory(root, target); err == nil || !strings.Contains(err.Error(), "zero-padded") {
		t.Fatalf("expected ambiguous numeric history rejection, got %v", err)
	}
}

func writeWorkspaceFile(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
