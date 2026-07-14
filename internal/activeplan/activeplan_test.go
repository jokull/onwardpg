package activeplan

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
