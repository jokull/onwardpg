package protocol

import "testing"

func TestRebindAnswersCarriesExactScopeAndReportsInvalidation(t *testing.T) {
	previous := Answers{
		ProtocolVersion: Version, CurrentFingerprint: "old-current", DesiredFingerprint: "old-desired",
		Answers: []Answer{
			{Kind: "rename_table", Key: "table:app:old", Value: "table:app:new"},
			{Kind: "drop", Key: "table:app:obsolete", Value: "drop"},
		},
	}
	oldQuestions := []Question{
		{Kind: "rename_table", Key: "table:app:old", Choices: []string{"table:app:new", "create"}, CurrentFingerprint: "old-current", DesiredFingerprint: "old-desired", ScopeFingerprint: "sha256:rename"},
		{Kind: "drop", Key: "table:app:obsolete", Choices: []string{"drop"}, CurrentFingerprint: "old-current", DesiredFingerprint: "old-desired", ScopeFingerprint: "sha256:drop-old"},
	}
	newQuestions := []Question{
		{Kind: "rename_table", Key: "table:app:old", Choices: []string{"table:app:new", "create"}, ScopeFingerprint: "sha256:rename"},
		{Kind: "drop", Key: "table:app:obsolete", Choices: []string{"drop"}, ScopeFingerprint: "sha256:drop-new"},
		{Kind: "set_not_null", Key: "column:app:users:email", Choices: []string{"direct", "staged"}, ScopeFingerprint: "sha256:null"},
	}
	report, err := RebindAnswers(previous, oldQuestions, newQuestions, "new-current", "new-desired")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Answers.Answers) != 1 || report.Answers.Answers[0].Kind != "rename_table" || report.Answers.Answers[0].QuestionFingerprint != "sha256:rename" {
		t.Fatalf("carried answers = %#v", report.Answers.Answers)
	}
	if len(report.Invalidated) != 1 || report.Invalidated[0].Reason != "question_scope_changed" {
		t.Fatalf("invalidated = %#v", report.Invalidated)
	}
	if len(report.Unanswered) != 2 {
		t.Fatalf("unanswered = %#v", report.Unanswered)
	}
}

func TestRebindAnswersRejectsQuestionFingerprintContradiction(t *testing.T) {
	previous := Answers{ProtocolVersion: Version, CurrentFingerprint: "old-current", DesiredFingerprint: "old-desired", Answers: []Answer{{
		Kind: "drop", Key: "table:app:old", Value: "drop", QuestionFingerprint: "wrong",
	}}}
	questions := []Question{{Kind: "drop", Key: "table:app:old", Choices: []string{"drop"}, CurrentFingerprint: "old-current", DesiredFingerprint: "old-desired", ScopeFingerprint: "right"}}
	report, err := RebindAnswers(previous, questions, questions, "new-current", "new-desired")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Answers.Answers) != 0 || len(report.Invalidated) != 1 || report.Invalidated[0].Reason != "previous_scope_invalid" {
		t.Fatalf("report = %#v", report)
	}
}
