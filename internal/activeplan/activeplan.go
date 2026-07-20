// Package activeplan stores the one mutable logical migration selected by a
// developer workspace. It is deliberately local state: the hash-chained
// bundle manifest proves history correctness, while this tiny anchor merely
// makes repeated `onwardpg plan` calls ergonomic.
package activeplan

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const Version = 1

type GitHygiene struct {
	Status      string `json:"status"`
	ExcludePath string `json:"exclude_path,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}

// Anchor is scoped to one configured target and one local worktree. PlanID is
// durable bundle identity; BundleID is the current, possibly resequenced,
// directory name.
type Anchor struct {
	Version    int         `json:"version"`
	Target     string      `json:"target"`
	PlanID     string      `json:"plan_id"`
	BundleID   string      `json:"bundle_id"`
	BranchHint string      `json:"branch_hint,omitempty"`
	Parked     []SavedPlan `json:"parked,omitempty"`
}

// SavedPlan is a local memory of an inactive worktree plan. It is not
// history, an authority claim, or a Git branch mapping: it merely lets a
// developer return to a plan after switching away in the same checkout. The
// durable bundle manifest still proves the plan identity when it is resumed.
type SavedPlan struct {
	PlanID     string `json:"plan_id"`
	BundleID   string `json:"bundle_id"`
	BranchHint string `json:"branch_hint,omitempty"`
}

func Path(root, target string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("active-plan root must be absolute")
	}
	if !safeName(target) {
		return "", fmt.Errorf("active-plan target %q is invalid", target)
	}
	return filepath.Join(root, ".onwardpg", "active", target+".json"), nil
}

func Load(root, target string) (Anchor, bool, error) {
	path, err := Path(root, target)
	if err != nil {
		return Anchor{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Anchor{}, false, nil
	}
	if err != nil {
		return Anchor{}, false, fmt.Errorf("read active plan: %w", err)
	}
	var anchor Anchor
	if err := json.Unmarshal(data, &anchor); err != nil {
		return Anchor{}, false, fmt.Errorf("decode active plan: %w", err)
	}
	if err := anchor.Validate(target); err != nil {
		return Anchor{}, false, err
	}
	return anchor, true, nil
}

func Store(root string, anchor Anchor) error {
	if err := anchor.Validate(anchor.Target); err != nil {
		return err
	}
	if _, err := EnsureGitExclude(root); err != nil {
		return err
	}
	path, err := Path(root, anchor.Target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create active-plan directory: %w", err)
	}
	data, err := json.MarshalIndent(anchor, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active plan: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".active-plan-")
	if err != nil {
		return fmt.Errorf("create active-plan temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect active-plan temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write active plan: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync active plan: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close active plan: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("install active plan: %w", err)
	}
	return nil
}

// EnsureGitExclude keeps worktree-local authoring state out of ordinary Git
// staging without changing the repository's committed .gitignore. It uses
// Git's own path resolution so linked worktrees and gitdir indirection behave
// the same as a normal checkout. Repositories that already track .onwardpg are
// rejected rather than mixing durable history with local PlanID state.
func EnsureGitExclude(root string) (GitHygiene, error) {
	if !filepath.IsAbs(root) {
		return GitHygiene{}, fmt.Errorf("git hygiene root must be absolute")
	}
	repositoryRoot, err := gitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		if isNotGitRepository(err) {
			return GitHygiene{Status: "outside_git"}, nil
		}
		return GitHygiene{}, fmt.Errorf("inspect Git repository: %w", err)
	}
	repositoryRoot = filepath.Clean(repositoryRoot)
	resolvedRepositoryRoot, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return GitHygiene{}, fmt.Errorf("resolve Git worktree root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return GitHygiene{}, fmt.Errorf("resolve project root: %w", err)
	}
	relativeRoot, err := filepath.Rel(resolvedRepositoryRoot, resolvedRoot)
	if err != nil || relativeRoot == ".." || strings.HasPrefix(relativeRoot, ".."+string(filepath.Separator)) {
		return GitHygiene{}, fmt.Errorf("project root %s is outside Git worktree %s", root, repositoryRoot)
	}
	localPath := ".onwardpg"
	if relativeRoot != "." {
		localPath = filepath.ToSlash(filepath.Join(relativeRoot, localPath))
	}
	tracked, err := gitOutput(root, "ls-files", "--", localPath, localPath+"/**")
	if err != nil {
		return GitHygiene{}, fmt.Errorf("inspect tracked local onward state: %w", err)
	}
	if strings.TrimSpace(tracked) != "" {
		return GitHygiene{}, fmt.Errorf("Git already tracks local onward state at %s; move durable files elsewhere and remove it from the index before continuing", localPath)
	}
	excludePath, err := gitOutput(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return GitHygiene{}, fmt.Errorf("resolve Git local exclude file: %w", err)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(repositoryRoot, excludePath)
	}
	excludePath = filepath.Clean(excludePath)
	pattern := "/" + filepath.ToSlash(localPath) + "/"
	body, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return GitHygiene{}, fmt.Errorf("read Git local exclude file: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == pattern {
			return GitHygiene{Status: "present", ExcludePath: excludePath, Pattern: pattern}, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return GitHygiene{}, fmt.Errorf("create Git info directory: %w", err)
	}
	updated := append([]byte(nil), body...)
	if len(updated) > 0 && updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	updated = append(updated, []byte("# onwardpg worktree-local PlanID state\n"+pattern+"\n")...)
	if err := os.WriteFile(excludePath, updated, 0o644); err != nil {
		return GitHygiene{}, fmt.Errorf("write Git local exclude file: %w", err)
	}
	return GitHygiene{Status: "installed", ExcludePath: excludePath, Pattern: pattern}, nil
}

type gitCommandError struct {
	output string
	err    error
}

func (e gitCommandError) Error() string { return strings.TrimSpace(e.output) + ": " + e.err.Error() }
func (e gitCommandError) Unwrap() error { return e.err }

func gitOutput(root string, arguments ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", gitCommandError{output: stderr.String(), err: err}
	}
	return strings.TrimSpace(string(output)), nil
}

func isNotGitRepository(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not a git repository")
}

func Clear(root, target string) error {
	path, err := Path(root, target)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove active plan: %w", err)
	}
	return nil
}

func NewID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate plan id: %w", err)
	}
	return "plan_" + hex.EncodeToString(bytes[:]), nil
}

func (a Anchor) Validate(target string) error {
	if a.Version != Version {
		return fmt.Errorf("active-plan version is %d, want %d", a.Version, Version)
	}
	if a.Target != target || !safeName(a.Target) {
		return fmt.Errorf("active-plan target %q is invalid for %q", a.Target, target)
	}
	if (a.PlanID == "") != (a.BundleID == "") {
		return fmt.Errorf("active-plan plan_id and bundle_id must both be present or absent")
	}
	if a.PlanID != "" && !safePlanID(a.PlanID) {
		return fmt.Errorf("active-plan plan_id %q is invalid", a.PlanID)
	}
	if a.BundleID != "" && !safeName(a.BundleID) {
		return fmt.Errorf("active-plan bundle_id %q is invalid", a.BundleID)
	}
	seenBundles := make(map[string]bool)
	seenPlans := make(map[string]bool)
	if a.BundleID != "" {
		seenBundles[a.BundleID], seenPlans[a.PlanID] = true, true
	}
	for _, saved := range a.Parked {
		if !safePlanID(saved.PlanID) || !safeName(saved.BundleID) {
			return fmt.Errorf("parked active plan is invalid")
		}
		if seenBundles[saved.BundleID] || seenPlans[saved.PlanID] {
			return fmt.Errorf("parked active plan duplicates %q", saved.BundleID)
		}
		seenBundles[saved.BundleID] = true
		seenPlans[saved.PlanID] = true
	}
	return nil
}

func (a Anchor) HasActive() bool { return a.PlanID != "" && a.BundleID != "" }

// ParkActive moves the current selector into local parked state without
// claiming anything about Git, merge, or deployment. Durable identity remains
// in the bundle manifest.
func (a Anchor) ParkActive() Anchor {
	if !a.HasActive() {
		return a
	}
	parked := append([]SavedPlan(nil), a.Parked...)
	parked = append(parked, SavedPlan{PlanID: a.PlanID, BundleID: a.BundleID, BranchHint: a.BranchHint})
	sortSavedPlans(parked)
	return Anchor{Version: Version, Target: a.Target, Parked: parked}
}

// WithoutActive retires only the current worktree selector. Unlike ParkActive,
// it deliberately forgets that selector after the caller has proved authoring
// is finished; previously parked plans remain available.
func (a Anchor) WithoutActive() Anchor {
	return Anchor{Version: Version, Target: a.Target, Parked: append([]SavedPlan(nil), a.Parked...)}
}

// WithActive returns a replacement active anchor while retaining the previous
// active plan as parked local context. Selecting a parked plan removes it from
// the parked set. The result is deterministic so an unchanged worktree does
// not churn its ignored anchor file.
func (a Anchor) WithActive(next SavedPlan) Anchor {
	parked := make([]SavedPlan, 0, len(a.Parked)+1)
	for _, saved := range a.Parked {
		if saved.BundleID != next.BundleID && saved.PlanID != next.PlanID {
			parked = append(parked, saved)
		}
	}
	if a.HasActive() && a.BundleID != next.BundleID && a.PlanID != next.PlanID {
		parked = append(parked, SavedPlan{PlanID: a.PlanID, BundleID: a.BundleID, BranchHint: a.BranchHint})
	}
	sortSavedPlans(parked)
	return Anchor{Version: Version, Target: a.Target, PlanID: next.PlanID, BundleID: next.BundleID, BranchHint: next.BranchHint, Parked: parked}
}

func (a Anchor) FindParked(bundleID string) (SavedPlan, bool) {
	for _, saved := range a.Parked {
		if saved.BundleID == bundleID {
			return saved, true
		}
	}
	return SavedPlan{}, false
}

func sortSavedPlans(plans []SavedPlan) {
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].BundleID != plans[j].BundleID {
			return plans[i].BundleID < plans[j].BundleID
		}
		return plans[i].PlanID < plans[j].PlanID
	})
}

func safePlanID(value string) bool {
	return strings.HasPrefix(value, "plan_") && len(value) == len("plan_")+32 && safeName(value)
}

func safeName(value string) bool {
	if value == "" || strings.Trim(value, ".") == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
