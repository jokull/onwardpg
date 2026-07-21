package testkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsTrailingEnvelope(t *testing.T) {
	result := CommandResult{Stdout: "{\"status\":\"ready\"}\n{\"status\":\"stale\"}\n"}
	var destination map[string]any
	if err := result.DecodeJSON(&destination); err == nil || !strings.Contains(err.Error(), "trailing value") {
		t.Fatalf("DecodeJSON trailing error = %v", err)
	}
}

func TestDecodeJSONRedactsMalformedOutput(t *testing.T) {
	const secret = "postgres://owner:secret@example.invalid/database"
	result := CommandResult{Stdout: "not JSON " + secret, Stderr: "failed near " + secret, redactions: []string{secret}}
	var destination map[string]any
	err := result.DecodeJSON(&destination)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("unsafe decode error: %v", err)
	}
}

func TestCommandTranscriptRedactsEnvironmentValuesEverywhere(t *testing.T) {
	artifactRoot := t.TempDir()
	t.Setenv("ONWARDPG_ACCEPTANCE_ARTIFACT_DIR", artifactRoot)
	const secret = "postgres://owner:secret@example.invalid/database"
	result := (Binary{Path: "/bin/sh"}).Run(context.Background(), t.TempDir(), map[string]string{"DATABASE_URL": secret},
		"-c", `printf '%s\n' "$DATABASE_URL"; printf '%s\n' "$DATABASE_URL" >&2`, secret)
	if result.ExitCode != 0 {
		t.Fatal(result.Failure())
	}
	if strings.Contains(result.Failure(), secret) || !strings.Contains(result.Failure(), "[REDACTED]") {
		t.Fatalf("unsafe failure rendering:\n%s", result.Failure())
	}
	files, err := filepath.Glob(filepath.Join(artifactRoot, "command-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("transcript files = %v, err=%v", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) || !strings.Contains(string(body), "[REDACTED]") || !strings.Contains(string(body), "DATABASE_URL") {
		t.Fatalf("unsafe transcript:\n%s", body)
	}
}

func TestRecordWorkspaceCopiesOnlyOnExplicitArtifactOptIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "migrations", "onward"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "migrations", "onward", "phase.sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactRoot := t.TempDir()
	t.Setenv("ONWARDPG_ACCEPTANCE_ARTIFACT_DIR", artifactRoot)
	if err := RecordWorkspace(root, "Scenario / One"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(artifactRoot, "workspaces", "scenario___one", "migrations", "onward", "phase.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "SELECT 1;\n" {
		t.Fatalf("copied phase = %q", body)
	}
}
