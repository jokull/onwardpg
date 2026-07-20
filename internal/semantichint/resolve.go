package semantichint

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// Resolution is the complete evidence produced while consuming semantic hints
// through the planner's dependency-ordered question stages.
type Resolution struct {
	Result    protocol.Result
	Answers   protocol.Answers
	Questions []protocol.Question
	Hints     []protocol.Hint
	Deferred  []protocol.Hint
}

// Resolve plans from snapshots using only semantic hints. It is shared by
// high-level CLI surfaces which do not have a stored bundle decision history.
func Resolve(current, desired *pgschema.Snapshot, hints []protocol.Hint, options graphplan.Options) (Resolution, error) {
	if err := protocol.ValidateHints(hints); err != nil {
		return Resolution{}, err
	}
	identityUsed, err := ApplyIdentityHints(current, desired, hints, &options)
	if err != nil {
		return Resolution{}, err
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		return Resolution{}, err
	}
	resolution := Resolution{Result: result, Answers: protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: result.CurrentFingerprint, DesiredFingerprint: result.DesiredFingerprint,
	}}
	used := identityUsed
	for iteration := 0; iteration <= len(hints)*2+1; iteration++ {
		resolution.Questions = mergeQuestions(resolution.Questions, resolution.Result.Questions)
		if resolution.Result.Status != protocol.NeedsInput {
			if err := rejectUnused(hints, used); err != nil {
				return Resolution{}, err
			}
			resolution.Hints = usedHints(hints, used)
			return resolution, nil
		}
		matched, err := MatchQuestions(resolution.Result.Questions, hints, current, desired)
		if err != nil {
			return Resolution{}, err
		}
		if len(matched.Answers) == 0 {
			deferred, err := classifyDeferredHints(current, desired, hints, used, options)
			if err != nil {
				return Resolution{}, err
			}
			resolution.Hints = usedHints(hints, used)
			resolution.Deferred = deferred
			return resolution, nil
		}
		for index := range matched.Used {
			used[index] = true
		}
		resolution.Answers.Answers = append(resolution.Answers.Answers, matched.Answers...)
		resolution.Result, err = graphplan.Build(current, desired, resolution.Answers, options)
		if err != nil {
			return Resolution{}, err
		}
	}
	return Resolution{}, fmt.Errorf("semantic hint planning did not converge")
}

func classifyDeferredHints(current, desired *pgschema.Snapshot, hints []protocol.Hint, used map[int]bool, options graphplan.Options) ([]protocol.Hint, error) {
	var deferred []protocol.Hint
	var impossible []string
	for index, hint := range hints {
		if used[index] {
			continue
		}
		reachable, err := hintReachable(current, desired, hint, options)
		if err != nil {
			return nil, err
		}
		if reachable {
			deferred = append(deferred, hint)
			continue
		}
		key, _ := hint.CanonicalKey()
		impossible = append(impossible, key)
	}
	if len(impossible) > 0 {
		sort.Strings(impossible)
		return nil, fmt.Errorf("unused semantic hints: %s", strings.Join(impossible, ", "))
	}
	deferred, _ = protocol.CanonicalHints(deferred)
	return deferred, nil
}

// DeferredHints validates that each supplied hint can be reached after one or
// more currently unanswered dependency-ordered questions. It never consumes or
// receipts those hints.
func DeferredHints(current, desired *pgschema.Snapshot, hints []protocol.Hint, options graphplan.Options) ([]protocol.Hint, error) {
	return classifyDeferredHints(current, desired, hints, map[int]bool{}, options)
}

