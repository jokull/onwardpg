package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const DecisionsVersion = "onwardpg.decisions/v1"

// Hint is the complete semantic intent an agent may add to a schema diff.
// It deliberately contains no planner IDs or fingerprints: onwardpg validates
// the statement against the observed diff and adds state binding to its own
// receipt after consumption.
type Hint struct {
	Kind     string   `json:"kind"`
	Object   string   `json:"object,omitempty"`
	Name     []string `json:"name,omitempty"`
	From     []string `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	Action   string   `json:"action,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
}

// Decision is one irreducible ambiguity. Each choice contains the exact hint
// which resolves it; callers can copy that object or construct the same stable
// semantic statement before planning.
type Decision struct {
	Choices []DecisionChoice `json:"choices"`
}

type DecisionChoice struct {
	Hint    Hint     `json:"hint"`
	Hazards []string `json:"hazards,omitempty"`
}

// DecisionReceipt is generated evidence, never an authoring format. Answers
// retain the full internal scope fingerprints while Hints retain the compact
// product intent that produced them.
type DecisionReceipt struct {
	Protocol string  `json:"protocol"`
	Hints    []Hint  `json:"hints,omitempty"`
	Answers  Answers `json:"answers"`
}

func (h Hint) Validate() error {
	if h.Kind == "" {
		return fmt.Errorf("hint kind is required")
	}
	switch h.Kind {
	case "identity":
		// Identity is deliberately narrower than a rename answer: it reaches
		// rename candidacy before the planner has chosen which questions exist.
		// The first vertical slice is table identity, where exporter-generated
		// child names otherwise hide the relation pairing entirely.
		if h.Object != "table" {
			return fmt.Errorf("identity hint currently supports object table")
		}
		if err := validateIdentifier(h.Object, h.From, "from"); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.To, "to"); err != nil {
			return err
		}
		if sameStrings(h.From, h.To) {
			return fmt.Errorf("identity hint from and to must differ")
		}
		return h.reject("name", h.Name, "action", h.Action, "strategy", h.Strategy)
	case "rename":
		if err := h.requireObject(); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.From, "from"); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.To, "to"); err != nil {
			return err
		}
		if sameStrings(h.From, h.To) {
			return fmt.Errorf("rename hint from and to must differ")
		}
		return h.reject("name", h.Name, "action", h.Action, "strategy", h.Strategy)
	case "drop", "preserve":
		if err := h.requireObject(); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.Name, "name"); err != nil {
			return err
		}
		return h.reject("from", h.From, "to", h.To, "action", h.Action, "strategy", h.Strategy)
	case "type_change":
		if h.Object != "" {
			return fmt.Errorf("type_change hint does not accept object; column is implied")
		}
		if err := validateIdentifier("column", h.Name, "name"); err != nil {
			return err
		}
		if h.Strategy != "manual_sql" {
			return fmt.Errorf("type_change hint strategy must be manual_sql")
		}
		return h.reject("from", h.From, "to", h.To, "action", h.Action)
	case "rollout":
		if h.Object != "" {
			return fmt.Errorf("rollout hint does not accept object; column is implied")
		}
		if err := validateIdentifier("column", h.Name, "name"); err != nil {
			return err
		}
		if h.Strategy != "direct" && h.Strategy != "staged" && h.Strategy != "staged_with_backfill" {
			return fmt.Errorf("not_null rollout strategy must be direct, staged, or staged_with_backfill")
		}
		return h.reject("from", h.From, "to", h.To, "action", h.Action)
	case "confirm":
		if err := h.requireObject(); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.Name, "name"); err != nil {
			return err
		}
		if h.Action == "" {
			return fmt.Errorf("confirm hint action is required")
		}
		return h.reject("from", h.From, "to", h.To, "strategy", h.Strategy)
	case "manual_sql":
		if err := h.requireObject(); err != nil {
			return err
		}
		if err := validateIdentifier(h.Object, h.Name, "name"); err != nil {
			return err
		}
		if h.Action == "" {
			return fmt.Errorf("manual_sql hint action is required")
		}
		return h.reject("from", h.From, "to", h.To, "strategy", h.Strategy)
	default:
		return fmt.Errorf("unknown hint kind %q", h.Kind)
	}
}

