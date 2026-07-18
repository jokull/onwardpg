package parity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const pinnedPGMigCommit = "d2cccb6886bfb0b6ad0649bbe1430a9ab57ae983"

type pgmigMatrix struct {
	SchemaVersion   int               `json:"schema_version"`
	Reference       pgmigReference    `json:"reference"`
	InventoryStatus string            `json:"inventory_status"`
	SourceFiles     []pgmigSourceFile `json:"source_files"`
	Scenarios       []pgmigScenario   `json:"scenarios"`
}

type pgmigReference struct {
	Repository   string `json:"repository"`
	Commit       string `json:"commit"`
	License      string `json:"license"`
	ScenarioRoot string `json:"scenario_root"`
}

type pgmigSourceFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Scenarios int    `json:"scenarios"`
}

type pgmigScenario struct {
	ID             string   `json:"id"`
	Area           string   `json:"area"`
	Family         string   `json:"family"`
	Name           string   `json:"name"`
	PGMigTest      string   `json:"pgmig_test"`
	SourceLine     int      `json:"source_line"`
	MinPostgres    int      `json:"min_postgres"`
	PGMigOutcome   string   `json:"pgmig_outcome"`
	Classification string   `json:"classification"`
	Workstream     string   `json:"workstream"`
	Tags           []string `json:"tags"`
	OnwardPGTests  []string `json:"onwardpg_tests"`
}

func TestPGMigScenarioMatrixIsCompleteAndClassified(t *testing.T) {
	matrix := loadPGMigMatrix(t)
	if matrix.SchemaVersion != 1 || matrix.Reference.Repository != "https://github.com/Apakottur/pgmig" || matrix.Reference.Commit != pinnedPGMigCommit || matrix.Reference.License != "MIT" || matrix.Reference.ScenarioRoot != "tests/_api" {
		t.Fatalf("invalid pgmig reference header: %#v", matrix.Reference)
	}
	if matrix.InventoryStatus != "classified_unverified" && matrix.InventoryStatus != "verified" {
		t.Fatalf("invalid pgmig inventory status %q", matrix.InventoryStatus)
	}
	if len(matrix.SourceFiles) != 47 || len(matrix.Scenarios) != 454 {
		t.Fatalf("incomplete pgmig corpus index: %d files, %d scenarios", len(matrix.SourceFiles), len(matrix.Scenarios))
	}

	allowedClassification := map[string]bool{
		"verified": true, "implemented_unverified": true, "needs_handoff": true,
		"unsupported_gap": true, "intentional_rejection": true,
	}
	allowedAreas := map[string]bool{
		"constraint": true, "general": true, "materialized_view": true,
		"routine": true, "schema": true, "sequence": true, "table": true,
		"type": true, "view": true,
	}
	files := make(map[string]pgmigSourceFile, len(matrix.SourceFiles))
	total := 0
	for _, file := range matrix.SourceFiles {
		if file.Path == "" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(file.SHA256) || file.Scenarios < 1 {
			t.Fatalf("invalid pgmig source receipt: %#v", file)
		}
		if _, exists := files[file.Path]; exists {
			t.Fatalf("duplicate pgmig source file %q", file.Path)
		}
		files[file.Path] = file
		total += file.Scenarios
	}
	if total != len(matrix.Scenarios) {
		t.Fatalf("source file counts total %d, matrix has %d scenarios", total, len(matrix.Scenarios))
	}

	seen := make(map[string]bool, len(matrix.Scenarios))
	countsByFile := make(map[string]int, len(files))
	classifications := make(map[string]int)
	for _, entry := range matrix.Scenarios {
		if entry.ID == "" || entry.Family == "" || entry.Name == "" || entry.SourceLine < 1 || entry.Workstream == "" || len(entry.Tags) < 2 || seen[entry.ID] {
			t.Fatalf("invalid or duplicate pgmig scenario: %#v", entry)
		}
		seen[entry.ID] = true
		classifications[entry.Classification]++
		if !allowedAreas[entry.Area] || !allowedClassification[entry.Classification] {
			t.Fatalf("pgmig scenario %s has invalid area/classification: %s/%s", entry.ID, entry.Area, entry.Classification)
		}
		if entry.PGMigOutcome != "converges" && entry.PGMigOutcome != "rejects" {
			t.Fatalf("pgmig scenario %s has invalid outcome %q", entry.ID, entry.PGMigOutcome)
		}
		if entry.MinPostgres != 15 && entry.MinPostgres != 18 {
			t.Fatalf("pgmig scenario %s has invalid PostgreSQL floor %d", entry.ID, entry.MinPostgres)
		}
		file, _, found := strings.Cut(entry.PGMigTest, "#")
		if !found || files[file].Path == "" {
			t.Fatalf("pgmig scenario %s has invalid source evidence %q", entry.ID, entry.PGMigTest)
		}
		countsByFile[file]++
		for _, reference := range entry.OnwardPGTests {
			assertOnwardTestReference(t, reference)
		}
		if entry.Classification == "verified" && len(entry.OnwardPGTests) == 0 {
			t.Fatalf("verified pgmig scenario %s lacks onwardpg evidence", entry.ID)
		}
	}
	if matrix.InventoryStatus == "verified" && (classifications["implemented_unverified"] != 0 || classifications["unsupported_gap"] != 0) {
		t.Fatalf("verified pgmig inventory retains unresolved classifications: %#v", classifications)
	}
	for path, file := range files {
		if countsByFile[path] != file.Scenarios {
			t.Fatalf("pgmig source %s records %d scenarios, found %d", path, file.Scenarios, countsByFile[path])
		}
	}

	wantPinnedScenarios := []string{
		"routine.test_view_trigger.test_view_trigger_create",
		"table.test_table_replica_identity.test_replica_identity_default_to_full",
		"type.test_range_type.test_range_type_create",
	}
	for _, id := range wantPinnedScenarios {
		if !seen[id] {
			t.Fatalf("pinned pgmig scenario %s disappeared without a corpus update", id)
		}
	}
}

func TestPGMigMatrixMatchesPinnedCheckout(t *testing.T) {
	checkout := os.Getenv("PGMIG_REF")
	if checkout == "" {
		t.Skip("set PGMIG_REF to validate the committed corpus against the pinned checkout")
	}
	output, err := exec.Command("git", "-C", checkout, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if actual := strings.TrimSpace(string(output)); actual != pinnedPGMigCommit {
		t.Fatalf("pgmig checkout is %s; expected %s", actual, pinnedPGMigCommit)
	}
	matrix := loadPGMigMatrix(t)
	for _, file := range matrix.SourceFiles {
		data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if actual := hex.EncodeToString(digest[:]); actual != file.SHA256 {
			t.Fatalf("pgmig source %s changed: got %s want %s", file.Path, actual, file.SHA256)
		}
	}
}

func loadPGMigMatrix(t *testing.T) pgmigMatrix {
	t.Helper()
	data, err := os.ReadFile("../../references/pgmig-d2cccb6.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix pgmigMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	return matrix
}
