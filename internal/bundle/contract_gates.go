package bundle

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
)

const contractGateOverridesPath = "contract-gate-overrides.json"

// ContractGates returns the effective, fully receipted gates for an artifact.
// Generated plans use plan.json directly. An edited phase may replace only a
// generated ONWARDPG TODO Boolean through a separately digested override file.
func ContractGates(artifact Artifact) ([]protocol.ContractGate, error) {
	var result protocol.Result
	if err := json.Unmarshal(artifact.Files["plan.json"], &result); err != nil {
		return nil, fmt.Errorf("decode plan.json: %w", err)
	}
	return effectiveContractGates(artifact, result)
}

func effectiveContractGates(artifact Artifact, result protocol.Result) ([]protocol.ContractGate, error) {
	gates := append([]protocol.ContractGate(nil), result.ContractGates...)
	if artifact.Manifest.ContractGateOverridesDigest == "" {
		for _, gate := range gates {
			if strings.Contains(gate.BooleanSQL, "ONWARDPG TODO") && artifact.Manifest.State == string(protocol.Planned) {
				return nil, fmt.Errorf("planned contract gate %q retains an unresolved ONWARDPG TODO", gate.ID)
			}
		}
		return gates, nil
	}
	body, exists := artifact.Files[contractGateOverridesPath]
	if !exists || Digest(body) != artifact.Manifest.ContractGateOverridesDigest {
		return nil, fmt.Errorf("contract gate override receipt is missing or has the wrong digest")
	}
	var overrides []protocol.ContractGate
	if err := json.Unmarshal(body, &overrides); err != nil {
		return nil, fmt.Errorf("decode contract gate overrides: %w", err)
	}
	byID := make(map[string]int, len(gates))
	needed := make(map[string]bool)
	for index, gate := range gates {
		byID[gate.ID] = index
		if strings.Contains(gate.BooleanSQL, "ONWARDPG TODO") {
			needed[gate.ID] = true
		}
	}
	seen := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		index, exists := byID[override.ID]
		if !exists || seen[override.ID] || !needed[override.ID] {
			return nil, fmt.Errorf("contract gate override %q is missing, duplicated, or does not target a generated placeholder", override.ID)
		}
		base := gates[index]
		overrideSQL := override.BooleanSQL
		base.BooleanSQL, override.BooleanSQL = "", ""
		if !reflect.DeepEqual(base, override) {
			return nil, fmt.Errorf("contract gate override %q changes generator-owned metadata", override.ID)
		}
		query := strings.TrimSpace(overrideSQL)
		upper := strings.ToUpper(query)
		if query == "" || strings.Contains(query, "ONWARDPG TODO") || (!strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "WITH ")) {
			return nil, fmt.Errorf("contract gate override %q must contain one resolved read-only Boolean SELECT", override.ID)
		}
		gates[index].BooleanSQL = query
		seen[override.ID] = true
	}
	for id := range needed {
		if !seen[id] {
			return nil, fmt.Errorf("placeholder contract gate %q has no edited SQL override", id)
		}
	}
	return gates, nil
}
