package parity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type matrix struct {
	SchemaVersion   int     `json:"schema_version"`
	AtlasCommit     string  `json:"atlas_commit"`
	InventoryStatus string  `json:"inventory_status"`
	Versions        []int   `json:"postgres_versions"`
	Entries         []entry `json:"entries"`
}

type entry struct {
	ID            string   `json:"id"`
	Area          string   `json:"area"`
	Behavior      string   `json:"behavior"`
	Status        string   `json:"status"`
	Since         int      `json:"since"`
	AtlasEvidence []string `json:"atlas_evidence"`
	AtlasTests    []string `json:"atlas_tests"`
	OnwardPGTests []string `json:"onwardpg_tests"`
}

type roadmapMatrix struct {
	SchemaVersion   int            `json:"schema_version"`
	Source          string         `json:"source"`
	InventoryStatus string         `json:"inventory_status"`
	Entries         []roadmapEntry `json:"entries"`
}

type roadmapEntry struct {
	ID            string   `json:"id"`
	Area          string   `json:"area"`
	Roadmap       string   `json:"roadmap"`
	OnwardPG      string   `json:"onwardpg"`
	OnwardPGTests []string `json:"onwardpg_tests"`
}

func TestAtlasPostgresMatrixIsMachineReadableAndEvidenceLinked(t *testing.T) {
	const pinnedAtlasCommit = "a5e0aecc2bb64143bf522734f8ad88e04885fca6"
	data, err := os.ReadFile("../../parity/atlas-postgres.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.SchemaVersion != 1 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(matrix.AtlasCommit) || matrix.AtlasCommit != pinnedAtlasCommit {
		t.Fatalf("invalid matrix header: %#v", matrix)
	}
	if len(matrix.Versions) == 0 || len(matrix.Entries) == 0 {
		t.Fatal("matrix must declare versions and entries")
	}
	seen := make(map[string]bool, len(matrix.Entries))
	for _, entry := range matrix.Entries {
		if entry.ID == "" || entry.Area == "" || seen[entry.ID] {
			t.Fatalf("invalid or duplicate entry id %q", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Behavior != "supported" && entry.Behavior != "rejected" && entry.Behavior != "ignored" {
			t.Fatalf("entry %s has invalid behavior %q", entry.ID, entry.Behavior)
		}
		if entry.Status != "pending" && entry.Status != "implemented" && entry.Status != "verified" {
			t.Fatalf("entry %s has invalid status %q", entry.ID, entry.Status)
		}
		// Since records when the pinned Atlas behavior became available. It is
		// intentionally independent of onwardpg's supported-server policy.
		if entry.Since < 10 || len(entry.AtlasEvidence) == 0 || len(entry.AtlasTests) == 0 {
			t.Fatalf("entry %s is not linked to Atlas evidence", entry.ID)
		}
		if entry.Status != "pending" && len(entry.OnwardPGTests) == 0 {
			t.Fatalf("%s entry %s has no onwardpg test", entry.Status, entry.ID)
		}
		for _, reference := range entry.OnwardPGTests {
			assertOnwardTestReference(t, reference)
		}
		if entry.Status == "verified" && !hasPinnedAtlasEvidence(entry) {
			t.Fatalf("verified entry %s has no pinned-Atlas differential evidence", entry.ID)
		}
	}
	if matrix.InventoryStatus == "complete" {
		for _, entry := range matrix.Entries {
			if entry.Status != "verified" {
				t.Fatalf("complete inventory contains non-verified entry %s", entry.ID)
			}
		}
	}
}

func hasPinnedAtlasEvidence(entry entry) bool {
	for _, reference := range entry.OnwardPGTests {
		if strings.HasPrefix(reference, "internal/differential#TestPinnedAtlas") {
			return true
		}
	}
	return false
}

func TestHasPinnedAtlasEvidence(t *testing.T) {
	if hasPinnedAtlasEvidence(entry{OnwardPGTests: []string{"internal/graphplan#TestPlan"}}) {
		t.Fatal("ordinary planner test must not satisfy pinned-Atlas evidence")
	}
	if !hasPinnedAtlasEvidence(entry{OnwardPGTests: []string{"internal/differential#TestPinnedAtlasAndOnwardPGConvergeForMutationCorpus/default-change"}}) {
		t.Fatal("pinned differential test must satisfy evidence gate")
	}
}

func assertOnwardTestReference(t *testing.T, reference string) {
	t.Helper()
	packagePath, testName, found := strings.Cut(reference, "#")
	if !found || packagePath == "" || testName == "" {
		t.Fatalf("invalid onwardpg test reference %q", reference)
	}
	// A differential subtest is linked through its parent Test function. The
	// test name itself remains the durable source-level reference.
	rootTest, subtest, hasSubtest := strings.Cut(testName, "/")
	root := filepath.Join("../..", filepath.FromSlash(packagePath))
	files, err := filepath.Glob(filepath.Join(root, "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("test reference %q has no test files in %q: %v", reference, root, err)
	}
	foundRoot, foundSubtest := false, !hasSubtest
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		contents := string(data)
		if strings.Contains(contents, "func "+rootTest+"(") {
			foundRoot = true
		}
		if hasSubtest && strings.Contains(contents, `"`+subtest+`"`) {
			foundSubtest = true
		}
	}
	if !foundRoot || !foundSubtest {
		t.Fatalf("test reference %q does not resolve to a current Go test/subtest", reference)
	}
}

func TestPGMigRoadmapMatrixKeepsAtlasScopeExpansionVisible(t *testing.T) {
	data, err := os.ReadFile("../../parity/pgmig-roadmap.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix roadmapMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.SchemaVersion != 1 || matrix.Source != "https://github.com/Apakottur/pgmig/issues/8" || matrix.InventoryStatus != "in_progress" {
		t.Fatalf("invalid roadmap header: %#v", matrix)
	}
	seen := make(map[string]bool, len(matrix.Entries))
	for _, entry := range matrix.Entries {
		if entry.ID == "" || entry.Area == "" || seen[entry.ID] {
			t.Fatalf("invalid or duplicate roadmap id %q", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Roadmap != "complete" && entry.Roadmap != "planned" && entry.Roadmap != "non_goal" {
			t.Fatalf("invalid roadmap state for %s: %q", entry.ID, entry.Roadmap)
		}
		if entry.OnwardPG != "implemented" && entry.OnwardPG != "partial" && entry.OnwardPG != "unmodeled" && entry.OnwardPG != "out_of_scope" {
			t.Fatalf("invalid onwardpg scope state for %s: %q", entry.ID, entry.OnwardPG)
		}
		for _, reference := range entry.OnwardPGTests {
			assertOnwardTestReference(t, reference)
		}
	}
}
