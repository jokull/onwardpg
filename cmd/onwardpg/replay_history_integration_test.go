package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
)

// The replay stops with the bundle and batch when accepted SQL fails, and it
// refuses a scratch server of another major version before it creates a
// database.
func TestReplayHistoryReportsTheFailingBundleAndRefusesAnotherMajorOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	repository := t.TempDir()
	writeHistoryFixture(t, repository, url, "genesis", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);\n")
	writeHistoryTransitionFixture(t, repository, url, "add-timezone", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);\n")
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	before := disposableDatabaseCount(t, url)

	// An empty chain is the catalog of an empty database.
	empty, err := verify.ReplayHistory(ctx, url, history.Chain{Target: "primary"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-postgresql", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyFingerprint, err := empty.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	directFingerprint, err := direct.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if emptyFingerprint != directFingerprint {
		t.Fatalf("empty history fingerprint %s, empty database %s", emptyFingerprint, directFingerprint)
	}

	broken := chain
	broken.Entries = append([]history.Entry(nil), chain.Entries...)
	second := broken.Entries[1]
	files := make(map[string][]byte, len(second.Artifact.Files))
	for name, body := range second.Artifact.Files {
		files[name] = body
	}
	var plan protocol.Result
	if err := json.Unmarshal(files["plan.json"], &plan); err != nil {
		t.Fatal(err)
	}
	plan.Batches[0].Statements = []protocol.Statement{{SQL: "ALTER TABLE app.missing ADD COLUMN timezone text;"}}
	if files["plan.json"], err = json.Marshal(plan); err != nil {
		t.Fatal(err)
	}
	second.Artifact.Files = files
	broken.Entries[1] = second
	_, err = verify.ReplayHistory(ctx, url, broken, nil)
	if err == nil || !strings.Contains(err.Error(), "history bundle add-timezone expand batch "+plan.Batches[0].ID+" (transactional) failed") || !strings.Contains(err.Error(), "42P01") {
		t.Fatalf("replay failure = %v", err)
	}

	actual, err := source.PostgresMajor(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	different := 14
	if actual == different {
		different = 15
	}
	mismatched := chain
	mismatched.Entries = append([]history.Entry(nil), chain.Entries...)
	mismatched.Entries[0].Artifact.Manifest.DesiredSource.PostgresMajor = different
	_, err = verify.ReplayHistory(ctx, url, mismatched, nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("history bundle genesis targets PostgreSQL %d but the scratch server is PostgreSQL %d", different, actual)) {
		t.Fatalf("major mismatch error = %v", err)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("disposable database count = %d, want %d", after, before)
	}
}
