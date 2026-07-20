package activeplan

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsureGitExcludeKeepsLocalStateOutOfStatus(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	result, err := EnsureGitExclude(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "installed" || result.Pattern != "/.onwardpg/" {
		t.Fatalf("git hygiene = %#v", result)
	}
	if err := os.MkdirAll(filepath.Join(root, ".onwardpg", "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".onwardpg", "active", "primary.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("git", "-C", root, "status", "--short", "--untracked-files=all").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("local state appeared in Git status: %s", output)
	}
	again, err := EnsureGitExclude(root)
	if err != nil || again.Status != "present" {
		t.Fatalf("second git hygiene = %#v, %v", again, err)
	}
}

func TestEnsureGitExcludeRejectsTrackedLocalState(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(root, ".onwardpg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".onwardpg", "tracked"), []byte("bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", root, "add", ".onwardpg/tracked").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if _, err := EnsureGitExclude(root); err == nil || !strings.Contains(err.Error(), "already tracks") {
		t.Fatalf("tracked collision error = %v", err)
	}
}

func TestStoreLoadAndClear(t *testing.T) {
	root := t.TempDir()
	anchor := Anchor{Version: Version, Target: "primary", PlanID: "plan_0123456789abcdef0123456789abcdef", BundleID: "booking-dates"}
	if err := Store(root, anchor); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := Load(root, "primary")
	if err != nil || !found || !reflect.DeepEqual(loaded, anchor) {
		t.Fatalf("Load() = %#v, %v, %v", loaded, found, err)
	}
	path, err := Path(root, "primary")
	if err != nil || filepath.Base(path) != "primary.json" {
		t.Fatalf("Path() = %q, %v", path, err)
	}
	if err := Clear(root, "primary"); err != nil {
		t.Fatal(err)
	}
	_, found, err = Load(root, "primary")
	if err != nil || found {
		t.Fatalf("Load() after Clear = found %v, err %v", found, err)
	}
}

func TestWithActiveParksAndRestoresPlansDeterministically(t *testing.T) {
	first := Anchor{
		Version: Version, Target: "primary", PlanID: "plan_0123456789abcdef0123456789abcdef", BundleID: "booking-dates",
		Parked: []SavedPlan{{PlanID: "plan_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BundleID: "older-feature"}},
	}
	second := first.WithActive(SavedPlan{PlanID: "plan_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BundleID: "payment-dates"})
	if second.BundleID != "payment-dates" || second.PlanID != "plan_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || len(second.Parked) != 2 {
		t.Fatalf("switched anchor = %#v", second)
	}
	if saved, found := second.FindParked("booking-dates"); !found || saved.PlanID != first.PlanID {
		t.Fatalf("booking plan was not parked: %#v", second.Parked)
	}
	restored := second.WithActive(SavedPlan{PlanID: first.PlanID, BundleID: first.BundleID})
	if restored.BundleID != first.BundleID || len(restored.Parked) != 2 {
		t.Fatalf("restored anchor = %#v", restored)
	}
	if _, found := restored.FindParked("booking-dates"); found {
		t.Fatalf("active plan remained parked: %#v", restored.Parked)
	}
	if err := restored.Validate("primary"); err != nil {
		t.Fatal(err)
	}
}

func TestParkAndFinishRetainOnlyIntendedLocalSelectors(t *testing.T) {
	anchor := Anchor{
		Version: Version, Target: "primary", PlanID: "plan_0123456789abcdef0123456789abcdef", BundleID: "booking-dates",
		Parked: []SavedPlan{{PlanID: "plan_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BundleID: "older-feature"}},
	}
	parked := anchor.ParkActive()
	if parked.HasActive() || len(parked.Parked) != 2 {
		t.Fatalf("parked anchor = %#v", parked)
	}
	if err := parked.Validate("primary"); err != nil {
		t.Fatal(err)
	}
	resumed := parked.WithActive(SavedPlan{PlanID: anchor.PlanID, BundleID: anchor.BundleID})
	if !resumed.HasActive() || resumed.PlanID != anchor.PlanID || len(resumed.Parked) != 1 {
		t.Fatalf("resumed anchor = %#v", resumed)
	}
	finished := resumed.WithoutActive()
	if finished.HasActive() || len(finished.Parked) != 1 || finished.Parked[0].BundleID != "older-feature" {
		t.Fatalf("finished anchor = %#v", finished)
	}
}

func TestNewIDAndValidation(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "plan_") || len(id) != len("plan_")+32 {
		t.Fatalf("NewID() = %q", id)
	}
	anchor := Anchor{Version: Version, Target: "primary", PlanID: id, BundleID: "feature"}
	if err := anchor.Validate("primary"); err != nil {
		t.Fatal(err)
	}
	anchor.PlanID = "not-a-plan"
	if err := anchor.Validate("primary"); err == nil {
		t.Fatal("Validate accepted invalid plan id")
	}
}
