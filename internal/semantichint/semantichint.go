// Package semantichint translates stable product intent into the planner's
// internal fingerprint-bound answer protocol. The planner remains the source
// of valid choices; hints can only consume choices which an observed graph diff
// actually produces.
package semantichint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

type Match struct {
	Answers []protocol.Answer
	Used    map[int]bool
}

func Decisions(questions []protocol.Question, current, desired *pgschema.Snapshot) ([]protocol.Decision, error) {
	result := make([]protocol.Decision, 0, len(questions))
	for _, question := range questions {
		choices, err := choicesForQuestion(question, current, desired)
		if err != nil {
			return nil, fmt.Errorf("decision %s:%s: %w", question.Kind, question.Key, err)
		}
		result = append(result, protocol.Decision{Choices: choices})
	}
	return result, nil
}

// MatchQuestions consumes every hint which resolves a currently reachable
// question. A hint may deliberately resolve more than one staged internal
// question: for example one semantic drop first means "not a rename" and later
// confirms the destructive removal.
func MatchQuestions(questions []protocol.Question, hints []protocol.Hint, current, desired *pgschema.Snapshot) (Match, error) {
	result := Match{Used: make(map[int]bool)}
	for _, question := range questions {
		var matched []struct {
			index  int
			answer protocol.Answer
		}
		for index, hint := range hints {
			answer, match, err := matchQuestion(question, hint, current, desired)
			if err != nil {
				return Match{}, fmt.Errorf("hint %d: %w", index+1, err)
			}
			if match {
				matched = append(matched, struct {
					index  int
					answer protocol.Answer
				}{index: index, answer: answer})
			}
		}
		if len(matched) > 1 {
			indices := make([]int, len(matched))
			for i := range matched {
				indices[i] = matched[i].index + 1
			}
			return Match{}, fmt.Errorf("contradictory hints %v resolve %s:%s", indices, question.Kind, question.Key)
		}
		if len(matched) == 0 {
			continue
		}
		selected := matched[0]
		result.Used[selected.index] = true
		result.Answers = append(result.Answers, selected.answer)
	}
	return result, nil
}

