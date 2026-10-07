package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitCheckout returns a new git work tree with one commit that holds the
// given files. The user's own git configuration and ignore file are kept out,
// so the result depends on the repository alone.
func gitCheckout(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	root := t.TempDir()
	git(t, root, "init", "--quiet", "--initial-branch=main")
	writeTreeFiles(t, root, files)
	git(t, root, "add", "--all")
	git(t, root, "commit", "--quiet", "--allow-empty", "--message=initial")
	return root
}

func git(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{
		"-C", root, "-c", "user.name=onwardpg", "-c", "user.email=onwardpg@example.com", "-c", "commit.gpgsign=false",
	}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func writeTreeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// exportThat returns a schema command that runs script in the checkout and
// then prints one statement.
func exportThat(script string) Target {
	return Target{SchemaCommand: []string{"sh", "-c", script + `; printf 'SELECT 1;\n'`}, DevDatabaseEnv: "DEV_DATABASE_URL"}
}

// observedCompile runs CompileDDL under an observer and returns the paths
// that it reported.
func observedCompile(t *testing.T, root string, target Target) []string {
	t.Helper()
	ctx, observer := ObserveExportSideEffects(context.Background())
	if _, err := CompileDDL(ctx, root, "primary", target); err != nil {
		t.Fatalf("CompileDDL = %v, want a successful export", err)
	}
	return observer.Take(context.Background())
}

func TestExportThatWritesToTheWorkTreeSucceedsAndIsReported(t *testing.T) {
	files := map[string]string{
		"schema.sql":       "CREATE TABLE users (id bigint);\n",
		"src/generated.ts": "export {}\n",
		"docs/removed.md":  "text\n",
		"with space.txt":   "text\n",
	}
	script := `printf '// again\n' >> src/generated.ts && rm -f docs/removed.md && printf x >> 'with space.txt' && mkdir -p out && printf x > out/new.sql`
	want := []string{"docs/removed.md", "out/new.sql", "src/generated.ts", "with space.txt"}

	// The DDL is the same in both runs, so the command succeeds. The paths
	// whose git status changed are reported.
	if got := observedCompile(t, gitCheckout(t, files), exportThat(script)); !slices.Equal(got, want) {
		t.Fatalf("side effects = %q, want %q", got, want)
	}

	// The same holds for a command that runs the export at its start and
	// again before its result.
	root := gitCheckout(t, files)
	ctx, observer := ObserveExportSideEffects(context.Background())
	export, err := StartExport(ctx, root, "primary", exportThat(script))
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := export.Confirm(ctx); err != nil || changed != nil {
		t.Fatalf("Confirm = %v, %v; want an unchanged export", changed, err)
	}
	// A file that the command writes after its last export run is its own
	// result, not a side effect of the export.
	writeTreeFiles(t, root, map[string]string{"bundles/primary/plan.json": "{}\n"})
	if got := observer.Take(context.Background()); !slices.Equal(got, want) {
		t.Fatalf("side effects = %q, want %q", got, want)
	}
	if got := observer.Take(context.Background()); len(got) != 0 {
		t.Fatalf("side effects were reported twice: %q", got)
	}
}

func TestExportThatWritesOnlyIgnoredFilesIsNotReported(t *testing.T) {
	root := gitCheckout(t, map[string]string{
		".gitignore": ".turbo/\ndist/\n*.log\n",
		"schema.sql": "CREATE TABLE users (id bigint);\n",
	})
	writeTreeFiles(t, root, map[string]string{"dist/schema.js": "a build output\n"})
	// What a build in the same checkout does while the export runs.
	script := `mkdir -p .turbo/cache && date > .turbo/cache/entry && printf rebuilt > dist/schema.js && printf line >> build.log`
	if got := observedCompile(t, root, exportThat(script)); len(got) != 0 {
		t.Fatalf("side effects = %q, want none for ignored files", got)
	}
}

func TestSideEffectObservationDoesNotReplaceTheOutputCheck(t *testing.T) {
	root := gitCheckout(t, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n", "counter": ""})
	// The command writes a tracked file and prints different DDL each time.
	target := Target{SchemaCommand: []string{"sh", "-c", `printf x >> counter; printf 'SELECT %s;\n' "$(wc -c < counter | tr -d ' ')"`}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	ctx, _ := ObserveExportSideEffects(context.Background())
	if _, err := CompileDDL(ctx, root, "primary", target); err == nil || !strings.Contains(err.Error(), "nondeterministic") {
		t.Fatalf("CompileDDL = %v, want a nondeterministic export", err)
	}
	export, err := StartExport(ctx, root, "primary", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := export.Confirm(ctx); err == nil || !strings.Contains(err.Error(), "nondeterministic") {
		t.Fatalf("Confirm = %v, want a nondeterministic export", err)
	}
}

// A command that stops before the last export run still reports what the
// export command wrote: the command closes the open watch as it writes its
// output.
func TestSideEffectsOfAFailedOrAbandonedExportAreReported(t *testing.T) {
	files := map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n", "generated.ts": "export {}\n"}
	failing := Target{SchemaCommand: []string{"sh", "-c", `printf x >> generated.ts; exit 3`}, DevDatabaseEnv: "DEV_DATABASE_URL"}
	want := []string{"generated.ts"}

	ctx, observer := ObserveExportSideEffects(context.Background())
	if _, err := CompileDDL(ctx, gitCheckout(t, files), "primary", failing); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("CompileDDL = %v, want a failed command", err)
	}
	if got := observer.Take(context.Background()); !slices.Equal(got, want) {
		t.Fatalf("side effects of a failed CompileDDL = %q, want %q", got, want)
	}

	ctx, observer = ObserveExportSideEffects(context.Background())
	if _, err := StartExport(ctx, gitCheckout(t, files), "primary", failing); err == nil {
		t.Fatal("StartExport of a failing command returned no error")
	}
	if got := observer.Take(context.Background()); !slices.Equal(got, want) {
		t.Fatalf("side effects of a failed StartExport = %q, want %q", got, want)
	}

	// The first run succeeds and the command then stops for another reason,
	// so Confirm never runs.
	ctx, observer = ObserveExportSideEffects(context.Background())
	if _, err := StartExport(ctx, gitCheckout(t, files), "primary", exportThat(`printf x >> generated.ts`)); err != nil {
		t.Fatal(err)
	}
	if got := observer.Take(context.Background()); !slices.Equal(got, want) {
		t.Fatalf("side effects of an abandoned export = %q, want %q", got, want)
	}
	if got := observer.Take(context.Background()); len(got) != 0 {
		t.Fatalf("side effects were reported twice: %q", got)
	}
}

func TestExportOutsideAGitWorkTreeIsNotObserved(t *testing.T) {
	gitCheckout(t, nil) // Only for a git configuration that does not depend on the user.
	root := t.TempDir()
	writeTreeFiles(t, root, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n"})
	if got := observedCompile(t, root, exportThat(`printf x >> schema.sql && printf x > new.sql`)); len(got) != 0 {
		t.Fatalf("side effects = %q, want no observation outside git", got)
	}
}

func TestExportWithoutAGitExecutableIsNotObserved(t *testing.T) {
	root := gitCheckout(t, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n"})
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	// A PATH that holds the shell and nothing else.
	tools := t.TempDir()
	if err := os.Symlink(shell, filepath.Join(tools, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	if _, err := exec.LookPath("git"); err == nil {
		t.Fatal("git is still on PATH")
	}
	if got := observedCompile(t, root, exportThat(`printf x >> schema.sql`)); len(got) != 0 {
		t.Fatalf("side effects = %q, want no observation without git", got)
	}
}

func TestExportIsNotObservedUnlessTheCommandAsks(t *testing.T) {
	root := gitCheckout(t, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n"})
	// A git that fails loudly would be run if the export were observed.
	if watch := watchExportSideEffects(context.Background(), root, exportThat(`true`)); watch != nil {
		t.Fatal("an export without an observer was watched")
	}
	ctx, _ := ObserveExportSideEffects(context.Background())
	if watch := watchExportSideEffects(ctx, root, Target{SchemaFile: "schema.sql"}); watch != nil {
		t.Fatal("a schema_file target, which runs no command, was watched")
	}
}

func TestGitStatusFailureIsNotAnError(t *testing.T) {
	root := gitCheckout(t, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n"})
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := observedCompile(t, root, exportThat(`printf x >> schema.sql`)); len(got) != 0 {
		t.Fatalf("side effects = %q, want no observation when git status fails", got)
	}
}

// The status of a path says that the file differs from the index. It does
// not say how. This is the stated limit of the observation.
func TestSecondWriteToAnAlreadyChangedFileIsNotSeen(t *testing.T) {
	root := gitCheckout(t, map[string]string{"schema.sql": "CREATE TABLE users (id bigint);\n"})
	writeTreeFiles(t, root, map[string]string{"schema.sql": "CREATE TABLE users (id bigint, name text);\n", "notes.txt": "draft\n"})
	if got := observedCompile(t, root, exportThat(`printf x >> schema.sql && printf x >> notes.txt`)); len(got) != 0 {
		t.Fatalf("side effects = %q", got)
	}
}

func TestParseGitStatusKeysEveryRecordByItsPath(t *testing.T) {
	root := gitCheckout(t, map[string]string{"old name.txt": "text\n", "kept.txt": "text\n", "staged.txt": "text\n"})
	git(t, root, "mv", "old name.txt", "new name.txt")
	writeTreeFiles(t, root, map[string]string{"kept.txt": "changed\n", "staged.txt": "changed\n", "dir/untracked file": "x"})
	git(t, root, "add", "staged.txt")
	status, ok := gitStatus(context.Background(), root)
	if !ok {
		t.Fatal("git status failed")
	}
	var paths []string
	for path, record := range status {
		paths = append(paths, path)
		if !strings.Contains(record, path) {
			t.Errorf("record %q does not belong to %s", record, path)
		}
	}
	slices.Sort(paths)
	if want := []string{"dir/untracked file", "kept.txt", "new name.txt", "staged.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
	if !strings.HasSuffix(status["new name.txt"], "\x00old name.txt") {
		t.Fatalf("rename record = %q, want the original path as its second field", status["new name.txt"])
	}
}
