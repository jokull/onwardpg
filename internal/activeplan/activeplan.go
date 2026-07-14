// Package activeplan stores the one mutable logical migration selected by a
// developer workspace. It is deliberately local state: the hash-chained
// bundle manifest proves history correctness, while this tiny anchor merely
// makes repeated `onwardpg plan` calls ergonomic.
package activeplan

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const Version = 1

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
	if !safePlanID(a.PlanID) {
		return fmt.Errorf("active-plan plan_id %q is invalid", a.PlanID)
	}
	if !safeName(a.BundleID) {
		return fmt.Errorf("active-plan bundle_id %q is invalid", a.BundleID)
	}
	seenBundles := map[string]bool{a.BundleID: true}
	seenPlans := map[string]bool{a.PlanID: true}
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
	if a.BundleID != next.BundleID && a.PlanID != next.PlanID {
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