func choicesForQuestion(question protocol.Question, current, desired *pgschema.Snapshot) ([]protocol.DecisionChoice, error) {
	if isIdentityRenameQuestion(question.Kind) {
		from, ok := findID(current, question.Key)
		if !ok {
			return nil, fmt.Errorf("current rename object %q is missing", question.Key)
		}
		object, fromName, err := reference(from)
		if err != nil {
			return nil, err
		}
		var result []protocol.DecisionChoice
		for _, value := range question.Choices {
			if value == "preserve" {
				result = append(result, protocol.DecisionChoice{
					Hint: protocol.Hint{Kind: "preserve", Object: object, Name: fromName},
				})
				continue
			}
			if value == "create" {
				result = append(result, protocol.DecisionChoice{
					Hint:    protocol.Hint{Kind: "drop", Object: object, Name: fromName},
					Hazards: []string{"data_loss"},
				})
				continue
			}
			to, ok := findID(desired, value)
			if !ok {
				return nil, fmt.Errorf("desired rename candidate %q is missing", value)
			}
			toObject, toName, err := reference(to)
			if err != nil {
				return nil, err
			}
			if toObject != object {
				return nil, fmt.Errorf("rename changes object kind from %s to %s", object, toObject)
			}
			result = append(result, protocol.DecisionChoice{Hint: protocol.Hint{
				Kind: "rename", Object: object, From: fromName, To: toName,
			}})
		}
		return result, nil
	}

	id, ok := findID(current, question.Key)
	if !ok {
		id, ok = findID(desired, question.Key)
	}
	if !ok {
		return nil, fmt.Errorf("participating object %q is missing", question.Key)
	}
	object, name, err := reference(id)
	if err != nil {
		return nil, err
	}
	switch question.Kind {
	case "drop":
		return []protocol.DecisionChoice{{
			Hint: protocol.Hint{Kind: "drop", Object: object, Name: name}, Hazards: []string{"data_loss"},
		}}, nil
	case "type_change":
		return []protocol.DecisionChoice{{
			Hint: protocol.Hint{Kind: "type_change", Name: name, Strategy: "manual_sql"}, Hazards: []string{"manual_sql"},
		}}, nil
	case "set_not_null":
		result := make([]protocol.DecisionChoice, 0, len(question.Choices))
		for _, strategy := range question.Choices {
			hazards := []string{"access_exclusive_lock"}
			if strategy != "direct" {
				hazards = []string{"table_scan", "staged_rollout"}
			}
			result = append(result, protocol.DecisionChoice{Hint: protocol.Hint{
				Kind: "rollout", Name: name, Strategy: strategy,
			}, Hazards: hazards})
		}
		return result, nil
	case "rename_backfill_strategy":
		result := make([]protocol.DecisionChoice, 0, len(question.Choices))
		for _, strategy := range question.Choices {
			hazards := []string{"manual_sql"}
			if strategy == "single_transaction" {
				hazards = []string{"unbounded_update", "long_transaction", "wal_volume", "replica_lag"}
			} else if strategy == "split_plan" {
				hazards = []string{"additional_deployment"}
			}
			result = append(result, protocol.DecisionChoice{Hint: protocol.Hint{
				Kind: "rename_backfill", Name: name, Strategy: strategy,
			}, Hazards: hazards})
		}
		return result, nil
	}

	if isManualQuestion(question.Kind) {
		return []protocol.DecisionChoice{{Hint: protocol.Hint{
			Kind: "manual_sql", Action: question.Kind, Object: object, Name: name,
		}, Hazards: []string{"manual_sql"}}}, nil
	}

	if len(question.Choices) != 1 || question.AllowsFreeform {
		return nil, fmt.Errorf("question shape has no semantic hint mapping")
	}
	return []protocol.DecisionChoice{{
		Hint:    protocol.Hint{Kind: "confirm", Action: question.Kind, Object: object, Name: name},
		Hazards: confirmationHazards(question.Kind),
	}}, nil
}

func matchQuestion(question protocol.Question, hint protocol.Hint, current, desired *pgschema.Snapshot) (protocol.Answer, bool, error) {
	if err := hint.Validate(); err != nil {
		return protocol.Answer{}, false, err
	}
	answer := protocol.Answer{Kind: question.Kind, Key: question.Key, QuestionFingerprint: question.ScopeFingerprint}
	if isIdentityRenameQuestion(question.Kind) {
		from, ok := findID(current, question.Key)
		if !ok {
			return answer, false, nil
		}
		object, fromName, err := reference(from)
		if err != nil {
			return answer, false, err
		}
		switch {
		case hint.Kind == "preserve" && hint.Object == object && equal(hint.Name, fromName) && contains(question.Choices, "preserve"):
			answer.Value = "preserve"
			return answer, true, nil
		case hint.Kind == "drop" && hint.Object == object && equal(hint.Name, fromName):
			answer.Value = "create"
			return answer, true, nil
		case (hint.Kind == "rename" || hint.Kind == "identity") && hint.Object == object && equal(hint.From, fromName):
			for _, choice := range question.Choices {
				to, ok := findID(desired, choice)
				if !ok {
					continue
				}
				toObject, toName, err := reference(to)
				if err != nil {
					return answer, false, err
				}
				if toObject == hint.Object && equal(hint.To, toName) {
					answer.Value = choice
					return answer, true, nil
				}
			}
		}
		return answer, false, nil
	}

	id, ok := findID(current, question.Key)
	if !ok {
		id, ok = findID(desired, question.Key)
	}
	if !ok {
		return answer, false, nil
	}
	object, name, err := reference(id)
	if err != nil {
		return answer, false, err
	}
	if !equal(hint.Name, name) {
		return answer, false, nil
	}
	if hint.Kind != "type_change" && hint.Kind != "rollout" && hint.Kind != "rename_backfill" && hint.Object != object {
		return answer, false, nil
	}
	switch question.Kind {
	case "drop":
		if hint.Kind == "drop" {
			answer.Value = "drop"
			return answer, true, nil
		}
	case "type_change":
		if hint.Kind == "type_change" && object == "column" {
			answer.Value = hint.Strategy
			return answer, true, nil
		}
	case "set_not_null":
		if hint.Kind == "rollout" && object == "column" && contains(question.Choices, hint.Strategy) {
			answer.Value = hint.Strategy
			return answer, true, nil
		}
	case "rename_backfill_strategy":
		if hint.Kind == "rename_backfill" && object == "column" && contains(question.Choices, hint.Strategy) {
			answer.Value = hint.Strategy
			return answer, true, nil
		}
	default:
		if isManualQuestion(question.Kind) && hint.Kind == "manual_sql" && hint.Action == question.Kind {
			displayName := strings.Join(name, ".")
			answer.Value = "provided"
			answer.Manual = &protocol.ManualWork{
				Summary:       "ONWARDPG TODO: provide " + question.Kind + " SQL for " + displayName,
				ExecutionMode: "transactional",
				Statements: []string{
					"-- ONWARDPG TODO: replace this comment with reviewed SQL for " + question.Kind + " on " + displayName + ".\n" +
						"-- Planner analysis: " + question.Message + "\n" +
						"-- Expected effect: complete the named operation and converge to the desired catalog state.\n" +
						"-- Add boolean assertions to verify.sql for every data-dependent assumption.",
				},
			}
			if question.Kind == "rename_backfill" {
				answer.Manual.VerificationSQL = []string{
					"SELECT false; -- ONWARDPG TODO: replace with a boolean old/new equality assertion",
				}
			}
			return answer, true, nil
		}
		if hint.Kind == "confirm" && hint.Action == question.Kind && len(question.Choices) == 1 {
			answer.Value = question.Choices[0]
			return answer, true, nil
		}
	}
	return answer, false, nil
}

