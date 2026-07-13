// Package gitbase classifies the Git contribution of a migration feature
// branch without mutating the repository. It deliberately separates the
// branch patch (merge-base to head) from the exact, possibly advanced PR base.
package gitbase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const Version = "onwardpg.git-status/v1"

type Options struct {
	BaseRef            string
	HeadRef            string
	HistoryPath        string
	IncludeWorkingTree bool
	ExcludePaths       []string
}

type Status struct {
	ProtocolVersion     string          `json:"protocol_version"`
	Outcome             string          `json:"outcome"`
	BaseRef             string          `json:"base_ref"`
	BaseCommit          string          `json:"base_commit"`
	HeadRef             string          `json:"head_ref"`
	HeadCommit          string          `json:"head_commit"`
	HeadRevision        string          `json:"head_revision"`
	MergeBase           string          `json:"merge_base"`
	Relationship        string          `json:"relationship"`
	HistoryPath         string          `json:"history_path"`
	WorkingTreeIncluded bool            `json:"working_tree_included"`
	Dirty               bool            `json:"dirty"`
	SyntheticHeadNeeded bool            `json:"synthetic_head_needed"`
	ExcludedPaths       []string        `json:"excluded_paths,omitempty"`
	HistoryChanges      []HistoryChange `json:"history_changes,omitempty"`
	Problems            []Problem       `json:"problems,omitempty"`
}

type HistoryChange struct {
	Origin    string `json:"origin"`
	Status    string `json:"status"`
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Ownership string `json:"ownership"`
	Problem   string `json:"problem,omitempty"`
}

type Problem struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type Repository struct {
	Root string
}

func Open(ctx context.Context, start string) (Repository, error) {
	if start == "" {
		start = "."
	}
	output, err := runGit(ctx, "", "-C", start, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repository{}, fmt.Errorf("find git repository: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" || !filepath.IsAbs(root) {
		return Repository{}, fmt.Errorf("git returned invalid repository root %q", root)
	}
	return Repository{Root: filepath.Clean(root)}, nil
}

func (r Repository) Inspect(ctx context.Context, options Options) (Status, error) {
	if options.BaseRef == "" {
		return Status{}, fmt.Errorf("base ref is required")
	}
	if options.HeadRef == "" {
		options.HeadRef = "HEAD"
	}
	if err := validateRepositoryPath(options.HistoryPath); err != nil {
		return Status{}, fmt.Errorf("history path: %w", err)
	}
	excluded, err := normalizePaths(options.ExcludePaths)
	if err != nil {
		return Status{}, fmt.Errorf("exclude paths: %w", err)
	}
	base, err := r.resolveCommit(ctx, options.BaseRef)
	if err != nil {
		return Status{}, fmt.Errorf("resolve base ref %q: %w", options.BaseRef, err)
	}
	head, err := r.resolveCommit(ctx, options.HeadRef)
	if err != nil {
		return Status{}, fmt.Errorf("resolve head ref %q: %w", options.HeadRef, err)
	}
	mergeBases, err := r.lines(ctx, "merge-base", "--all", base, head)
	if err != nil {
		return Status{}, fmt.Errorf("resolve merge base: %w", err)
	}
	if len(mergeBases) != 1 {
		return Status{}, fmt.Errorf("expected exactly one merge base, found %d", len(mergeBases))
	}
	mergeBase := mergeBases[0]

	baseFiles, err := r.treeFiles(ctx, base, options.HistoryPath)
	if err != nil {
		return Status{}, fmt.Errorf("list base history: %w", err)
	}
	ancestorFiles, err := r.treeFiles(ctx, mergeBase, options.HistoryPath)
	if err != nil {
		return Status{}, fmt.Errorf("list merge-base history: %w", err)
	}
	protected := make(map[string]bool, len(baseFiles)+len(ancestorFiles))
	for name := range baseFiles {
		protected[name] = true
	}
	for name := range ancestorFiles {
		protected[name] = true
	}

	committed, err := r.diff(ctx, mergeBase, head, options.HistoryPath)
	if err != nil {
		return Status{}, fmt.Errorf("classify committed migration changes: %w", err)
	}
	for i := range committed {
		committed[i].Origin = "committed"
	}
	changes := committed
	headRevision := head
	dirty := false
	if options.IncludeWorkingTree {
		working, err := r.diff(ctx, head, "", options.HistoryPath)
		if err != nil {
			return Status{}, fmt.Errorf("classify working-tree migration changes: %w", err)
		}
		for i := range working {
			working[i].Origin = "working_tree"
		}
		untracked, err := r.untracked(ctx, options.HistoryPath, nil)
		if err != nil {
			return Status{}, fmt.Errorf("list untracked history files: %w", err)
		}
		for _, name := range untracked {
			working = append(working, HistoryChange{Origin: "working_tree", Status: "A", Path: name})
		}
		changes = append(changes, working...)
		headRevision, dirty, err = r.workingRevision(ctx, head, excluded)
		if err != nil {
			return Status{}, fmt.Errorf("fingerprint working tree: %w", err)
		}
	}

	problems := classify(changes, protected, baseFiles)
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Origin != changes[j].Origin {
			return changes[i].Origin < changes[j].Origin
		}
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].OldPath < changes[j].OldPath
	})
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Code != problems[j].Code {
			return problems[i].Code < problems[j].Code
		}
		return problems[i].Path < problems[j].Path
	})
	outcome := "ready"
	if len(problems) > 0 {
		outcome = "blocked"
	}
	return Status{
		ProtocolVersion: Version, Outcome: outcome,
		BaseRef: options.BaseRef, BaseCommit: base, HeadRef: options.HeadRef,
		HeadCommit: head, HeadRevision: headRevision, MergeBase: mergeBase,
		Relationship: relationship(base, head, mergeBase), HistoryPath: options.HistoryPath,
		WorkingTreeIncluded: options.IncludeWorkingTree, Dirty: dirty,
		SyntheticHeadNeeded: base != mergeBase, ExcludedPaths: excluded,
		HistoryChanges: changes, Problems: problems,
	}, nil
}

