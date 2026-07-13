package semantichint

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
)

func TestResolveConsumesSemanticDropAcrossPlannerStages(t *testing.T) {
	current, desired, _ := renameFixture(t)
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	resolution, err := Resolve(current, desired, []protocol.Hint{hint}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.Planned || len(resolution.Answers.Answers) != 2 || len(resolution.Questions) != 2 || len(resolution.Hints) != 1 {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestResolveRejectsUnconsumedIntent(t *testing.T) {
	current, desired, _ := renameFixture(t)
	_, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "drop", Object: "column", Name: []string{"public", "missing", "name"},
	}}, graphplan.Options{})
	if err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("error = %v", err)
	}
}
