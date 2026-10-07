package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// countingCommand returns a schema command that prints one line per entry of
// outputs, in order, and repeats the last entry afterwards. The run counter is
// kept outside root, so the command leaves the fingerprinted tree unchanged.
func countingCommand(t *testing.T, outputs ...string) (Target, func() int) {
	t.Helper()
	state := t.TempDir()
	counter := filepath.Join(state, "runs")
	if err := os.WriteFile(counter, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index, output := range outputs {
		if err := os.WriteFile(filepath.Join(state, "out-"+strconv.Itoa(index+1)), []byte(output), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `n=$(($(cat "$0/runs") + 1)); printf %s "$n" > "$0/runs"; ` +
		`if [ "$n" -gt ` + strconv.Itoa(len(outputs)) + ` ]; then n=` + strconv.Itoa(len(outputs)) + `; fi; cat "$0/out-$n"`
	runs := func() int {
		body, err := os.ReadFile(counter)
		if err != nil {
			t.Fatal(err)
		}
		count, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	return Target{SchemaCommand: []string{"sh", "-c", script, state}, DevDatabaseEnv: "DEV_DATABASE_URL"}, runs
}

func TestExportConfirmsEqualBytesWithTwoRuns(t *testing.T) {
	target, runs := countingCommand(t, "CREATE TABLE users (id bigint);\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if runs() != 1 {
		t.Fatalf("StartExport ran the command %d times, want 1", runs())
	}
	if got := string(export.Compiled().DDL); got != "CREATE TABLE users (id bigint);\n" {
		t.Fatalf("compiled DDL = %q", got)
	}
	changed, err := export.Confirm(context.Background())
	if err != nil || changed != nil {
		t.Fatalf("Confirm = %v, %v; want an unchanged export", changed, err)
	}
	if runs() != 2 {
		t.Fatalf("an unchanged export ran the command %d times, want 2", runs())
	}
	// The proof is complete. A later request for it must not run the command.
	if err := ConfirmDeterministic(context.Background(), export); err != nil {
		t.Fatal(err)
	}
	if runs() != 2 {
		t.Fatalf("a confirmed export ran the command again: %d runs", runs())
	}
}

func TestExportReturnsAChangedDeterministicExport(t *testing.T) {
	target, runs := countingCommand(t, "CREATE TABLE users (id bigint);\n", "CREATE TABLE users (id bigint, name text);\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := export.Confirm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil || string(changed.DDL) != "CREATE TABLE users (id bigint, name text);\n" {
		t.Fatalf("Confirm = %#v, want the changed export", changed)
	}
	if got := string(export.Compiled().DDL); got != "CREATE TABLE users (id bigint);\n" {
		t.Fatalf("the first export changed to %q", got)
	}
	// The second run differed from the first, so a third run had to prove that
	// the second is deterministic.
	if runs() != 3 {
		t.Fatalf("a changed export ran the command %d times, want 3", runs())
	}
}

func TestExportRejectsANondeterministicExport(t *testing.T) {
	target, _ := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n", "SELECT 3;\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := export.Confirm(context.Background())
	if err == nil || !strings.Contains(err.Error(), "DDL export is nondeterministic") || changed != nil {
		t.Fatalf("Confirm = %v, %v; want a nondeterministic export error", changed, err)
	}
	if export.confirmed {
		t.Fatal("a failed confirmation was recorded as a determinism proof")
	}
}

func TestConfirmDeterministicCompletesTheSecondRun(t *testing.T) {
	stable, runs := countingCommand(t, "SELECT 1;\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", stable)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmDeterministic(context.Background(), export); err != nil {
		t.Fatal(err)
	}
	if runs() != 2 {
		t.Fatalf("the determinism proof ran the command %d times, want 2", runs())
	}
	if err := ConfirmDeterministic(context.Background(), nil); err != nil {
		t.Fatalf("a command that started no export must need no proof: %v", err)
	}

	unstable, _ := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n", "SELECT 3;\n")
	export, err = StartExport(context.Background(), t.TempDir(), "primary", unstable)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmDeterministic(context.Background(), export); err == nil || !strings.Contains(err.Error(), "DDL export is nondeterministic") {
		t.Fatalf("ConfirmDeterministic = %v, want a nondeterministic export error", err)
	}

	// An export that changed once and is then stable is deterministic. Two
	// back-to-back runs at the start would also have accepted it.
	edited, _ := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n")
	export, err = StartExport(context.Background(), t.TempDir(), "primary", edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmDeterministic(context.Background(), export); err != nil {
		t.Fatalf("ConfirmDeterministic rejected a changed deterministic export: %v", err)
	}
}

func TestExportConfirmReadsAChangedSchemaFile(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "schema.sql")
	if err := os.WriteFile(name, []byte("CREATE TABLE users (id bigint);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := Target{SchemaFile: "schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL"}
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("CREATE TABLE users (id bigint, name text);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := export.Confirm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil || !strings.Contains(string(changed.DDL), "name text") {
		t.Fatalf("Confirm = %#v, want the edited schema file", changed)
	}
}

func TestExportSettleReportsAFirstRunThatWritesToTheCheckout(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	script := `n=$(($(cat "$0/runs" 2>/dev/null || printf 0) + 1)); printf %s "$n" > "$0/runs"; printf x > leaked; printf 'SELECT 1;\n'`
	target := Target{SchemaCommand: []string{"sh", "-c", script, state}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	// The output is returned while the fingerprint is still in progress. Every
	// later step must then reject the run.
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := export.Settle(); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
			t.Fatalf("Settle = %v, want a modified inputs rejection", err)
		}
	}
	if _, err := export.Confirm(context.Background()); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("Confirm = %v, want a modified inputs rejection", err)
	}
	if err := ConfirmDeterministic(context.Background(), export); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("ConfirmDeterministic = %v, want a modified inputs rejection", err)
	}
	if body, err := os.ReadFile(filepath.Join(state, "runs")); err != nil || string(body) != "1" {
		t.Fatalf("a rejected first run was followed by another run: runs=%q err=%v", body, err)
	}
}

func TestCompileDDLReportsAChangedCheckoutBeforeAFailedCommand(t *testing.T) {
	target := Target{SchemaCommand: []string{"sh", "-c", "printf x > leaked; exit 3"}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	if _, err := CompileDDL(context.Background(), t.TempDir(), "primary", target); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("CompileDDL = %v, want a modified inputs rejection", err)
	}
	if _, err := StartExport(context.Background(), t.TempDir(), "primary", target); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("StartExport = %v, want a modified inputs rejection", err)
	}
	failing := Target{SchemaCommand: []string{"sh", "-c", "exit 3"}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	if _, err := StartExport(context.Background(), t.TempDir(), "primary", failing); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("StartExport = %v, want a failed command", err)
	}
}

func TestPreparedConfirmAcceptsACheckoutEditBeforeTheRun(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	target, runs := countingCommand(t, "SELECT 1;\n")
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := export.Settle(); err != nil {
		t.Fatal(err)
	}
	export.Prepare(context.Background())
	// Wait for the prepared fingerprint, then edit a file. The edit is before
	// the confirming run, so the run did not make it.
	if _, err := export.prepared.wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := export.Confirm(context.Background())
	if err != nil || changed != nil {
		t.Fatalf("Confirm = %v, %v; want an unchanged export", changed, err)
	}
	// The run with the prepared fingerprint was not accepted. One more run
	// with a fingerprint of its own was.
	if runs() != 3 {
		t.Fatalf("the command ran %d times, want 3", runs())
	}
}

func TestPreparedConfirmUsesThePreparedFingerprint(t *testing.T) {
	root := t.TempDir()
	target, runs := countingCommand(t, "SELECT 1;\n")
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	export.Prepare(context.Background())
	first := export.prepared
	export.Prepare(context.Background())
	if export.prepared != first {
		t.Fatal("a second Prepare replaced the fingerprint in progress")
	}
	changed, err := export.Confirm(context.Background())
	if err != nil || changed != nil {
		t.Fatalf("Confirm = %v, %v; want an unchanged export", changed, err)
	}
	if runs() != 2 || export.prepared != nil {
		t.Fatalf("runs=%d prepared=%v; want 2 runs and a used fingerprint", runs(), export.prepared)
	}
}

func TestPreparedConfirmRejectsACommandThatWritesToTheCheckout(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	// The first run is read-only. Every later run writes to the checkout.
	script := `if [ -e "$0/later" ]; then date +%s%N >> leaked; printf x >> leaked; fi; : > "$0/later"; printf 'SELECT 1;\n'`
	target := Target{SchemaCommand: []string{"sh", "-c", script, state}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	export.Prepare(context.Background())
	if _, err := export.Confirm(context.Background()); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("Confirm = %v, want a modified inputs rejection", err)
	}
	if export.confirmed {
		t.Fatal("a rejected run was recorded as a determinism proof")
	}
}

func TestPrepareDoesNothingForASchemaFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	export, err := StartExport(context.Background(), root, "primary", Target{SchemaFile: "schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := export.Settle(); err != nil {
		t.Fatal(err)
	}
	export.Prepare(context.Background())
	if export.prepared != nil {
		t.Fatal("a schema file needs no checkout fingerprint")
	}
	if changed, err := export.Confirm(context.Background()); err != nil || changed != nil {
		t.Fatalf("Confirm = %v, %v; want an unchanged export", changed, err)
	}
}

func TestExportConfirmRejectsACommandThatWritesToTheCheckout(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	// The first run is read-only. The second run writes a file to the checkout.
	script := `if [ -e "$0/second" ]; then printf x > leaked; fi; : > "$0/second"; printf 'SELECT 1;\n'`
	target := Target{SchemaCommand: []string{"sh", "-c", script, state}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	export, err := StartExport(context.Background(), root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := export.Confirm(context.Background()); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("Confirm = %v, want a modified inputs rejection", err)
	}
}

func TestCompileDDLRejectsAnEditBetweenItsTwoRuns(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	// The command leaves the checkout unchanged while it runs. It starts a
	// background writer that edits the checkout after the first run ends, so
	// the tree differs from the fingerprint that both runs share.
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := digestTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := Target{SchemaCommand: []string{"sh", "-c", `printf 'SELECT 1;\n'`, state}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	// A run that receives the fingerprint of an older tree state must fail,
	// although the command itself wrote nothing.
	if _, _, err := compileDDLOnce(context.Background(), root, "primary", target, fingerprint); err == nil || !strings.Contains(err.Error(), "modified repository inputs") {
		t.Fatalf("compileDDLOnce = %v, want a modified inputs rejection", err)
	}
	// The same run with its own fingerprint succeeds.
	if _, _, err := compileDDLOnce(context.Background(), root, "primary", target, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDigestTreeSeesEveryKindOfChange(t *testing.T) {
	build := func(t *testing.T) string {
		root := t.TempDir()
		for name, body := range map[string]string{
			"a.txt": "alpha", "dir/b.txt": "beta", "dir/sub/c.txt": "gamma", "empty.txt": "",
		} {
			full := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		return root
	}
	digest := func(t *testing.T, root string) string {
		t.Helper()
		value, err := digestTree(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := digest(t, build(t))
	if again := digest(t, build(t)); again != base {
		t.Fatalf("two equal trees have fingerprints %s and %s", base, again)
	}
	changes := map[string]func(root string) error{
		"content of the same size": func(root string) error {
			return os.WriteFile(filepath.Join(root, "dir", "b.txt"), []byte("BETA"), 0o644)
		},
		"content of another size": func(root string) error {
			return os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha!"), 0o644)
		},
		"mode": func(root string) error { return os.Chmod(filepath.Join(root, "a.txt"), 0o755) },
		"new file": func(root string) error {
			return os.WriteFile(filepath.Join(root, "dir", "new.txt"), nil, 0o644)
		},
		"new directory": func(root string) error { return os.Mkdir(filepath.Join(root, "dir", "more"), 0o755) },
		"removed file":  func(root string) error { return os.Remove(filepath.Join(root, "empty.txt")) },
		"renamed file": func(root string) error {
			return os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "z.txt"))
		},
		"symlink target": func(root string) error {
			if err := os.Remove(filepath.Join(root, "link")); err != nil {
				return err
			}
			return os.Symlink("dir/b.txt", filepath.Join(root, "link"))
		},
		"bytes moved between two files": func(root string) error {
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alph"), 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "dir", "b.txt"), []byte("abeta"), 0o644)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			root := build(t)
			if err := change(root); err != nil {
				t.Fatal(err)
			}
			if digest(t, root) == base {
				t.Fatalf("the fingerprint did not change after: %s", name)
			}
		})
	}
}

func TestDigestTreeDoesNotDependOnWorkerOrder(t *testing.T) {
	root := t.TempDir()
	for index := range 400 {
		name := filepath.Join(root, "d"+strconv.Itoa(index%7), "f"+strconv.Itoa(index))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(strings.Repeat("x", index)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, err := digestTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := digestTree(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("fingerprint changed between walks of one tree: %s then %s", first, again)
		}
	}
}

func TestDigestTreeStopsWhenCanceled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := digestTree(canceled, root); err == nil {
		t.Fatal("a canceled fingerprint returned no error")
	}
}
