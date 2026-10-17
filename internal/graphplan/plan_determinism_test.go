package graphplan

import (
	"bytes"
	"slices"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// planRepeatedly builds the same inputs many times. Go randomizes map
// iteration per range statement, so a map-ordered reason, hint, or statement
// shows up as a differing bundle byte within a few runs.
func planRepeatedly(t *testing.T, current, desired *pgschema.Snapshot, runs int) protocol.Result {
	t.Helper()
	var first protocol.Result
	var firstFiles map[string][]byte
	for run := 0; run < runs; run++ {
		result, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		metadata := bundle.Metadata{
			BundleID: "determinism", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1, Target: "primary", Purpose: "feature",
			BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "baseline", Fingerprint: result.CurrentFingerprint},
			DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "desired", Fingerprint: result.DesiredFingerprint},
			Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
		}
		artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first, firstFiles = result, artifact.Files
			continue
		}
		if len(artifact.Files) != len(firstFiles) {
			t.Fatalf("run %d wrote %d bundle files, run 0 wrote %d", run, len(artifact.Files), len(firstFiles))
		}
		for _, name := range bundle.SortedFiles(firstFiles) {
			if !bytes.Equal(artifact.Files[name], firstFiles[name]) {
				t.Fatalf("run %d wrote a different %s:\nrun 0: %s\nrun %d: %s", run, name, firstFiles[name], run, artifact.Files[name])
			}
		}
	}
	return first
}

func addObjects(t *testing.T, snapshot *pgschema.Snapshot, objects ...pgschema.Object) {
	t.Helper()
	for _, object := range objects {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildIsByteIdenticalForRejectedTableRenameWithSeveralMismatchingChildren(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "monthly_close_draft"}
	to := pgschema.Table{Schema: "app", Name: "monthly_close_drafts"}
	addObjects(t, current, from, pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"})
	addObjects(t, desired, to, pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"})
	// User-chosen names are not derived from the table name, so each child is
	// a separate identity mismatch and any of them could name the rejection.
	for _, names := range [][2]string{{"draft_a", "close_a"}, {"draft_b", "close_b"}, {"draft_c", "close_c"}, {"draft_d", "close_d"}, {"draft_e", "close_e"}} {
		addObjects(t, current, pgschema.Constraint{Table: from.ObjectID(), Name: names[0], Type: pgschema.ConstraintCheck, Definition: "CHECK (id > 0)", Validated: true})
		addObjects(t, desired, pgschema.Constraint{Table: to.ObjectID(), Name: names[1], Type: pgschema.ConstraintCheck, Definition: "CHECK (id > 0)", Validated: true})
	}
	for _, names := range [][2]string{{"draft_x_idx", "close_x_idx"}, {"draft_y_idx", "close_y_idx"}} {
		addObjects(t, current, pgschema.Index{Table: from.ObjectID(), Name: names[0], Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}})
		addObjects(t, desired, pgschema.Index{Table: to.ObjectID(), Name: names[1], Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}})
	}

	result := planRepeatedly(t, current, desired, 20)
	if len(result.Analysis) != 1 || result.Analysis[0].Kind != "rename_table" || result.Analysis[0].Outcome != "rejected" {
		t.Fatalf("expected one rejected table rename, got %#v", result.Analysis)
	}
	// The first mismatching child by stable key order names the rejection.
	if want := "child_identity_mismatch:constraint:app:monthly_close_draft:draft_a"; result.Analysis[0].Reason != want {
		t.Fatalf("reason = %q, want %q", result.Analysis[0].Reason, want)
	}
}

func TestBuildIsByteIdenticalForSeveralReplacedDomainConstraints(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Domain{Schema: "app", Name: "amount", BaseType: "bigint"}
	after := before
	for _, name := range []string{"amount_a", "amount_b", "amount_c", "amount_d", "amount_e"} {
		before.Constraints = append(before.Constraints, pgschema.DomainConstraint{Name: name, Definition: "CHECK (VALUE > 0)", Validated: true})
		after.Constraints = append(after.Constraints, pgschema.DomainConstraint{Name: name, Definition: "CHECK (VALUE >= 0)", Validated: true})
	}
	addObjects(t, current, before)
	addObjects(t, desired, after)

	result := planRepeatedly(t, current, desired, 20)
	var dropped []string
	for _, statement := range result.Statements {
		for _, name := range []string{"amount_a", "amount_b", "amount_c", "amount_d", "amount_e"} {
			if statement.SQL == `ALTER DOMAIN "app"."amount" DROP CONSTRAINT "`+name+`";` {
				dropped = append(dropped, name)
			}
		}
	}
	if want := []string{"amount_a", "amount_b", "amount_c", "amount_d", "amount_e"}; !slices.Equal(dropped, want) {
		t.Fatalf("replaced domain constraints in order %v, want %v; statements=%#v", dropped, want, result.Statements)
	}
}
