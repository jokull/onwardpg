package verify

import (
	"errors"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

const expandOnlyPlan = `{"batches":[{"id":"batch-expand-001","phase":"expand","transactional":true,"statements":[{"sql":"ALTER TABLE t ADD COLUMN c text;"}]}]}`
const expandAndContractPlan = `{"batches":[{"id":"batch-expand-001","phase":"expand","transactional":true,"statements":[{"sql":"ALTER TABLE t ADD COLUMN c text;"}]},{"id":"batch-contract-001","phase":"contract","transactional":true,"statements":[{"sql":"ALTER TABLE t DROP COLUMN old;"}]}]}`

func generatedEntry(id, plan string) history.Entry {
	transactional := true
	return history.Entry{Directory: id, Artifact: bundle.Artifact{
		Manifest: bundle.Manifest{
			BundleID: id, PhaseSource: "generated",
			Phases: map[string]bundle.PhaseArtifact{"expand": {Path: "phases/expand.sql", Transactional: &transactional}},
		},
		Files: map[string][]byte{
			"plan.json":         []byte(plan),
			"phases/expand.sql": []byte("ALTER TABLE t ADD COLUMN c text;\n"),
		},
	}}
}

func editedEntry(id string) history.Entry {
	entry := generatedEntry(id, expandOnlyPlan)
	entry.Artifact.Manifest.PhaseSource = "edited"
	return entry
}

func chainOf(entries ...history.Entry) history.Chain {
	return history.Chain{Target: "primary", Entries: entries}
}

func identity(t *testing.T, chain history.Chain, bundleID, through string, ignores ...string) string {
	t.Helper()
	value, err := executionIdentity("postgres://scratch", chain, bundleID, through, ignores)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestExecutionIdentityIgnoresThePhaseOnlyWhenNoWorkFollowsExpand(t *testing.T) {
	base := generatedEntry("base", expandOnlyPlan)

	expandOnly := chainOf(base, generatedEntry("feature", expandOnlyPlan))
	if identity(t, expandOnly, "feature", "expand") != identity(t, expandOnly, "feature", "contract") {
		t.Fatal("a generated bundle with no contract batch runs the same SQL for both phases, but the identities differ")
	}

	withContract := chainOf(base, generatedEntry("feature", expandAndContractPlan))
	if identity(t, withContract, "feature", "expand") == identity(t, withContract, "feature", "contract") {
		t.Fatal("a contract batch runs only for the contract phase, but the identities are equal")
	}

	edited := chainOf(base, editedEntry("feature"))
	if identity(t, edited, "feature", "expand") != identity(t, edited, "feature", "contract") {
		t.Fatal("an edited bundle with no contract phase and no assertions runs the same SQL for both phases, but the identities differ")
	}

	withAssertions := editedEntry("feature")
	withAssertions.Artifact.Manifest.VerificationDigest = "sha256:assertions"
	withAssertions.Artifact.Files["verify.sql"] = []byte("-- onwardpg:assert a\nSELECT true;\n")
	asserted := chainOf(base, withAssertions)
	if identity(t, asserted, "feature", "expand") == identity(t, asserted, "feature", "contract") {
		t.Fatal("assertions run only for the contract phase, but the identities are equal")
	}

	withContractPhase := editedEntry("feature")
	nonTransactional := false
	withContractPhase.Artifact.Manifest.Phases["contract"] = bundle.PhaseArtifact{Path: "phases/contract.sql", Transactional: &nonTransactional}
	withContractPhase.Artifact.Files["phases/contract.sql"] = []byte("ALTER TABLE t DROP COLUMN old;\n")
	contracted := chainOf(base, withContractPhase)
	if identity(t, contracted, "feature", "expand") == identity(t, contracted, "feature", "contract") {
		t.Fatal("an edited contract phase runs only for the contract phase, but the identities are equal")
	}

	// The phase of an earlier bundle is never limited, so the selected phase
	// must still count when the selected bundle has later work, whatever the
	// earlier bundles hold.
	earlierContract := chainOf(generatedEntry("base", expandAndContractPlan), generatedEntry("feature", expandOnlyPlan))
	if identity(t, earlierContract, "feature", "expand") != identity(t, earlierContract, "feature", "contract") {
		t.Fatal("a contract batch in an earlier bundle changed the identity of the selected phase")
	}
}

func TestExecutionIdentityChangesWithEveryExecutedInput(t *testing.T) {
	build := func() history.Chain {
		return chainOf(editedEntry("base"), generatedEntry("feature", expandAndContractPlan))
	}
	reference := identity(t, build(), "feature", "expand", "table:public.ignored")

	changes := map[string]func(chain *history.Chain) (bundleID, through string, ignores []string){
		"phase SQL of the selected bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[1].Artifact.Files["phases/expand.sql"] = []byte("ALTER TABLE t ADD COLUMN c integer;\n")
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"phase SQL of an earlier bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[0].Artifact.Files["phases/expand.sql"] = []byte("ALTER TABLE t ADD COLUMN c integer;\n")
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"plan of the selected bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[1].Artifact.Files["plan.json"] = []byte(expandOnlyPlan)
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"transaction mode of a phase": func(chain *history.Chain) (string, string, []string) {
			nonTransactional := false
			receipt := chain.Entries[0].Artifact.Manifest.Phases["expand"]
			receipt.Transactional = &nonTransactional
			chain.Entries[0].Artifact.Manifest.Phases["expand"] = receipt
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"phase source": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[0].Artifact.Manifest.PhaseSource = "generated"
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"assertions of an earlier bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[0].Artifact.Manifest.VerificationDigest = "sha256:assertions"
			chain.Entries[0].Artifact.Files["verify.sql"] = []byte("-- onwardpg:assert a\nSELECT true;\n")
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"directory of a bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries[0].Directory = "renamed"
			return "feature", "expand", []string{"table:public.ignored"}
		},
		"selected phase": func(chain *history.Chain) (string, string, []string) {
			return "feature", "contract", []string{"table:public.ignored"}
		},
		"ignore selectors": func(chain *history.Chain) (string, string, []string) {
			return "feature", "expand", nil
		},
		"removed bundle": func(chain *history.Chain) (string, string, []string) {
			chain.Entries = chain.Entries[1:]
			return "feature", "expand", []string{"table:public.ignored"}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			chain := build()
			bundleID, through, ignores := change(&chain)
			if identity(t, chain, bundleID, through, ignores...) == reference {
				t.Fatalf("the identity did not change with: %s", name)
			}
		})
	}

	other, err := executionIdentity("postgres://another-server", build(), "feature", "expand", []string{"table:public.ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if other == reference {
		t.Fatal("the identity did not change with the scratch server")
	}
}

func TestExecutionIdentityIgnoresReceiptsThatAreNotExecuted(t *testing.T) {
	plain := chainOf(generatedEntry("base", expandOnlyPlan), generatedEntry("feature", expandAndContractPlan))
	reference := identity(t, plain, "feature", "contract")

	// These are the changes that the expand checkpoint receipt makes.
	receipted := chainOf(generatedEntry("base", expandOnlyPlan), generatedEntry("feature", expandAndContractPlan))
	receipted.HeadDigest = "sha256:new-head"
	receipted.Entries[1].Artifact.Manifest.ExpandCheckpointDigest = "sha256:checkpoint"
	receipted.Entries[1].Artifact.Manifest.History = &bundle.HistoryReceipt{EntryDigest: "sha256:new-entry"}
	receipted.Entries[1].Artifact.Files["expand-checkpoint.json"] = []byte(`{"expand_fingerprint":"sha256:x"}`)
	receipted.Entries[1].Artifact.Files["manifest.json"] = []byte(`{"changed":true}`)
	if identity(t, receipted, "feature", "contract") != reference {
		t.Fatal("a receipt that no execution reads changed the identity")
	}
}

func TestExecutionIdentityRejectsAnUnreadablePlan(t *testing.T) {
	chain := chainOf(generatedEntry("feature", "{not json"))
	if _, err := executionIdentity("postgres://scratch", chain, "feature", "expand", nil); err == nil {
		t.Fatal("an unreadable plan produced an identity")
	}
}

func TestExecutionsRunEachSlotOnceAndKeepInstancesSeparate(t *testing.T) {
	executions := NewExecutions()
	runs := make(map[string]int)
	run := func(name string) func() (*pgschema.Snapshot, int, *Failure, error) {
		return func() (*pgschema.Snapshot, int, *Failure, error) {
			runs[name]++
			return pgschema.New(), 1, nil, nil
		}
	}
	first := executions.start(executionSlot{identity: "a"}, run("a0"))
	<-first.done
	again := executions.start(executionSlot{identity: "a"}, run("a0"))
	<-again.done
	if again != first || runs["a0"] != 1 {
		t.Fatalf("a filled slot ran again: runs=%d", runs["a0"])
	}
	second := executions.start(executionSlot{identity: "a", instance: 1}, run("a1"))
	<-second.done
	if second == first || second.snapshot == first.snapshot || runs["a1"] != 1 {
		t.Fatal("the second instance of one identity is not a separate execution")
	}
	other := executions.start(executionSlot{identity: "b"}, run("b0"))
	<-other.done
	if other == first || runs["b0"] != 1 {
		t.Fatal("another identity did not run")
	}
	if executions.Started() != 3 {
		t.Fatalf("Started = %d, want 3", executions.Started())
	}
}

func TestExecutionsKeepAFailureButNotAnError(t *testing.T) {
	executions := NewExecutions()
	failures := 0
	failed := executions.start(executionSlot{identity: "sql"}, func() (*pgschema.Snapshot, int, *Failure, error) {
		failures++
		return nil, 1, &Failure{Code: "transactional_batch_failed", Phase: protocol.PhaseExpand}, nil
	})
	<-failed.done
	again := executions.start(executionSlot{identity: "sql"}, func() (*pgschema.Snapshot, int, *Failure, error) {
		failures++
		return nil, 1, nil, nil
	})
	<-again.done
	if failures != 1 || again.failure == nil {
		t.Fatalf("a SQL failure is a result of the SQL and must be kept: runs=%d", failures)
	}

	attempts := 0
	broken := executions.start(executionSlot{identity: "server"}, func() (*pgschema.Snapshot, int, *Failure, error) {
		attempts++
		return nil, 0, nil, errors.New("scratch server unavailable")
	})
	<-broken.done
	if broken.err == nil {
		t.Fatal("the error was lost")
	}
	retried := executions.start(executionSlot{identity: "server"}, func() (*pgschema.Snapshot, int, *Failure, error) {
		attempts++
		return pgschema.New(), 1, nil, nil
	})
	<-retried.done
	if attempts != 2 || retried.err != nil || retried.snapshot == nil {
		t.Fatalf("an error must not be kept as a result: attempts=%d err=%v", attempts, retried.err)
	}
}
