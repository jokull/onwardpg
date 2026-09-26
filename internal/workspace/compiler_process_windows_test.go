//go:build windows

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompilerWindowsFastExit(t *testing.T) {
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	for index := 0; index < 20; index++ {
		artifact, err := CompileDDL(context.Background(), t.TempDir(), "primary", Target{
			SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "tiny"},
		})
		if err != nil {
			t.Fatalf("fast exporter run %d: %v", index, err)
		}
		if string(artifact.DDL) != "tiny" {
			t.Fatalf("fast exporter run %d returned %q", index, artifact.DDL)
		}
	}
}

func TestCompilerWindowsStopsChildWriter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	t.Setenv("ONWARDPG_COMPILER_MARKER", marker)
	start := time.Now()
	_, err := CompileDDL(context.Background(), t.TempDir(), "primary", Target{
		SchemaCommand: []string{os.Args[0], "-test.run=^TestCompilerHelperProcess$", "--", "spawn-oversize-child"},
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

func TestCompilerWindowsStopsDescendantAfterParentExits(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	t.Setenv("ONWARDPG_COMPILER_HELPER", "1")
	t.Setenv("ONWARDPG_COMPILER_MARKER", marker)
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
