package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDrizzleHistoryUsesJournalOrder(t *testing.T) {
	root := t.TempDir()
	target := Target{Adapter: "drizzle", SchemaFile: "schema.sql", MigrationPath: "drizzle", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
	writeWorkspaceFile(t, root, "drizzle/meta/_journal.json", `{"dialect":"postgresql","entries":[{"idx":0,"tag":"0000_first"},{"idx":1,"tag":"0001_second"}]}`)
	writeWorkspaceFile(t, root, "drizzle/0000_first.sql", "SELECT 'first';\n")
	writeWorkspaceFile(t, root, "drizzle/0001_second.sql", "SELECT 'second';\n")
	history, err := LoadMigrationHistory(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Files) != 2 || strings.Index(string(history.DDL), "first") > strings.Index(string(history.DDL), "second") || history.Digest == "" {
		t.Fatalf("history = %#v", history)
	}
}

func TestLoadDrizzleHistoryRejectsUnjournaledSQL(t *testing.T) {
	root := t.TempDir()
	target := Target{Adapter: "drizzle", SchemaFile: "schema.sql", MigrationPath: "drizzle", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
	writeWorkspaceFile(t, root, "drizzle/meta/_journal.json", `{"dialect":"postgresql","entries":[]}`)
	writeWorkspaceFile(t, root, "drizzle/untracked.sql", "SELECT 1;\n")
	if _, err := LoadMigrationHistory(root, target); err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("expected unjournaled SQL rejection, got %v", err)
	}
}

func TestLoadSortedSQLHistoryIsDeterministic(t *testing.T) {
	root := t.TempDir()
	target := Target{Adapter: "ddl", SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
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
