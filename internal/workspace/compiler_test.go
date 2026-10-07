package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTargetCompilerGivesExportCommandARegularStdoutFile(t *testing.T) {
	root := t.TempDir()
	target := Target{
		SchemaCommand:  []string{"sh", "-c", "test -f /dev/stdout && printf 'CREATE TABLE users (id bigint);\\n'"},
		DevDatabaseEnv: "DEV_DATABASE_URL",
	}
	artifact, err := CompileDDL(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifact.DDL), "CREATE TABLE users") {
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

func TestCompilerRejectsOversizedSchemaFile(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxCompilerOutput + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = CompileDDL(context.Background(), root, "primary", Target{SchemaFile: "schema.sql"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected schema file size rejection, got %v", err)
	}
}

func TestCompilerRejectsOversizedRunningExporter(t *testing.T) {
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	start := time.Now()
	_, err := CompileDDL(context.Background(), t.TempDir(), "primary", Target{
		SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "oversize-and-wait"},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected running exporter size rejection, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("oversized exporter was not stopped while it was running")
	}
}

func TestCompilerCancelsRunningExporter(t *testing.T) {
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := CompileDDL(ctx, t.TempDir(), "primary", Target{
		SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "wait"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller deadline, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancelled exporter did not stop promptly")
	}
}

func TestCompilerCapturesLargeRegularFileWithoutTruncation(t *testing.T) {
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	root := t.TempDir()
	artifact, err := CompileDDL(context.Background(), root, "primary", Target{
		SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "large"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.DDL) != 8<<20 || artifact.DDL[0] != 'x' || artifact.DDL[len(artifact.DDL)-1] != 'x' {
		t.Fatalf("large export was truncated: %d bytes", len(artifact.DDL))
	}
}

func TestCompilerHelperProcess(t *testing.T) {
	if os.Getenv("ONWARDPG_COMPILER_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	chunk := make([]byte, 32<<10)
	for index := range chunk {
		chunk[index] = 'x'
	}
	write := func(size int) {
		for size > 0 {
			part := min(size, len(chunk))
			if _, err := os.Stdout.Write(chunk[:part]); err != nil {
				os.Exit(2)
			}
			size -= part
		}
	}
	switch mode {
	case "tiny":
		_, _ = io.WriteString(os.Stdout, "tiny")
	case "large":
		write(8 << 20)
	case "oversize-and-wait":
		write(maxCompilerOutput + 1)
		time.Sleep(30 * time.Second)
	case "wait":
		_, _ = io.WriteString(os.Stdout, "ready")
		time.Sleep(30 * time.Second)
	case "spawn-mark-child-and-exit", "spawn-oversize-child":
		childMode := "mark-after-wait"
		if mode == "spawn-oversize-child" {
			childMode = "oversize-child-mark"
		}
		child := exec.Command(os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", childMode)
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if mode == "spawn-oversize-child" {
			if err := child.Wait(); err != nil {
				os.Exit(2)
			}
		} else {
			_, _ = io.WriteString(os.Stdout, "stable")
		}
	case "mark-after-wait":
		time.Sleep(time.Second)
		if err := os.WriteFile(os.Getenv("ONWARDPG_COMPILER_MARKER"), []byte("survived"), 0o600); err != nil {
			os.Exit(2)
		}
	case "oversize-child-mark":
		write(maxCompilerOutput + 1)
		time.Sleep(time.Second)
		if err := os.WriteFile(os.Getenv("ONWARDPG_COMPILER_MARKER"), []byte("survived"), 0o600); err != nil {
			os.Exit(2)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
