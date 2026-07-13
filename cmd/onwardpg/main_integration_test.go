package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
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
