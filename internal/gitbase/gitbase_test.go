package gitbase

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectKeepsBaseErosionOutOfTheBranchContribution(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "migrations/0001_base.sql", "CREATE TABLE base_table (id bigint);\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "migrations/0002_feature.sql", "CREATE TABLE feature_table (id bigint);\n")
	commit(t, repository, "feature")
	feature := strings.TrimSpace(run(t, repository, "rev-parse", "HEAD"))

	run(t, repository, "checkout", "main")
	write(t, repository, "migrations/0003_main.sql", "CREATE TABLE main_table (id bigint);\n")
	commit(t, repository, "main erosion")
	base := strings.TrimSpace(run(t, repository, "rev-parse", "HEAD"))
	run(t, repository, "checkout", "feature")

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", HeadRef: "HEAD", MigrationPath: "migrations", IncludeWorkingTree: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.Outcome != "ready" || status.Relationship != "base_advanced" || !status.SyntheticHeadNeeded {
		t.Fatalf("status = %#v", status)
	}
	if status.BaseCommit != base || status.HeadCommit != feature || status.Dirty || status.HeadRevision != feature {
		t.Fatalf("provenance = %#v", status)
	}
	if len(status.MigrationChanges) != 1 || status.MigrationChanges[0].Path != "migrations/0002_feature.sql" || status.MigrationChanges[0].Ownership != "branch_draft" {
		t.Fatalf("main migration was mistaken for a branch change: %#v", status.MigrationChanges)
	}
}

func TestInspectBlocksBaseHistoryMutationAndConcurrentPathCollision(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "migrations/0001_base.sql", "SELECT 1;\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "migrations/0001_base.sql", "SELECT 2;\n")
	write(t, repository, "migrations/0002_collision.sql", "SELECT 'feature';\n")
	commit(t, repository, "feature changes")

	run(t, repository, "checkout", "main")
	write(t, repository, "migrations/0002_collision.sql", "SELECT 'main';\n")
	commit(t, repository, "main collision")
	run(t, repository, "checkout", "feature")

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Outcome != "blocked" || len(status.Problems) != 2 {
		t.Fatalf("status = %#v", status)
	}
	if !hasProblem(status, "base_history_modified", "migrations/0001_base.sql") || !hasProblem(status, "base_path_collision", "migrations/0002_collision.sql") {
		t.Fatalf("problems = %#v", status.Problems)
	}
}

func TestInspectIncludesDirtyDraftsAndContentInHeadRevision(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "migrations/0001_base.sql", "SELECT 1;\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "migrations/0002_feature.sql", "SELECT 2;\n")
	commit(t, repository, "feature")
	write(t, repository, "migrations/0002_feature.sql", "SELECT 22;\n")
	write(t, repository, "migrations/0003_untracked.sql", "SELECT 3;\n")

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	first, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations", IncludeWorkingTree: true})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Dirty || !strings.HasPrefix(first.HeadRevision, "dirty-sha256:") || len(first.MigrationChanges) != 3 {
		t.Fatalf("dirty status = %#v", first)
	}
	write(t, repository, "migrations/0003_untracked.sql", "SELECT 33;\n")
	second, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations", IncludeWorkingTree: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.HeadRevision == second.HeadRevision {
		t.Fatal("dirty revision did not include untracked file content")
	}
}

func TestPreparePRTreeMaterializesBaseErosionAndFeatureContribution(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "schema/base.sql", "base\n")
	write(t, repository, "migrations/0001.sql", "base\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "schema/feature.sql", "feature\n")
	write(t, repository, "migrations/0002.sql", "feature\n")
	commit(t, repository, "feature")
	run(t, repository, "checkout", "main")
	write(t, repository, "schema/main.sql", "main\n")
	write(t, repository, "migrations/0003.sql", "main\n")
	commit(t, repository, "main erosion")
	run(t, repository, "checkout", "feature")

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations", IncludeWorkingTree: true})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gitRepository.PreparePRTree(context.Background(), status)
	if err != nil {
		t.Fatal(err)
	}
	root := prepared.Root
	defer prepared.Close()
	for _, name := range []string{"schema/base.sql", "schema/feature.sql", "schema/main.sql", "migrations/0001.sql", "migrations/0002.sql", "migrations/0003.sql"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("synthetic tree is missing %s: %v", name, err)
		}
	}
	if prepared.TreeID == "" || prepared.HeadRevision != status.HeadRevision {
		t.Fatalf("prepared tree = %#v", prepared)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("prepared tree cleanup left %s", root)
	}
}

