package draftflow

import (
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/history"
)

func TestAcceptedStepsSinceInventoriesStructuralAndUnprovableWork(t *testing.T) {
	root := bundle.HistoryRootDigest()
	parent := "sha256:parent"
	chain := history.Chain{
		RootDigest: root,
		Entries: []history.Entry{
			{
				Directory: "baseline",
				Artifact:  bundle.Artifact{Manifest: bundle.Manifest{History: &bundle.HistoryReceipt{EntryDigest: parent}}},
			},
			{
				Directory: "generated-upstream",
				Artifact: bundle.Artifact{Manifest: bundle.Manifest{
					PhaseSource: "generated",
					Phases: map[string]bundle.PhaseArtifact{
						"contract": {Path: "phases/contract.sql"},
						"expand":   {Path: "phases/expand.sql"},
					},
				}},
			},
			{
				Directory: "edited-upstream",
				Artifact: bundle.Artifact{Manifest: bundle.Manifest{
					PhaseSource:        "edited",
					VerificationDigest: "sha256:assertions",
					Phases: map[string]bundle.PhaseArtifact{
						"expand":   {Path: "phases/expand.sql"},
						"contract": {Path: "phases/contract.sql"},
					},
				}},
			},
		},
	}

	steps := acceptedStepsSince(chain, parent)
	if len(steps) != 5 {
		t.Fatalf("steps = %#v", steps)
	}
	want := map[string]struct {
		kind   string
		review bool
	}{
		"generated-upstream/phases/expand.sql":   {"generated_structural_phase", false},
		"generated-upstream/phases/contract.sql": {"generated_contract_phase", true},
		"edited-upstream/phases/expand.sql":      {"agent_authored_phase", true},
		"edited-upstream/phases/contract.sql":    {"agent_authored_phase", true},
		"edited-upstream/verify.sql":             {"verification_assertions", true},
	}
	for _, step := range steps {
		expected, exists := want[step.Path]
		if !exists || step.Kind != expected.kind || step.RequiresReview != expected.review || step.Reason == "" {
			t.Fatalf("unexpected accepted step %#v", step)
		}
		delete(want, step.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing accepted steps %#v", want)
	}
}

func TestAcceptedStepsSinceDoesNotReportExistingBase(t *testing.T) {
	root := bundle.HistoryRootDigest()
	chain := history.Chain{RootDigest: root, Entries: []history.Entry{{
		Directory: "baseline",
		Artifact: bundle.Artifact{Manifest: bundle.Manifest{
			PhaseSource: "generated",
			History:     &bundle.HistoryReceipt{EntryDigest: "sha256:baseline"},
			Phases:      map[string]bundle.PhaseArtifact{"expand": {Path: "phases/expand.sql"}},
		}},
	}}}
	if steps := acceptedStepsSince(chain, "sha256:baseline"); len(steps) != 0 {
		t.Fatalf("existing base steps = %#v", steps)
	}
}

func TestDevelopmentPostconditionsSinceIncludesOnlyExplicitlyMarkedAssertions(t *testing.T) {
	root := bundle.HistoryRootDigest()
	chain := history.Chain{RootDigest: root, Entries: []history.Entry{
		{Directory: "baseline", Artifact: bundle.Artifact{Manifest: bundle.Manifest{History: &bundle.HistoryReceipt{EntryDigest: "sha256:baseline"}}}},
		{Directory: "upstream", Artifact: bundle.Artifact{
			Manifest: bundle.Manifest{VerificationDigest: "sha256:verify"},
			Files:    map[string][]byte{"verify.sql": []byte("-- onwardpg:assert safe\n-- onwardpg:dev-postcondition\nSELECT true;\n-- onwardpg:assert disposable_only\nSELECT true;\n")},
		}},
	}}
	checks, err := developmentPostconditionsSince(chain, "sha256:baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].BundleID != "upstream" || checks[0].ID != "safe" || checks[0].Path != "upstream/verify.sql" {
		t.Fatalf("checks = %#v", checks)
	}
}
