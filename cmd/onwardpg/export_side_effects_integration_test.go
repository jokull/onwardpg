package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
)

// A schema command that writes a tracked file and prints the same DDL each
// time does not stop a command. Every command that runs the export reports
// the changed path as a warning, and never reports the files that onwardpg
// itself writes.
func TestExportSideEffectsAreWarningsOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repository := t.TempDir()
	git := func(arguments ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{
			"-C", repository, "-c", "user.name=onwardpg", "-c", "user.email=onwardpg@example.com", "-c", "commit.gpgsign=false",
		}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
	}
	commit := func() {
		t.Helper()
		git("add", "--all")
		git("commit", "--quiet", "--allow-empty", "--message=state")
	}
	configuration := func(command string) string {
		return `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_command = ["sh", "-c", "` + command + `"]
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`
	}
	git("init", "--quiet", "--initial-branch=main")
	writeTestFile(t, repository, ".gitignore", "build/\n")
	writeTestFile(t, repository, ".onwardpg.toml", configuration("printf x >> generated.txt; mkdir -p build; date > build/cache; cat schema.sql"))
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.items (id bigint PRIMARY KEY);\n")
	writeTestFile(t, repository, "generated.txt", "")
	commit()

	warned := func(name string, wantCode int, call func() int) {
		t.Helper()
		output := captureStdout(t, call)
		if output.code != wantCode {
			t.Fatalf("%s = %d, want %d: %s", name, output.code, wantCode, output.stdout)
		}
		var result struct {
			Warnings []protocol.Warning `json:"warnings"`
		}
		if err := json.Unmarshal([]byte(output.stdout), &result); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, output.stdout)
		}
		if len(result.Warnings) != 1 || result.Warnings[0].Code != protocol.ExportSideEffectsCode || !slices.Equal(result.Warnings[0].Paths, []string{"generated.txt"}) {
			t.Fatalf("%s warnings = %#v, want one export_side_effects warning for generated.txt\n%s", name, result.Warnings, output.stdout)
		}
	}
	configPath := filepath.Join(repository, ".onwardpg.toml")
	warned("config check", 0, func() int { return runConfig([]string{"check", "--config", configPath}) })
	commit()
	warned("init", 0, func() int { return runInitAt([]string{"--target", "primary"}, repository) })
	commit()
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.items (id bigint PRIMARY KEY, note text);\n")
	commit()
	warned("plan", 0, func() int { return runWorkflowPlanAt([]string{"add-note", "--target", "primary"}, repository) })
	commit()
	warned("verify", 0, func() int { return runVerifyAt([]string{"--target", "primary", "--bundle", "add-note"}, repository) })
	commit()
	warned("verify --check", 0, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "add-note", "--check"}, repository)
	})

	// A read-only export has nothing to report.
	writeTestFile(t, repository, ".onwardpg.toml", configuration("cat schema.sql"))
	commit()
	for name, call := range map[string]func() int{
		"config check": func() int { return runConfig([]string{"check", "--config", configPath}) },
		"verify --check": func() int {
			return runVerifyAt([]string{"--target", "primary", "--bundle", "add-note", "--check"}, repository)
		},
	} {
		output := captureStdout(t, call)
		if output.code != 0 || strings.Contains(output.stdout, `"warnings"`) {
			t.Fatalf("%s with a read-only export = %d, %s", name, output.code, output.stdout)
		}
	}
}
