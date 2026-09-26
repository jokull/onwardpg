//go:build darwin || linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCompilerStopsChildWriterAfterOversize(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	root := t.TempDir()
	start := time.Now()
	_, err := CompileDDL(context.Background(), root, "primary", Target{
		SchemaCommand: []string{
			"sh", "-c", "(head -c 67108865 /dev/zero; sleep 1; echo survived > \"$1\") & wait", "sh", marker,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected child writer size rejection, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("oversized child writer was not stopped promptly")
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child writer continued after exporter termination: %v", err)
	}
}

func TestCompilerRejectsSchemaFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "schema.sql"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := CompileDDL(context.Background(), root, "primary", Target{SchemaFile: "schema.sql"})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected FIFO rejection, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("schema FIFO inspection blocked")
	}
}

func TestCompilerStopsDescendantAfterParentExits(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	t.Setenv("ONWARDPG_COMPILER_MARKER", marker)
	// Race-instrumented helper processes otherwise pause for one second at
	// os.Exit, long enough for the descendant to write before its parent exits.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	artifact, err := CompileDDL(context.Background(), t.TempDir(), "primary", Target{
		SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "spawn-mark-child-and-exit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.DDL) != "stable" {
		t.Fatalf("unexpected exporter output %q", artifact.DDL)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant continued after its parent exited: %v", err)
	}
}

func TestCompilerEarlyCancellationHasNoGroupRace(t *testing.T) {
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	for index := 0; index < 32; index++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Microsecond)
			cancel()
		}()
		_, err := CompileDDL(ctx, t.TempDir(), "primary", Target{
			SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "wait"},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("early cancellation run %d: %v", index, err)
		}
	}
}