func relationship(base, head, mergeBase string) string {
	switch {
	case base == head:
		return "same_commit"
	case mergeBase == base:
		return "head_contains_base"
	case mergeBase == head:
		return "head_behind_base"
	default:
		return "base_advanced"
	}
}

func classify(changes []HistoryChange, protected, baseFiles map[string]bool) []Problem {
	var problems []Problem
	seenProblems := make(map[string]bool)
	addProblem := func(problem Problem) {
		key := problem.Code + "\x00" + problem.Path
		if !seenProblems[key] {
			seenProblems[key] = true
			problems = append(problems, problem)
		}
	}
	for i := range changes {
		change := &changes[i]
		oldProtected := change.OldPath != "" && protected[change.OldPath]
		pathProtected := protected[change.Path]
		collision := strings.HasPrefix(change.Status, "A") && baseFiles[change.Path]
		switch {
		case collision:
			change.Ownership, change.Problem = "base_reachable", "base_path_collision"
			addProblem(Problem{Code: change.Problem, Path: change.Path, Message: "branch history path collides with a bundle now reachable from the PR base"})
		case oldProtected || pathProtected:
			change.Ownership, change.Problem = "base_reachable", "base_history_modified"
			path := change.Path
			if oldProtected {
				path = change.OldPath
			}
			addProblem(Problem{Code: change.Problem, Path: path, Message: "branch changes onwardpg history reachable from the PR base or merge base"})
		default:
			change.Ownership = "branch_draft"
		}
	}
	return problems
}

func (r Repository) resolveCommit(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) != ref || ref == "" || strings.IndexFunc(ref, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid ref")
	}
	output, err := r.git(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(output))
	if !isObjectID(commit) {
		return "", fmt.Errorf("git returned invalid commit id %q", commit)
	}
	return commit, nil
}

