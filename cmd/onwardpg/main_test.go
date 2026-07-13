package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

func TestSourceReceiptRedactsDatabaseURLAndAbsoluteDDLPath(t *testing.T) {
	database := sourceReceipt(source.Parse("postgres://user:secret@db.example/app"), "sha256:"+repeat("a", 64), "current")
	data, err := json.Marshal(database)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(data), "secret") || contains(string(data), "db.example") || database.Description != "current database" {
		t.Fatalf("database receipt leaked source URL: %s", data)
	}
	ddl := sourceReceipt(source.Parse("file:///private/work/schema.sql"), "sha256:"+repeat("b", 64), "desired")
	if ddl.Description != "desired DDL schema.sql" {
		t.Fatalf("DDL description = %q", ddl.Description)
	}
}

func TestVersionedDiagnosticContract(t *testing.T) {
	diagnostic := protocol.ErrorDiagnostic("invalid_invocation", errors.New("bad flags"))
	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), protocol.DiagnosticVersion) || !contains(string(data), `"code":"invalid_invocation"`) {
		t.Fatalf("diagnostic = %s", data)
	}
}

func TestPRStatusCLIBlocksBaseMigrationEdits(t *testing.T) {
	repository := t.TempDir()
	git(t, repository, "init", "-b", "main")
	git(t, repository, "config", "user.name", "Onward Test")
	git(t, repository, "config", "user.email", "onward@example.test")
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
postgres_major = 16
`)
	writeTestFile(t, repository, "schema.sql", "CREATE TABLE users (id bigint);\n")
	writeTestFile(t, repository, "onward-bundles/primary/base/plan.json", "SELECT 1;\n")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "base")
	git(t, repository, "checkout", "-b", "feature")
	writeTestFile(t, repository, "onward-bundles/primary/base/plan.json", "SELECT 2;\n")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-m", "edit history")

	output := captureStdout(t, func() int {
		return runPRAt([]string{"status", "--base", "main", "--target", "primary"}, repository)
	})
	if output.code != 4 {
		t.Fatalf("exit = %d, stdout = %s", output.code, output.stdout)
	}
	var status gitbase.Status
	if err := json.Unmarshal([]byte(output.stdout), &status); err != nil {
		t.Fatal(err)
	}
	if status.ProtocolVersion != gitbase.Version || status.Outcome != "blocked" || len(status.Problems) != 1 || status.Problems[0].Code != "base_history_modified" {
		t.Fatalf("status = %#v", status)
	}
}

func writeTestFile(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
