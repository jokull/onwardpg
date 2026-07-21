package main

import (
	"strings"
	"testing"
)

func TestCheckEventsRejectsSkippedDescendant(t *testing.T) {
	log := strings.NewReader("{\"Action\":\"pass\",\"Test\":\"TestRequired\"}\n" +
		"{\"Action\":\"skip\",\"Test\":\"TestRequired/variant\"}\n")
	err := checkEvents(log, []string{"TestRequired"})
	if err == nil || !strings.Contains(err.Error(), "TestRequired/variant") {
		t.Fatalf("skipped descendant error = %v", err)
	}
}

func TestCheckEventsRequiresEveryAuthoritativePass(t *testing.T) {
	log := strings.NewReader("{\"Action\":\"pass\",\"Test\":\"TestOne\"}\n")
	err := checkEvents(log, []string{"TestOne", "TestTwo"})
	if err == nil || !strings.Contains(err.Error(), "TestTwo") {
		t.Fatalf("missing authoritative pass error = %v", err)
	}
}

func TestCheckEventsAcceptsCompleteRun(t *testing.T) {
	log := strings.NewReader("{\"Action\":\"pass\",\"Test\":\"TestOne\"}\n" +
		"{\"Action\":\"pass\",\"Test\":\"TestTwo\"}\n")
	if err := checkEvents(log, []string{"TestOne", "TestTwo"}); err != nil {
		t.Fatal(err)
	}
}
