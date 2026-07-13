// Package protocol is the stable JSON contract between onwardpg and an agent.
package protocol

import (
	"fmt"
	"sort"
	"strings"
)

const Version = "onwardpg.plan/v1"

type Status string

const (
	Planned     Status = "planned"
	NeedsInput  Status = "needs_input"
	Unsupported Status = "unsupported"
)

type Result struct {
	ProtocolVersion    string      `json:"protocol_version,omitempty"`
	CurrentFingerprint string      `json:"current_fingerprint,omitempty"`
	DesiredFingerprint string      `json:"desired_fingerprint,omitempty"`
	Status             Status      `json:"status"`
	Statements         []Statement `json:"statements,omitempty"`
	Batches            []Batch     `json:"batches,omitempty"`
	Questions          []Question  `json:"questions,omitempty"`
	Ignored            []string    `json:"ignored,omitempty"`
	Unsupported        []string    `json:"unsupported,omitempty"`
}

type Statement struct {
	SQL     string   `json:"sql"`
	Safety  string   `json:"safety"`
	Hazards []string `json:"hazards,omitempty"`
	// Phase allows a caller to split a forward migration across compatible
	// application releases. "expand" is additive/compatible work; "contract"
	// needs an explicit review after old application code is gone.
	Phase string `json:"phase"`
	// NonTransactional is planner metadata used when forming executable
	// batches. It is intentionally not part of the public statement JSON: the
	// batch is the execution boundary exposed by the protocol.
	NonTransactional bool `json:"-"`
	// Manual is present only for operator-authored work. onwardpg records it
	// verbatim instead of inventing a data transformation from schema state.
	Manual *ManualWork `json:"manual,omitempty"`
}

// ManualWork is a fingerprint-bound, operator-authored migration contract.
// Statements are deliberately not parsed or rewritten: their correctness is
// owned by the reviewer who supplied them. VerificationSQL gives an agent a
// concrete postcondition to run before it proceeds to a contract phase.
type ManualWork struct {
	Summary string `json:"summary"`
	// ExecutionMode is required because onwardpg cannot infer whether
	// operator-supplied SQL may run inside a transaction. It is either
	// "transactional" or "non_transactional".
	ExecutionMode   string   `json:"execution_mode"`
	Statements      []string `json:"statements"`
	VerificationSQL []string `json:"verification_sql,omitempty"`
}

type Batch struct {
	ID            string      `json:"id"`
	Phase         string      `json:"phase"`
	Transactional bool        `json:"transactional"`
	Statements    []Statement `json:"statements"`
}

type Question struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Message            string   `json:"message"`
	Key                string   `json:"key"`
	Choices            []string `json:"choices"`
	AllowsFreeform     bool     `json:"allows_freeform,omitempty"`
	CurrentFingerprint string   `json:"current_fingerprint,omitempty"`
	DesiredFingerprint string   `json:"desired_fingerprint,omitempty"`
}

type Answers struct {
	ProtocolVersion    string   `json:"protocol_version,omitempty"`
	CurrentFingerprint string   `json:"current_fingerprint,omitempty"`
	DesiredFingerprint string   `json:"desired_fingerprint,omitempty"`
	Answers            []Answer `json:"answers"`
}

type Answer struct {
	Kind   string      `json:"kind"`
	Key    string      `json:"key"`
	Value  string      `json:"value"`
	Manual *ManualWork `json:"manual,omitempty"`
}

// Resolver validates and consumes a fingerprint-bound answer document. It is
// the only answer-consumption path so stale and unused intent cannot be
// ignored.
type Resolver struct {
	answers map[string]Answer
	used    map[string]bool
}

func (a Answers) Resolver(currentFingerprint, desiredFingerprint string) (*Resolver, error) {
	if a.ProtocolVersion != Version {
		return nil, fmt.Errorf("answer protocol_version is %q, want %q", a.ProtocolVersion, Version)
	}
	if a.CurrentFingerprint != currentFingerprint || a.DesiredFingerprint != desiredFingerprint {
		return nil, fmt.Errorf("answer fingerprints are stale: current=%q desired=%q", a.CurrentFingerprint, a.DesiredFingerprint)
	}
	resolver := &Resolver{answers: make(map[string]Answer, len(a.Answers)), used: make(map[string]bool)}
	for _, answer := range a.Answers {
		if answer.Kind == "" || answer.Key == "" || answer.Value == "" {
			return nil, fmt.Errorf("answer kind, key and value are required")
		}
		id := answer.Kind + ":" + answer.Key
		if existing, exists := resolver.answers[id]; exists {
			if existing.Value != answer.Value {
				return nil, fmt.Errorf("contradictory answers for %s", id)
			}
			return nil, fmt.Errorf("duplicate answer for %s", id)
		}
		resolver.answers[id] = answer
	}
	return resolver, nil
}

func (r *Resolver) Resolve(question Question) (string, bool, error) {
	id := question.Kind + ":" + question.Key
	answer, exists := r.answers[id]
	if !exists {
		return "", false, nil
	}
	if answer.Manual != nil {
		return "", false, fmt.Errorf("answer %s includes manual work but this question does not accept it", id)
	}
	valid := question.AllowsFreeform || len(question.Choices) == 0
	for _, choice := range question.Choices {
		if answer.Value == choice {
			valid = true
			break
		}
	}
	if !valid {
		return "", false, fmt.Errorf("answer %s has invalid value %q", id, answer.Value)
	}
	r.used[id] = true
	return answer.Value, true, nil
}

// ResolveManual resolves an explicit operator-owned work contract. It keeps
// the answer validation path (including fingerprints and unused-answer
// rejection) identical to other ambiguity answers, while refusing to turn an
// acknowledgement into an incomplete successful plan.
func (r *Resolver) ResolveManual(question Question) (ManualWork, bool, error) {
	id := question.Kind + ":" + question.Key
	answer, exists := r.answers[id]
	if !exists {
		return ManualWork{}, false, nil
	}
	if answer.Value != "provided" || answer.Manual == nil {
		return ManualWork{}, false, fmt.Errorf("manual answer %s must use value %q and include manual work", id, "provided")
	}
	work := *answer.Manual
	if strings.TrimSpace(work.Summary) == "" || len(work.Statements) == 0 {
		return ManualWork{}, false, fmt.Errorf("manual answer %s requires summary and statements", id)
	}
	if strings.ContainsAny(work.Summary, "\r\n") {
		return ManualWork{}, false, fmt.Errorf("manual answer %s summary must be one line", id)
	}
	if work.ExecutionMode != "transactional" && work.ExecutionMode != "non_transactional" {
		return ManualWork{}, false, fmt.Errorf("manual answer %s execution_mode must be transactional or non_transactional", id)
	}
	for _, sql := range work.Statements {
		if strings.TrimSpace(sql) == "" {
			return ManualWork{}, false, fmt.Errorf("manual answer %s contains an empty SQL entry", id)
		}
	}
	for _, sql := range work.VerificationSQL {
		if strings.TrimSpace(sql) == "" || strings.ContainsAny(sql, "\r\n") {
			return ManualWork{}, false, fmt.Errorf("manual answer %s verification SQL must be non-empty and one line", id)
		}
	}
	r.used[id] = true
	return work, true, nil
}

func (r *Resolver) ValidateAllUsed() error {
	var unused []string
	for id := range r.answers {
		if !r.used[id] {
			unused = append(unused, id)
		}
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	return fmt.Errorf("unused answers: %v", unused)
}
