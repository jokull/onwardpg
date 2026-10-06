// Package driftcheck compares a replayed onwardpg history head with an
// explicitly supplied live PostgreSQL catalog. It is diagnostic only.
package driftcheck

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

type Difference struct {
	Kind     string          `json:"kind"`
	ObjectID string          `json:"object_id"`
	Expected json.RawMessage `json:"expected,omitempty"`
	Actual   json.RawMessage `json:"actual,omitempty"`
}

type Report struct {
	Outcome             string       `json:"status"`
	Target              string       `json:"target"`
	HistoryHead         string       `json:"history_head"`
	ExpectedFingerprint string       `json:"expected_fingerprint"`
	ActualFingerprint   string       `json:"actual_fingerprint"`
	Differences         []Difference `json:"differences,omitempty"`
	// Unsupported lists catalog state, on either side, that the planner cannot
	// model. It is the same set `diff` reports; a result without entries means
	// the catalog holds nothing the planner would refuse.
	Unsupported []string  `json:"unsupported,omitempty"`
	Ignored     []string  `json:"ignored,omitempty"`
	Observer    *Observer `json:"observer,omitempty"`
}

// Observer describes environmental catalog state proven to belong only to a
// dedicated read-only inspection role and projected before comparison.
type Observer struct {
	Role            string   `json:"role"`
	DatabaseOwner   string   `json:"database_owner"`
	Mode            string   `json:"mode"`
	ProjectedAccess []string `json:"projected_access,omitempty"`
}

func Compare(target, historyHead string, expected, actual *pgschema.Snapshot) (Report, error) {
	if target == "" || historyHead == "" || expected == nil || actual == nil {
		return Report{}, fmt.Errorf("target, history head, expected, and actual schemas are required")
	}
	// An explicitly ignored object may exist on only one side (for example a
	// framework journal omitted from exported DDL). Compare both catalogs under
	// the same observed policy while retaining that evidence in the report.
	expected, actual, err := source.AlignIgnoreReceipts(expected, actual)
	if err != nil {
		return Report{}, err
	}
	expectedFingerprint, err := expected.Fingerprint()
	if err != nil {
		return Report{}, err
	}
	actualFingerprint, err := actual.Fingerprint()
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Outcome: "drift_free", Target: target,
		HistoryHead: historyHead, ExpectedFingerprint: expectedFingerprint, ActualFingerprint: actualFingerprint,
		Ignored: expected.Ignored(), Unsupported: unionSorted(expected.Unsupported(), actual.Unsupported()),
	}
	for _, item := range change.Between(expected, actual) {
		difference := Difference{ObjectID: item.ID.String()}
		switch item.Kind {
		case change.Drop:
			difference.Kind = "missing_in_actual"
			difference.Expected, err = json.Marshal(item.Before)
		case change.Create:
			difference.Kind = "unexpected_in_actual"
			difference.Actual, err = json.Marshal(item.After)
		case change.Modify:
			difference.Kind = "changed_in_actual"
			difference.Expected, err = json.Marshal(item.Before)
			if err == nil {
				difference.Actual, err = json.Marshal(item.After)
			}
		default:
			return Report{}, fmt.Errorf("unknown drift change kind %q", item.Kind)
		}
		if err != nil {
			return Report{}, fmt.Errorf("encode drift object %s: %w", item.ID, err)
		}
		report.Differences = append(report.Differences, difference)
	}
	// Unsupported state outranks drift so that drift check and diff agree on
	// whether a catalog is supported, while the differences stay in the same
	// report: one run shows everything.
	switch {
	case len(report.Unsupported) > 0:
		report.Outcome = "unsupported"
	case len(report.Differences) > 0 || expectedFingerprint != actualFingerprint:
		report.Outcome = "drifted"
	}
	return report, nil
}

func unionSorted(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	var result []string
	for _, values := range [][]string{first, second} {
		for _, value := range values {
			if !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
	}
	sort.Strings(result)
	return result
}
