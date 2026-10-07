package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// countingCommand returns a schema command that prints one line per entry of
// outputs, in order, and repeats the last entry afterwards. The run counter is
// kept outside root, so the command leaves the checkout unchanged.
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
	if err := ConfirmUnchanged(context.Background(), export); err != nil {
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
	target, runs := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n", "SELECT 3;\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := export.Confirm(context.Background())
	if err == nil || !strings.Contains(err.Error(), "DDL export is nondeterministic") || changed != nil {
		t.Fatalf("Confirm = %v, %v; want a nondeterministic export error", changed, err)
	}
	// The rejection is final. Nothing runs the export again to get another
	// answer: after three different outputs the fake export is stable, and a
	// new pair of runs would accept it.
	rejected := runs()
	if err := ConfirmUnchanged(context.Background(), export); err != nil {
		t.Fatalf("ConfirmUnchanged after a reported rejection = %v", err)
	}
	if _, err := export.Confirm(context.Background()); err == nil {
		t.Fatal("a second Confirm of a rejected export returned no error")
	}
	if runs() != rejected {
		t.Fatalf("a rejected export ran again: %d runs, then %d", rejected, runs())
	}
}

func TestConfirmUnchangedRunsTheExportOnceMore(t *testing.T) {
	stable, runs := countingCommand(t, "SELECT 1;\n")
	export, err := StartExport(context.Background(), t.TempDir(), "primary", stable)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmUnchanged(context.Background(), export); err != nil {
		t.Fatal(err)
	}
	if runs() != 2 {
		t.Fatalf("the determinism proof ran the command %d times, want 2", runs())
	}
	if err := ConfirmUnchanged(context.Background(), nil); err != nil {
		t.Fatalf("a command that started no export must need no proof: %v", err)
	}

	unstable, _ := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n", "SELECT 3;\n")
	export, err = StartExport(context.Background(), t.TempDir(), "primary", unstable)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmUnchanged(context.Background(), export); err == nil || !strings.Contains(err.Error(), "DDL export is nondeterministic") {
		t.Fatalf("ConfirmUnchanged = %v, want a nondeterministic export error", err)
	}

	// An export that changed once and is then stable is deterministic, but the
	// result of the command describes the first output. It is not reported.
	edited, _ := countingCommand(t, "SELECT 1;\n", "SELECT 2;\n")
	export, err = StartExport(context.Background(), t.TempDir(), "primary", edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmUnchanged(context.Background(), export); !errors.Is(err, ErrExportChanged) {
		t.Fatalf("ConfirmUnchanged = %v, want ErrExportChanged", err)
	}

	// A schema file has no command, and the same rule.
	root := t.TempDir()
	name := filepath.Join(root, "schema.sql")
	if err := os.WriteFile(name, []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	export, err = StartExport(context.Background(), root, "primary", Target{SchemaFile: "schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("SELECT 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmUnchanged(context.Background(), export); !errors.Is(err, ErrExportChanged) {
		t.Fatalf("ConfirmUnchanged of an edited schema file = %v, want ErrExportChanged", err)
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
