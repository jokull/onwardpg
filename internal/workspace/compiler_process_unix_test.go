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

func TestOpenListedRegularFileReturnsOnlyRegularFiles(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("content"), 0o640); err != nil {
		t.Fatal(err)
	}
	file, info, ok := openListedRegularFile(regular)
	if !ok {
		t.Fatal("a regular file was refused")
	}
	listed, err := os.Lstat(regular)
	if err != nil {
		t.Fatal(err)
	}
	// The digest frames the mode. The open descriptor must report the mode
	// that a path lookup reports, or two walks of one tree could differ.
	if info.Mode().String() != listed.Mode().String() || info.Size() != listed.Size() {
		t.Fatalf("descriptor reports %s/%d, path reports %s/%d", info.Mode(), info.Size(), listed.Mode(), listed.Size())
	}
	file.Close()

	link := filepath.Join(root, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Each of these can replace a regular file after the directory listing.
	// None may be read as that file, and the FIFO must not block.
	for name, path := range map[string]string{"symbolic link": link, "directory": directory, "FIFO": fifo, "missing path": filepath.Join(root, "missing")} {
		if file, _, ok := openListedRegularFile(path); ok {
			file.Close()
			t.Errorf("a %s was opened as a regular file", name)
		}
	}
}

func TestDigestTreeGivesOneFingerprintForBothWaysToOpenAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	fast, err := digestTreeEntry(context.Background(), root, treeEntry{name: "a.txt", listedRegular: true})
	if err != nil {
		t.Fatal(err)
	}
	general, err := digestTreeEntry(context.Background(), root, treeEntry{name: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if fast != general {
		t.Fatal("one file has two fingerprints")
	}
	// A listing that is out of date must not change what is fingerprinted.
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	stale, err := digestTreeEntry(context.Background(), root, treeEntry{name: "link", listedRegular: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err := digestTreeEntry(context.Background(), root, treeEntry{name: "link"})
	if err != nil {
		t.Fatal(err)
	}
	if stale != current {
		t.Fatal("a symbolic link listed as a regular file was fingerprinted as its target")
	}
}
