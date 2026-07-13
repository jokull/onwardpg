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

const pinnedStripeCommit = "6208f8f3ceccae8ca634055dc47907a6a864cb76"

type stripeMatrix struct {
	SchemaVersion   int                `json:"schema_version"`
	Reference       stripeReference    `json:"reference"`
	InventoryStatus string             `json:"inventory_status"`
	SourceFiles     []stripeSourceFile `json:"source_files"`
	Cases           []stripeCase       `json:"cases"`
}

type stripeReference struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	License    string `json:"license"`
}

type stripeSourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Cases  int    `json:"cases"`
}

type stripeCase struct {
	ID               string           `json:"id"`
	Capability       string           `json:"capability"`
	Scenario         string           `json:"scenario"`
	StripeTest       string           `json:"stripe_test"`
	SourceLine       int              `json:"source_line"`
	Classification   string           `json:"classification"`
	Dimensions       stripeDimensions `json:"dimensions"`
	OnwardPGTests    []string         `json:"onwardpg_tests"`
	DifferentialTest string           `json:"differential_test"`
}

type stripeDimensions struct {
	Catalog        string `json:"catalog"`
	Mutation       string `json:"mutation"`
	OnlineStrategy string `json:"online_strategy"`
	Rejection      string `json:"rejection"`
	Workflow       string `json:"workflow"`
}

func TestStripeAcceptanceMatrixIsCompleteAndClassified(t *testing.T) {
	matrix := loadStripeMatrix(t)
	if matrix.SchemaVersion != 1 || matrix.Reference.Repository != "https://github.com/stripe/pg-schema-diff" || matrix.Reference.Tag != "v1.0.7" || matrix.Reference.Commit != pinnedStripeCommit || matrix.Reference.License != "MIT" {
		t.Fatalf("invalid Stripe reference header: %#v", matrix.Reference)
	}
	if matrix.InventoryStatus != "classified_unverified" && matrix.InventoryStatus != "verified" {
		t.Fatalf("invalid inventory status %q", matrix.InventoryStatus)
	}
	// These counts are receipts for the immutable v1.0.7 corpus. A changed
	// count requires an explicit new pin and a fresh classification audit.
	if len(matrix.SourceFiles) != 24 || len(matrix.Cases) != 415 {
		t.Fatalf("incomplete Stripe corpus index: %d files, %d cases", len(matrix.SourceFiles), len(matrix.Cases))
	}
	allowed := map[string]bool{
		"supported": true, "weaker": true, "missing": true,
		"intentionally_different": true, "out_of_scope": true,
	}
	files := make(map[string]stripeSourceFile, len(matrix.SourceFiles))
	total := 0
	for _, file := range matrix.SourceFiles {
		if file.Path == "" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(file.SHA256) || file.Cases < 1 {
			t.Fatalf("invalid Stripe source receipt: %#v", file)
		}
		if _, exists := files[file.Path]; exists {
			t.Fatalf("duplicate Stripe source file %q", file.Path)
		}
		files[file.Path] = file
		total += file.Cases
	}
	if total != len(matrix.Cases) {
		t.Fatalf("source file counts total %d, matrix has %d cases", total, len(matrix.Cases))
	}

	seen := make(map[string]bool, len(matrix.Cases))
	countsByFile := make(map[string]int, len(files))
	for _, entry := range matrix.Cases {
		if entry.ID == "" || entry.Capability == "" || entry.Scenario == "" || entry.SourceLine < 1 || seen[entry.ID] {
			t.Fatalf("invalid or duplicate Stripe case %#v", entry)
		}
		seen[entry.ID] = true
		if !allowed[entry.Classification] {
			t.Fatalf("Stripe case %s is unclassified: %q", entry.ID, entry.Classification)
		}
		for name, value := range map[string]string{
			"catalog": entry.Dimensions.Catalog, "mutation": entry.Dimensions.Mutation,
			"online_strategy": entry.Dimensions.OnlineStrategy, "rejection": entry.Dimensions.Rejection,
			"workflow": entry.Dimensions.Workflow,
		} {
			if !allowed[value] {
				t.Fatalf("Stripe case %s has unclassified %s dimension %q", entry.ID, name, value)
			}
		}
		file, _, found := strings.Cut(entry.StripeTest, "#")
		if !found || files[file].Path == "" {
			t.Fatalf("Stripe case %s has invalid source evidence %q", entry.ID, entry.StripeTest)
		}
		countsByFile[file]++
		for _, reference := range entry.OnwardPGTests {
			assertOnwardTestReference(t, reference)
		}
		if entry.DifferentialTest != "" {
			assertOnwardTestReference(t, entry.DifferentialTest)
		}
		if matrix.InventoryStatus == "verified" && (len(entry.OnwardPGTests) == 0 || entry.DifferentialTest == "") {
			t.Fatalf("verified Stripe case %s lacks onwardpg and differential evidence", entry.ID)
		}
	}
	for path, file := range files {
		if countsByFile[path] != file.Cases {
			t.Fatalf("Stripe source %s records %d cases, found %d", path, file.Cases, countsByFile[path])
		}
	}
}

func TestStripeMatrixMatchesPinnedAcceptanceCheckout(t *testing.T) {
	checkout := os.Getenv("STRIPE_PG_SCHEMA_DIFF_REF")
	if checkout == "" {
		t.Skip("set STRIPE_PG_SCHEMA_DIFF_REF to validate the committed corpus against the pinned checkout")
	}
	output, err := exec.Command("git", "-C", checkout, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if actual := strings.TrimSpace(string(output)); actual != pinnedStripeCommit {
		t.Fatalf("Stripe checkout is %s; expected %s", actual, pinnedStripeCommit)
	}
	matrix := loadStripeMatrix(t)
	for _, file := range matrix.SourceFiles {
		data, err := os.ReadFile(filepath.Join(checkout, "internal", filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if actual := hex.EncodeToString(digest[:]); actual != file.SHA256 {
			t.Fatalf("Stripe acceptance source %s changed: got %s want %s", file.Path, actual, file.SHA256)
		}
	}
}

func TestStripeIsNotAProductionDependency(t *testing.T) {
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(goMod), "github.com/stripe/pg-schema-diff") {
		t.Fatal("Stripe pg-schema-diff must not be a Go module dependency")
	}
	for _, root := range []string{"../../cmd", "../../internal", "../../pgschema"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "github.com/stripe/pg-schema-diff") {
				t.Errorf("production source %s depends on Stripe pg-schema-diff", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func loadStripeMatrix(t *testing.T) stripeMatrix {
	t.Helper()
	data, err := os.ReadFile("../../parity/stripe-pg-schema-diff-v1.0.7.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix stripeMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	return matrix
}
