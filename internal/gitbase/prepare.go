package gitbase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type PreparedTree struct {
	Root         string
	TreeID       string
	HeadRevision string
	container    string
}

func (p *PreparedTree) Close() error {
	if p == nil || p.container == "" {
		return nil
	}
	err := os.RemoveAll(p.container)
	p.container, p.Root = "", ""
	return err
}

type MergeConflictError struct {
	BaseCommit string
	HeadCommit string
	Details    string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("PR contribution conflicts with base while constructing synthetic head: %s", e.Details)
}

// PreparePRTree materializes the would-be merge result of the exact base and
// PR head into an isolated directory. If status includes a dirty working tree,
// the current checkout delta is overlaid only after its revision is rechecked.
func (r Repository) PreparePRTree(ctx context.Context, status Status) (*PreparedTree, error) {
	if status.ProtocolVersion != Version || status.BaseCommit == "" || status.HeadCommit == "" || status.HeadRevision == "" {
		return nil, fmt.Errorf("invalid git status receipt")
	}
	if status.Outcome == "blocked" {
		return nil, fmt.Errorf("cannot prepare a blocked git status")
	}
	if status.WorkingTreeIncluded {
		currentHead, err := r.resolveCommit(ctx, "HEAD")
		if err != nil {
			return nil, err
		}
		if currentHead != status.HeadCommit {
			return nil, fmt.Errorf("HEAD changed after git status: have %s want %s", currentHead, status.HeadCommit)
		}
		revision, dirty, err := r.workingRevision(ctx, currentHead, status.ExcludedPaths)
		if err != nil {
			return nil, err
		}
		if revision != status.HeadRevision || dirty != status.Dirty {
			return nil, fmt.Errorf("working tree changed after git status")
		}
	}
	treeID, err := r.mergeTree(ctx, status.BaseCommit, status.HeadCommit)
	if err != nil {
		return nil, err
	}
	prepared, err := r.materializeTree(ctx, treeID, status.HeadRevision)
	if err != nil {
		return nil, err
	}
	if status.WorkingTreeIncluded && status.Dirty {
		if err := r.overlayWorkingTree(ctx, prepared.Root, status.HeadCommit, status.ExcludedPaths); err != nil {
			_ = prepared.Close()
			return nil, err
		}
		revision, dirty, err := r.workingRevision(ctx, status.HeadCommit, status.ExcludedPaths)
		if err != nil {
			_ = prepared.Close()
			return nil, err
		}
		if !dirty || revision != status.HeadRevision {
			_ = prepared.Close()
			return nil, fmt.Errorf("working tree changed while preparing synthetic head")
		}
	}
	return prepared, nil
}

// PrepareCommitTree materializes an exact commit tree, primarily for compiling
// the base declarative schema without checking out or mutating the repository.
func (r Repository) PrepareCommitTree(ctx context.Context, commit string) (*PreparedTree, error) {
	resolved, err := r.resolveCommit(ctx, commit)
	if err != nil {
		return nil, err
	}
	output, err := r.git(ctx, "rev-parse", "--verify", "--end-of-options", resolved+"^{tree}")
	if err != nil {
		return nil, err
	}
	treeID := strings.TrimSpace(string(output))
	if !isObjectID(treeID) {
		return nil, fmt.Errorf("git returned invalid tree id %q", treeID)
	}
	return r.materializeTree(ctx, treeID, resolved)
}

func (r Repository) mergeTree(ctx context.Context, base, head string) (string, error) {
	arguments := []string{"merge-tree", "--write-tree", "--messages", base, head}
	command := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		details := sanitizeMergeDetails(string(output))
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 && strings.Contains(string(output), "CONFLICT ") {
			return "", &MergeConflictError{BaseCommit: base, HeadCommit: head, Details: details}
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(arguments, " "), strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || !isObjectID(strings.TrimSpace(lines[0])) {
		return "", fmt.Errorf("git merge-tree returned invalid tree output")
	}
	return strings.TrimSpace(lines[0]), nil
}

func sanitizeMergeDetails(output string) string {
	var details []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CONFLICT ") || strings.HasPrefix(line, "Auto-merging ") {
			details = append(details, line)
		}
	}
	if len(details) == 0 {
		return "git merge-tree reported a conflict"
	}
	return strings.Join(details, "; ")
}

func (r Repository) materializeTree(ctx context.Context, treeID, revision string) (*PreparedTree, error) {
	if !isObjectID(treeID) {
		return nil, fmt.Errorf("invalid tree id %q", treeID)
	}
	container, err := os.MkdirTemp("", "onwardpg-tree-")
	if err != nil {
		return nil, err
	}
	root := filepath.Join(container, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		_ = os.RemoveAll(container)
		return nil, err
	}
	index := filepath.Join(container, "index")
	environment := []string{"GIT_INDEX_FILE=" + index}
	if _, err := r.gitWithEnv(ctx, environment, "read-tree", treeID); err != nil {
		_ = os.RemoveAll(container)
		return nil, fmt.Errorf("read synthetic tree: %w", err)
	}
	prefix := root + string(filepath.Separator)
	if _, err := r.gitWithEnv(ctx, environment, "checkout-index", "--all", "--force", "--prefix="+prefix); err != nil {
		_ = os.RemoveAll(container)
		return nil, fmt.Errorf("materialize synthetic tree: %w", err)
	}
	return &PreparedTree{Root: root, TreeID: treeID, HeadRevision: revision, container: container}, nil
}

func (r Repository) overlayWorkingTree(ctx context.Context, destination, head string, excludes []string) error {
	arguments := []string{"diff", "--binary", "--full-index", "--no-ext-diff", head, "--", "."}
	arguments = append(arguments, excludePathspecs(excludes)...)
	patch, err := r.git(ctx, arguments...)
	if err != nil {
		return err
	}
	if len(patch) > 0 {
		command := exec.CommandContext(ctx, "git", "apply", "--binary", "--whitespace=nowarn")
		command.Dir = destination
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
		command.Stdin = bytes.NewReader(patch)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("dirty tracked changes conflict with synthetic head: %s", strings.TrimSpace(string(output)))
		}
	}
	untracked, err := r.untracked(ctx, ".", excludes)
	if err != nil {
		return err
	}
	for _, name := range untracked {
		if err := copyUntracked(filepath.Join(r.Root, filepath.FromSlash(name)), filepath.Join(destination, filepath.FromSlash(name)), name); err != nil {
			return err
		}
	}
	return nil
}

func copyUntracked(source, destination, relative string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("untracked path %s collides with synthetic head", relative)
	} else if !os.IsNotExist(err) {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("untracked path %s is not a regular file or symlink", relative)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (r Repository) gitWithEnv(ctx context.Context, environment []string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, arguments...)...)
	command.Env = append(os.Environ(), append([]string{"GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"}, environment...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(arguments, " "), message)
	}
	return output, nil
}