// hintReachable explores dependency-ordered public choices without accepting
// any of them. It is used only to distinguish a valid later hint from an
// impossible one; deferred hints are not added to the answer receipt.
func hintReachable(current, desired *pgschema.Snapshot, target protocol.Hint, options graphplan.Options) (bool, error) {
	type state struct{ answers protocol.Answers }
	initial, err := graphplan.Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		return false, err
	}
	queue := []state{{answers: protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: initial.CurrentFingerprint, DesiredFingerprint: initial.DesiredFingerprint}}}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		if len(seen) > 1024 {
			return false, fmt.Errorf("semantic hint reachability exceeded 1024 decision states")
		}
		currentState := queue[0]
		queue = queue[1:]
		keyBytes, _ := json.Marshal(currentState.answers.Answers)
		key := string(keyBytes)
		if seen[key] {
			continue
		}
		seen[key] = true
		plan, err := graphplan.Build(current, desired, currentState.answers, options)
		if err != nil {
			return false, err
		}
		if plan.Status != protocol.NeedsInput {
			continue
		}
		matched, err := MatchQuestions(plan.Questions, []protocol.Hint{target}, current, desired)
		if err != nil {
			return false, err
		}
		if len(matched.Answers) > 0 {
			return true, nil
		}
		for _, question := range plan.Questions {
			choices, err := choicesForQuestion(question, current, desired)
			if err != nil {
				return false, err
			}
			for _, choice := range choices {
				answer, err := MatchQuestions([]protocol.Question{question}, []protocol.Hint{choice.Hint}, current, desired)
				if err != nil || len(answer.Answers) != 1 {
					continue
				}
				next := currentState.answers
				next.Answers = append(append([]protocol.Answer(nil), currentState.answers.Answers...), answer.Answers[0])
				queue = append(queue, state{answers: next})
			}
		}
	}
	return false, nil
}

// ApplyIdentityHints validates the source and desired endpoints of explicit
// identity assertions before they influence graph rename candidacy. It returns
// the consumed hint indices so every accepted assertion remains receipted and
// unused assertions still fail planning.
func ApplyIdentityHints(current, desired *pgschema.Snapshot, hints []protocol.Hint, options *graphplan.Options) (map[int]bool, error) {
	used := make(map[int]bool)
	for index, hint := range hints {
		if hint.Kind != "identity" {
			continue
		}
		if hint.Object != "table" {
			return nil, fmt.Errorf("identity hint %d has unsupported object %q", index+1, hint.Object)
		}
		from := pgschema.Table{Schema: hint.From[0], Name: hint.From[1]}.ObjectID()
		to := pgschema.Table{Schema: hint.To[0], Name: hint.To[1]}.ObjectID()
		if _, ok := current.Object(from); !ok {
			return nil, fmt.Errorf("identity hint %d source %s is absent from the current schema", index+1, from)
		}
		if _, ok := desired.Object(to); !ok {
			return nil, fmt.Errorf("identity hint %d target %s is absent from the desired schema", index+1, to)
		}
		options.IdentityHints = append(options.IdentityHints, hint)
		used[index] = true
	}
	return used, nil
}

func rejectUnused(hints []protocol.Hint, used map[int]bool) error {
	var unused []string
	for index, hint := range hints {
		if used[index] {
			continue
		}
		key, _ := hint.CanonicalKey()
		unused = append(unused, key)
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	return fmt.Errorf("unused semantic hints: %s", strings.Join(unused, ", "))
}

func usedHints(hints []protocol.Hint, used map[int]bool) []protocol.Hint {
	result := make([]protocol.Hint, 0, len(used))
	for index, hint := range hints {
		if used[index] {
			result = append(result, hint)
		}
	}
	result, _ = protocol.CanonicalHints(result)
	return result
}

func mergeQuestions(first, second []protocol.Question) []protocol.Question {
	result := append([]protocol.Question(nil), first...)
	seen := make(map[string]bool, len(result))
	for _, question := range result {
		seen[question.Kind+":"+question.Key] = true
	}
	for _, question := range second {
		id := question.Kind + ":" + question.Key
		if !seen[id] {
			result = append(result, question)
			seen[id] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+":"+result[i].Key < result[j].Kind+":"+result[j].Key
	})
	return result
}
