package historyinit

import (
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
)

func TestAuthoritativeBaselinePlanDropsStagedContractEvidence(t *testing.T) {
	inventory := protocol.Result{
		ProtocolVersion: protocol.Version,
		Status:          protocol.Planned,
		Statements: []protocol.Statement{{
			ID: "contract-step", SQL: "SELECT 1;", Safety: "review", Phase: protocol.PhaseContract,
			RequiresGates: []string{"writers:legacy"},
		}},
		ContractGates: []protocol.ContractGate{{
			ID: "writers:legacy", Kind: "writer_attestation", RequiredEvidence: []string{"web"},
		}},
		Reconciliations: []protocol.Reconciliation{{TransitionID: "test", Strategy: "assert_only", GateIDs: []string{"writers:legacy"}}},
	}

	result := authoritativeBaselinePlan(inventory, []byte("CREATE TABLE app.events (id bigint);"))
	if len(result.Statements) != 1 || result.Statements[0].Phase != protocol.PhaseExpand || len(result.Batches) != 1 {
		t.Fatalf("baseline replay = %#v", result)
	}
	if len(result.ContractGates) != 0 || len(result.Reconciliations) != 0 {
		t.Fatalf("baseline retained staged contract evidence: %#v", result)
	}
}
