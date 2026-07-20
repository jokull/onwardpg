// Package driftcheck compares a replayed onwardpg history head with an
// explicitly supplied live PostgreSQL catalog. It is diagnostic only.
package driftcheck

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jokull/onwardpg/internal/change"
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
	Ignored             []string     `json:"ignored,omitempty"`
}

func Compare(target, historyHead string, expected, actual *pgschema.Snapshot) (Report, error) {
	if target == "" || historyHead == "" || expected == nil || actual == nil {
		return Report{}, fmt.Errorf("target, history head, expected, and actual schemas are required")
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
		Ignored: mergeIgnored(expected.Ignored(), actual.Ignored()),
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
	if len(report.Differences) > 0 || expectedFingerprint != actualFingerprint {
		report.Outcome = "drifted"
	}
	return report, nil
}

func mergeIgnored(groups ...[]string) []string {
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, value := range group {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
