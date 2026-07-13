package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
)

const (
	current = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	desired = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestLoadOrdersByHashChainAndReplaysPhases(t *testing.T) {
	root := t.TempDir()
	first := writeBundle(t, root, "z-first", bundle.HistoryRootDigest(), "SELECT 1;", "expand")
	second := writeBundle(t, root, "a-second", first, "SELECT 2;", "contract")

	chain, err := Load(root, "migrations/onward", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if chain.HeadDigest != second || len(chain.Entries) != 2 || chain.Entries[0].Directory != "z-first" || chain.Entries[1].Directory != "a-second" {
		t.Fatalf("chain = %#v", chain)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(replay.DDL), "SELECT 1") > strings.Index(string(replay.DDL), "SELECT 2") || replay.Digest != second {
		t.Fatalf("replay = %#v\n%s", replay, replay.DDL)
	}
}

func TestLoadRejectsForkAndMissingParent(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*testing.T, string)
		wanted string
	}{
		{name: "fork", setup: func(t *testing.T, root string) {
			writeBundle(t, root, "first", bundle.HistoryRootDigest(), "SELECT 1;", "expand")
			writeBundle(t, root, "second", bundle.HistoryRootDigest(), "SELECT 2;", "expand")
		}, wanted: "fork"},
		{name: "missing-parent", setup: func(t *testing.T, root string) {
			writeBundle(t, root, "orphan", current, "SELECT 1;", "expand")
		}, wanted: "missing parent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			_, err := Load(root, "migrations/onward", "primary")
			if err == nil || !strings.Contains(err.Error(), test.wanted) {
				t.Fatalf("expected %q rejection, got %v", test.wanted, err)
			}
		})
	}
}

func TestLoadRejectsTamperedBundleAndDirectoryIdentity(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "feature", bundle.HistoryRootDigest(), "SELECT 1;", "expand")
	directory := filepath.Join(root, "migrations/onward/primary/feature")
	if err := os.WriteFile(filepath.Join(directory, "phases/expand.sql"), []byte("SELECT 999;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, "migrations/onward", "primary"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}

	root = t.TempDir()
	writeBundle(t, root, "feature", bundle.HistoryRootDigest(), "SELECT 1;", "expand")
	oldName := filepath.Join(root, "migrations/onward/primary/feature")
	newName := filepath.Join(root, "migrations/onward/primary/renamed")
	if err := os.Rename(oldName, newName); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, "migrations/onward", "primary"); err == nil || !strings.Contains(err.Error(), "contains bundle_id") {
		t.Fatalf("expected directory identity rejection, got %v", err)
	}
}

func TestLoadEmptyHistoryUsesStableRoot(t *testing.T) {
	chain, err := Load(t.TempDir(), "migrations/onward", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if chain.HeadDigest != bundle.HistoryRootDigest() || len(chain.Entries) != 0 {
		t.Fatalf("chain = %#v", chain)
	}
}

func writeBundle(t *testing.T, root, id, parent, sql, phase string) string {
	t.Helper()
	statement := protocol.Statement{SQL: sql, Phase: phase, Safety: "safe"}
	statement.ID = protocol.StableStatementID(statement)
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: current, DesiredFingerprint: desired,
		Status: protocol.Planned, Statements: []protocol.Statement{statement},
		Batches: []protocol.Batch{{ID: "batch-1", Phase: phase, Transactional: true, Statements: []protocol.Statement{statement}}},
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature", Mode: "pr",
		BaseRef: "origin/main", BaseCommit: strings.Repeat("a", 40), HeadRevision: strings.Repeat("b", 40),
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "base history", Fingerprint: current, PostgresMajor: 16},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "desired schema", Fingerprint: desired, PostgresMajor: 16},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: parent,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "migrations/onward/primary", id)
	if err := bundle.Write(destination, artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return artifact.Manifest.History.EntryDigest
}