func isManualQuestion(kind string) bool {
	switch kind {
	case "refresh_materialized_view", "partition_reconfiguration", "backfill_not_null", "rename_compatibility_bridge", "rename_backfill":
		return true
	default:
		return false
	}
}

func isIdentityRenameQuestion(kind string) bool {
	return strings.HasPrefix(kind, "rename_") && kind != "rename_compatibility_bridge" && kind != "rename_backfill_strategy" && kind != "rename_backfill"
}

func confirmationHazards(kind string) []string {
	switch kind {
	case "drop_identity", "rebuild_materialized_view":
		return []string{"data_loss"}
	case "alter_policy", "replace_policy", "relax_row_security", "revoke_grant_option":
		return []string{"authorization_change"}
	default:
		return nil
	}
}

func findID(snapshot *pgschema.Snapshot, value string) (pgschema.ID, bool) {
	if snapshot == nil {
		return pgschema.ID{}, false
	}
	for _, id := range snapshot.IDs() {
		if id.String() == value {
			return id, true
		}
	}
	return pgschema.ID{}, false
}

func reference(id pgschema.ID) (string, []string, error) {
	var name []string
	switch id.Kind {
	case pgschema.KindSchema:
		name = []string{id.Name}
	case pgschema.KindColumn, pgschema.KindConstraint, pgschema.KindIndex, pgschema.KindTrigger, pgschema.KindPolicy, pgschema.KindPrivilege:
		name = []string{id.Schema, id.Name, id.Part}
	case pgschema.KindRoutine:
		name = []string{id.Schema, id.Name, id.Signature}
	default:
		name = []string{id.Schema, id.Name}
	}
	if id.Kind == "" || id.Name == "" {
		return "", nil, fmt.Errorf("invalid graph object identity %#v", id)
	}
	return string(id.Kind), name, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func Unused(hints []protocol.Hint, used map[int]bool) []protocol.Hint {
	var result []protocol.Hint
	for index, hint := range hints {
		if !used[index] {
			result = append(result, hint)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := result[i].CanonicalKey()
		right, _ := result[j].CanonicalKey()
		return left < right
	})
	return result
}