func (h Hint) requireObject() error {
	if !knownObjectKind(h.Object) {
		return fmt.Errorf("hint object %q is not a modeled PostgreSQL object kind", h.Object)
	}
	return nil
}

func (h Hint) reject(fields ...any) error {
	for i := 0; i < len(fields); i += 2 {
		name := fields[i].(string)
		switch value := fields[i+1].(type) {
		case string:
			if value != "" {
				return fmt.Errorf("%s hint does not accept %s", h.Kind, name)
			}
		case []string:
			if len(value) != 0 {
				return fmt.Errorf("%s hint does not accept %s", h.Kind, name)
			}
		}
	}
	return nil
}

func DecodeHint(data []byte) (Hint, error) {
	var hint Hint
	if err := decodeStrictJSON(data, &hint); err != nil {
		return Hint{}, err
	}
	if err := hint.Validate(); err != nil {
		return Hint{}, err
	}
	return hint, nil
}

func DecodeHints(data []byte) ([]Hint, error) {
	var hints []Hint
	if err := decodeStrictJSON(data, &hints); err != nil {
		return nil, err
	}
	if hints == nil {
		return nil, fmt.Errorf("hints file must contain a JSON array")
	}
	if err := ValidateHints(hints); err != nil {
		return nil, err
	}
	return hints, nil
}

func ValidateHints(hints []Hint) error {
	seen := make(map[string]bool, len(hints))
	for i, hint := range hints {
		if err := hint.Validate(); err != nil {
			return fmt.Errorf("hint %d: %w", i+1, err)
		}
		key, err := hint.CanonicalKey()
		if err != nil {
			return fmt.Errorf("hint %d: %w", i+1, err)
		}
		if seen[key] {
			return fmt.Errorf("hint %d duplicates an earlier hint", i+1)
		}
		seen[key] = true
	}
	return nil
}

func (h Hint) CanonicalKey() (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func CanonicalHints(hints []Hint) ([]Hint, error) {
	if err := ValidateHints(hints); err != nil {
		return nil, err
	}
	result := append([]Hint(nil), hints...)
	sort.Slice(result, func(i, j int) bool {
		left, _ := result[i].CanonicalKey()
		right, _ := result[j].CanonicalKey()
		return left < right
	})
	return result, nil
}

func decodeStrictJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key must be a string")
			}
			if seen[key] {
				return fmt.Errorf("JSON object contains duplicate key %q", key)
			}
			seen[key] = true
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("JSON object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("JSON array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func validateIdentifier(kind string, parts []string, field string) error {
	want := identifierParts(kind)
	if want == 0 {
		return fmt.Errorf("unknown object kind %q", kind)
	}
	if len(parts) != want {
		return fmt.Errorf("%s %s must contain %d identifier parts", kind, field, want)
	}
	for index, part := range parts {
		// A no-argument routine has an intentionally empty signature. The array
		// position still distinguishes it losslessly from schema and name.
		if part == "" && !(kind == "routine" && index == 2) || strings.ContainsRune(part, '\x00') {
			return fmt.Errorf("%s %s contains an empty or invalid identifier", kind, field)
		}
	}
	return nil
}

func knownObjectKind(kind string) bool { return identifierParts(kind) != 0 }

func identifierParts(kind string) int {
	switch kind {
	case "schema":
		return 1
	case "extension", "enum", "domain", "composite", "sequence", "table", "view", "materialized_view", "row_security", "foreign_table":
		return 2
	case "column", "constraint", "index", "routine", "trigger", "policy", "table_privilege":
		return 3
	default:
		return 0
	}
}

func sameStrings(left, right []string) bool {
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