func TestPreparePRTreeOverlaysDirtyFilesWithoutWritingTheirObjects(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "migrations/0001.sql", "base\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "schema.sql", "committed feature\n")
	commit(t, repository, "feature")
	write(t, repository, "schema.sql", "unique dirty tracked content\n")
	write(t, repository, "untracked.sql", "unique dirty untracked content\n")
	trackedObject := strings.TrimSpace(run(t, repository, "hash-object", "schema.sql"))
	untrackedObject := strings.TrimSpace(run(t, repository, "hash-object", "untracked.sql"))
	assertMissingObject(t, repository, trackedObject)
	assertMissingObject(t, repository, untrackedObject)

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations", IncludeWorkingTree: true})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gitRepository.PreparePRTree(context.Background(), status)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if got := read(t, filepath.Join(prepared.Root, "schema.sql")); got != "unique dirty tracked content\n" {
		t.Fatalf("tracked overlay = %q", got)
	}
	if got := read(t, filepath.Join(prepared.Root, "untracked.sql")); got != "unique dirty untracked content\n" {
		t.Fatalf("untracked overlay = %q", got)
	}
	assertMissingObject(t, repository, trackedObject)
	assertMissingObject(t, repository, untrackedObject)
}

func TestPreparePRTreeReportsMergeConflict(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "schema.sql", "base\n")
	write(t, repository, "migrations/0001.sql", "base\n")
	commit(t, repository, "base")
	run(t, repository, "checkout", "-b", "feature")
	write(t, repository, "schema.sql", "feature\n")
	commit(t, repository, "feature")
	run(t, repository, "checkout", "main")
	write(t, repository, "schema.sql", "main\n")
	commit(t, repository, "main")
	run(t, repository, "checkout", "feature")

	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{BaseRef: "main", MigrationPath: "migrations"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gitRepository.PreparePRTree(context.Background(), status)
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Details, "schema.sql") {
		t.Fatalf("expected typed merge conflict, got %v", err)
	}
}

func TestInspectExcludesGeneratedReceiptRootFromDirtyRevision(t *testing.T) {
	repository := newRepository(t)
	write(t, repository, "migrations/0001.sql", "base\n")
	commit(t, repository, "base")
	write(t, repository, "onward-bundles/primary/feature/manifest.json", "generated\n")
	write(t, repository, "migrations/0002_feature.sql", "generated migration\n")
	gitRepository, err := Open(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitRepository.Inspect(context.Background(), Options{
		BaseRef: "main", MigrationPath: "migrations", IncludeWorkingTree: true,
		ExcludePaths: []string{"onward-bundles", "migrations"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty || status.HeadRevision != status.HeadCommit || len(status.ExcludedPaths) != 2 {
		t.Fatalf("generated receipt changed source revision: %#v", status)
	}
	prepared, err := gitRepository.PreparePRTree(context.Background(), status)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := os.Stat(filepath.Join(prepared.Root, "onward-bundles")); !os.IsNotExist(err) {
		t.Fatalf("excluded untracked bundle reached synthetic tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prepared.Root, "migrations", "0002_feature.sql")); !os.IsNotExist(err) {
		t.Fatalf("excluded untracked migration reached synthetic tree: %v", err)
	}
}

func hasProblem(status Status, code, path string) bool {
	for _, problem := range status.Problems {
		if problem.Code == code && problem.Path == path {
			return true
		}
	}
	return false
}

func read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMissingObject(t *testing.T, repository, object string) {
	t.Helper()
	command := exec.Command("git", "-C", repository, "cat-file", "-e", object)
	if err := command.Run(); err == nil {
		t.Fatalf("dirty content object %s was unexpectedly written", object)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	run(t, directory, "init", "-b", "main")
	run(t, directory, "config", "user.name", "Onward Test")
	run(t, directory, "config", "user.email", "onward@example.test")
	return directory
}

func write(t *testing.T, repository, name, contents string) {
	t.Helper()
	full := filepath.Join(repository, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repository, message string) {
	t.Helper()
	run(t, repository, "add", "-A")
	run(t, repository, "commit", "-m", message)
}

func run(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