func (r Repository) lines(ctx context.Context, arguments ...string) ([]string, error) {
	output, err := r.git(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func (r Repository) treeFiles(ctx context.Context, commit, directory string) (map[string]bool, error) {
	output, err := r.git(ctx, "ls-tree", "-r", "-z", "--name-only", commit, "--", directory)
	if err != nil {
		return nil, err
	}
	files := make(map[string]bool)
	for _, name := range splitNUL(output) {
		files[name] = true
	}
	return files, nil
}

func (r Repository) diff(ctx context.Context, from, to, directory string) ([]HistoryChange, error) {
	arguments := []string{"diff", "--name-status", "-z", "--find-renames", "--no-ext-diff", from}
	if to != "" {
		arguments = append(arguments, to)
	}
	arguments = append(arguments, "--", directory)
	output, err := r.git(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(output)
}

func parseNameStatus(output []byte) ([]HistoryChange, error) {
	fields := splitNUL(output)
	changes := make([]HistoryChange, 0, len(fields)/2)
	for index := 0; index < len(fields); {
		status := fields[index]
		index++
		if status == "" || index >= len(fields) {
			return nil, fmt.Errorf("invalid git name-status output")
		}
		if status[0] == 'R' || status[0] == 'C' {
			if index+1 >= len(fields) {
				return nil, fmt.Errorf("invalid git rename/copy output")
			}
			changes = append(changes, HistoryChange{Status: status, OldPath: fields[index], Path: fields[index+1]})
			index += 2
			continue
		}
		changes = append(changes, HistoryChange{Status: status, Path: fields[index]})
		index++
	}
	return changes, nil
}

func (r Repository) untracked(ctx context.Context, directory string, excludes []string) ([]string, error) {
	arguments := []string{"ls-files", "--others", "--exclude-standard", "-z", "--", directory}
	arguments = append(arguments, excludePathspecs(excludes)...)
	output, err := r.git(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	result := splitNUL(output)
	sort.Strings(result)
	return result, nil
}

func (r Repository) workingRevision(ctx context.Context, head string, excludes []string) (string, bool, error) {
	arguments := []string{"diff", "--binary", "--no-ext-diff", head, "--", "."}
	arguments = append(arguments, excludePathspecs(excludes)...)
	diff, err := r.git(ctx, arguments...)
	if err != nil {
		return "", false, err
	}
	untracked, err := r.untracked(ctx, ".", excludes)
	if err != nil {
		return "", false, err
	}
	if len(diff) == 0 && len(untracked) == 0 {
		return head, false, nil
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "onwardpg-working-tree-v1\x00"+head+"\x00")
	_, _ = hash.Write(diff)
	for _, name := range untracked {
		_, _ = io.WriteString(hash, "\x00"+name+"\x00")
		full := filepath.Join(r.Root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return "", false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", false, err
			}
			_, _ = io.WriteString(hash, "symlink\x00"+target)
			continue
		}
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("untracked path %s is not a regular file or symlink", name)
		}
		file, err := os.Open(full)
		if err != nil {
			return "", false, err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", false, copyErr
		}
		if closeErr != nil {
			return "", false, closeErr
		}
	}
	return "dirty-sha256:" + hex.EncodeToString(hash.Sum(nil)), true, nil
}

func normalizePaths(values []string) ([]string, error) {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if err := validateRepositoryPath(value); err != nil {
			return nil, fmt.Errorf("%q: %w", value, err)
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result, nil
}

func excludePathspecs(paths []string) []string {
	result := make([]string, 0, len(paths)*2)
	for _, value := range paths {
		result = append(result, ":(exclude)"+value, ":(exclude)"+value+"/**")
	}
	return result
}

func (r Repository) git(ctx context.Context, arguments ...string) ([]byte, error) {
	return runGit(ctx, r.Root, arguments...)
}

func runGit(ctx context.Context, root string, arguments ...string) ([]byte, error) {
	if root != "" {
		arguments = append([]string{"-C", root}, arguments...)
	}
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
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

func splitNUL(output []byte) []string {
	parts := strings.Split(string(output), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func validateRepositoryPath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, `\`) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("must be a repository-relative slash-separated path")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must be normalized and remain within the repository")
	}
	return nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
