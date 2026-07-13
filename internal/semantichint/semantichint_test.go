package semantichint

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestRenameDecisionOffersReusableSemanticHints(t *testing.T) {
	current, desired, question := renameFixture(t)
	decisions, err := Decisions([]protocol.Question{question}, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	wantRename := protocol.Hint{
		Kind: "rename", Object: "column",
		From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"},
	}
	wantDrop := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	if len(decisions) != 1 || len(decisions[0].Choices) != 2 || !reflect.DeepEqual(decisions[0].Choices[0].Hint, wantRename) || !reflect.DeepEqual(decisions[0].Choices[1].Hint, wantDrop) {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestRenameHintBecomesFingerprintBoundInternalAnswer(t *testing.T) {
	current, desired, question := renameFixture(t)
	hint := protocol.Hint{
		Kind: "rename", Object: "column",
		From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"},
	}
	matched, err := MatchQuestions([]protocol.Question{question}, []protocol.Hint{hint}, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !matched.Used[0] || len(matched.Answers) != 1 {
		t.Fatalf("match = %#v", matched)
	}
	answer := matched.Answers[0]
	if answer.Kind != question.Kind || answer.Key != question.Key || answer.Value != "column:public:users:display_name" || answer.QuestionFingerprint != question.ScopeFingerprint {
		t.Fatalf("answer = %#v", answer)
	}
}

func TestDropHintResolvesRenameRejectionAndDestructiveConfirmation(t *testing.T) {
	current, desired, rename := renameFixture(t)
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	first, err := MatchQuestions([]protocol.Question{rename}, []protocol.Hint{hint}, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Answers[0].Value; got != "create" {
		t.Fatalf("rename answer = %q", got)
	}
	drop := protocol.Question{
		Kind: "drop", Key: "column:public:users:name", Choices: []string{"drop"}, ScopeFingerprint: "sha256:drop",
	}
	second, err := MatchQuestions([]protocol.Question{drop}, []protocol.Hint{hint}, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Answers[0].Value; got != "drop" {
		t.Fatalf("drop answer = %q", got)
	}
}

func TestContradictoryRenameAndDropHintsFail(t *testing.T) {
	current, desired, question := renameFixture(t)
	hints := []protocol.Hint{
		{Kind: "rename", Object: "column", From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"}},
		{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}},
	}
	_, err := MatchQuestions([]protocol.Question{question}, hints, current, desired)
	if err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("error = %v", err)
	}
}

func renameFixture(t *testing.T) (*pgschema.Snapshot, *pgschema.Snapshot, protocol.Question) {
	t.Helper()
	table := pgschema.Table{Schema: "public", Name: "users"}
	current := pgschema.New()
	desired := pgschema.New()
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(pgschema.Schema{Name: "public"}); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Column{Table: table.ObjectID(), Name: "name", Type: "text"}
	after := pgschema.Column{Table: table.ObjectID(), Name: "display_name", Type: "text"}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	return current, desired, protocol.Question{
		Kind: "rename_column", Key: before.ObjectID().String(),
		Choices: []string{after.ObjectID().String(), "create"}, ScopeFingerprint: "sha256:scope",
	}
}
