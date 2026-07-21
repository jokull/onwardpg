package acceptance

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var inventoryIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type coverageInventory struct {
	SchemaVersion        int                `json:"schema_version"`
	MandatoryCheckpoints []string           `json:"mandatory_checkpoints"`
	Claims               []coverageClaim    `json:"claims"`
	Owners               []coverageOwner    `json:"owners"`
	Scenarios            []coverageScenario `json:"scenarios"`
}

type coverageClaim struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Match   string `json:"match"`
	Summary string `json:"summary"`
}

type coverageOwner struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Engine string `json:"engine"`
	Path   string `json:"path"`
	Test   string `json:"test"`
}

type coverageScenario struct {
	ID                 string                        `json:"id"`
	Family             string                        `json:"family"`
	Title              string                        `json:"title"`
	ProofModes         []string                      `json:"proof_modes"`
	PublicClaimIDs     []string                      `json:"public_claim_ids"`
	ProtectedInvariant string                        `json:"protected_invariant"`
	NativeOwnerIDs     []string                      `json:"native_owner_ids"`
	PGlite             pgliteCoverage                `json:"pglite"`
	Dimensions         coverageDimensions            `json:"dimensions"`
	Checkpoints        map[string]coverageCheckpoint `json:"checkpoints"`
}

type pgliteCoverage struct {
	Eligibility   string   `json:"eligibility"`
	OwnerID       string   `json:"owner_id,omitempty"`
	NativeOwnerID string   `json:"native_owner_id,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

type coverageDimensions struct {
	DDL           string `json:"ddl"`
	Data          string `json:"data"`
	Client        string `json:"client"`
	Decision      string `json:"decision"`
	PlanEvolution string `json:"plan_evolution"`
	Environment   string `json:"environment"`
}

type coverageCheckpoint struct {
	OwnerIDs      []string `json:"owner_ids,omitempty"`
	OmittedReason string   `json:"omitted_reason,omitempty"`
}

func TestCoverageInventoryContract(t *testing.T) {
	repoRoot := filepath.Clean("..")
	inventory := loadCoverageInventory(t, filepath.Join(repoRoot, "acceptance", "coverage.json"))

	if inventory.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", inventory.SchemaVersion)
	}
	wantCheckpoints := []string{"planned", "expanded_legacy", "expanded_new", "contract_blocked", "contract_ready", "converged"}
	if strings.Join(inventory.MandatoryCheckpoints, "\x00") != strings.Join(wantCheckpoints, "\x00") {
		t.Fatalf("mandatory_checkpoints = %q, want exact release lifecycle %q", inventory.MandatoryCheckpoints, wantCheckpoints)
	}
	if len(inventory.Claims) == 0 || len(inventory.Owners) == 0 || len(inventory.Scenarios) == 0 {
		t.Fatal("coverage inventory must contain claims, owners, and scenarios")
	}

	allIDs := make(map[string]string)
	claimByID := make(map[string]coverageClaim)
	for _, claim := range inventory.Claims {
		registerUniqueID(t, allIDs, claim.ID, "claim")
		requireText(t, claim.ID+" summary", claim.Summary)
		requireRepoPath(t, repoRoot, claim.ID+" source", claim.Source)
		requireText(t, claim.ID+" match", claim.Match)
		contents, err := os.ReadFile(filepath.Join(repoRoot, claim.Source))
		if err != nil {
			t.Fatalf("claim %s source: %v", claim.ID, err)
		}
		if !bytes.Contains(contents, []byte(claim.Match)) {
			t.Errorf("claim %s no longer matches %s: missing %q", claim.ID, claim.Source, claim.Match)
		}
		claimByID[claim.ID] = claim
	}

	ownerByID := make(map[string]coverageOwner)
	for _, owner := range inventory.Owners {
		registerUniqueID(t, allIDs, owner.ID, "owner")
		if owner.Status != "existing" && owner.Status != "planned" {
			t.Errorf("owner %s status = %q, want existing or planned", owner.ID, owner.Status)
		}
		if owner.Engine != "native_postgres" && owner.Engine != "pglite" {
			t.Errorf("owner %s engine = %q, want native_postgres or pglite", owner.ID, owner.Engine)
		}
		requireRepoPath(t, repoRoot, owner.ID+" path", owner.Path)
		if !strings.HasSuffix(owner.Path, "_test.go") {
			t.Errorf("owner %s path %q is not an exact Go test file", owner.ID, owner.Path)
		}
		if !regexp.MustCompile(`^Test[A-Za-z0-9_]+$`).MatchString(owner.Test) {
			t.Errorf("owner %s test = %q, want an exact Go Test name", owner.ID, owner.Test)
		}
		if owner.Status == "existing" {
			assertExistingTestOwner(t, repoRoot, owner)
		}
		ownerByID[owner.ID] = owner
	}

	usedClaims := make(map[string]bool)
	usedOwners := make(map[string]bool)
	pgliteOwnerUse := make(map[string]int)
	dimensionOwners := make(map[string]string)
	allowedProofModes := map[string]bool{
		"generated_automation": true,
		"reviewed_sql_handoff": true,
		"split_plan":           true,
		"refusal":              true,
	}

	for _, scenario := range inventory.Scenarios {
		registerUniqueID(t, allIDs, scenario.ID, "scenario")
		requireText(t, scenario.ID+" family", scenario.Family)
		requireText(t, scenario.ID+" title", scenario.Title)
		requireText(t, scenario.ID+" protected_invariant", scenario.ProtectedInvariant)
		if len(scenario.ProofModes) == 0 {
			t.Errorf("scenario %s has no proof_modes", scenario.ID)
		}
		assertUniqueStrings(t, scenario.ID+" proof_modes", scenario.ProofModes)
		for _, mode := range scenario.ProofModes {
			if !allowedProofModes[mode] {
				t.Errorf("scenario %s has unknown proof mode %q", scenario.ID, mode)
			}
		}

		if len(scenario.PublicClaimIDs) == 0 {
			t.Errorf("scenario %s maps no public claim", scenario.ID)
		}
		assertUniqueStrings(t, scenario.ID+" public_claim_ids", scenario.PublicClaimIDs)
		for _, claimID := range scenario.PublicClaimIDs {
			if _, ok := claimByID[claimID]; !ok {
				t.Errorf("scenario %s references unknown claim %s", scenario.ID, claimID)
			}
			usedClaims[claimID] = true
		}

		if len(scenario.NativeOwnerIDs) == 0 {
			t.Errorf("scenario %s has no native owner", scenario.ID)
		}
		assertUniqueStrings(t, scenario.ID+" native_owner_ids", scenario.NativeOwnerIDs)
		nativeOwnerSet := make(map[string]bool)
		hasExistingNativeOwner := false
		for _, ownerID := range scenario.NativeOwnerIDs {
			owner, ok := ownerByID[ownerID]
			if !ok {
				t.Errorf("scenario %s references unknown native owner %s", scenario.ID, ownerID)
				continue
			}
			if owner.Engine != "native_postgres" {
				t.Errorf("scenario %s native owner %s has engine %s", scenario.ID, ownerID, owner.Engine)
			}
			if owner.Status == "existing" {
				hasExistingNativeOwner = true
			}
			nativeOwnerSet[ownerID] = true
			usedOwners[ownerID] = true
		}
		if !hasExistingNativeOwner {
			t.Errorf("scenario %s has only planned native owners; public claims need a current lower-layer receipt while acceptance remains planned", scenario.ID)
		}

		validatePGliteCoverage(t, scenario, ownerByID, nativeOwnerSet, usedOwners, pgliteOwnerUse)
		validateDimensions(t, scenario, dimensionOwners)
		validateCheckpoints(t, scenario, wantCheckpoints, ownerByID, nativeOwnerSet, usedOwners)
	}

	for claimID := range claimByID {
		if !usedClaims[claimID] {
			t.Errorf("public claim %s is orphaned from every scenario", claimID)
		}
	}
	for ownerID, owner := range ownerByID {
		if owner.Engine == "pglite" && pgliteOwnerUse[ownerID] == 0 {
			t.Errorf("PGlite owner %s has no scenario and paired native owner", ownerID)
		}
		if !usedOwners[ownerID] {
			t.Errorf("owner %s is orphaned from every scenario/checkpoint", ownerID)
		}
	}
}

func loadCoverageInventory(t *testing.T, path string) coverageInventory {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var inventory coverageInventory
	if err := decoder.Decode(&inventory); err != nil {
		t.Fatalf("decode strict coverage schema: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("coverage inventory must contain exactly one JSON value: %v", err)
	}
	return inventory
}

func registerUniqueID(t *testing.T, seen map[string]string, id, kind string) {
	t.Helper()
	if !inventoryIDPattern.MatchString(id) {
		t.Errorf("%s ID %q is not stable lowercase identifier syntax", kind, id)
	}
	if previous, ok := seen[id]; ok {
		t.Errorf("duplicate global ID %q used by %s and %s", id, previous, kind)
		return
	}
	seen[id] = kind
}

func requireText(t *testing.T, field, value string) {
	t.Helper()
	if strings.TrimSpace(value) == "" {
		t.Errorf("%s is required", field)
	}
}

func requireRepoPath(t *testing.T, repoRoot, field, path string) {
	t.Helper()
	requireText(t, field, path)
	clean := filepath.Clean(path)
	if filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		t.Errorf("%s %q escapes repository root %s", field, path, repoRoot)
	}
}

func assertExistingTestOwner(t *testing.T, repoRoot string, owner coverageOwner) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(repoRoot, owner.Path))
	if err != nil {
		t.Errorf("existing owner %s: %v", owner.ID, err)
		return
	}
	declaration := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(owner.Test) + `\s*\(`)
	if !declaration.Match(contents) {
		t.Errorf("existing owner %s does not define %s in %s", owner.ID, owner.Test, owner.Path)
	}
}

func assertUniqueStrings(t *testing.T, field string, values []string) {
	t.Helper()
	seen := make(map[string]bool)
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s contains an empty value", field)
		}
		if seen[value] {
			t.Errorf("%s contains duplicate %q", field, value)
		}
		seen[value] = true
	}
}

func validatePGliteCoverage(t *testing.T, scenario coverageScenario, owners map[string]coverageOwner, nativeOwners map[string]bool, usedOwners map[string]bool, uses map[string]int) {
	t.Helper()
	switch scenario.PGlite.Eligibility {
	case "eligible", "eligible_planned":
		if strings.TrimSpace(scenario.PGlite.Reason) != "" {
			t.Errorf("scenario %s eligible PGlite entry must not carry an ineligibility reason", scenario.ID)
		}
		owner, ok := owners[scenario.PGlite.OwnerID]
		if !ok {
			t.Errorf("scenario %s references unknown PGlite owner %s", scenario.ID, scenario.PGlite.OwnerID)
		} else if owner.Engine != "pglite" {
			t.Errorf("scenario %s PGlite owner %s has engine %s", scenario.ID, owner.ID, owner.Engine)
		} else if scenario.PGlite.Eligibility == "eligible" && owner.Status != "existing" {
			t.Errorf("scenario %s claims delivered PGlite eligibility but owner %s is %s", scenario.ID, owner.ID, owner.Status)
		}
		if !nativeOwners[scenario.PGlite.NativeOwnerID] {
			t.Errorf("scenario %s PGlite variant is orphaned: paired native owner %s is not a scenario owner", scenario.ID, scenario.PGlite.NativeOwnerID)
		}
		if len(scenario.PGlite.Capabilities) == 0 {
			t.Errorf("scenario %s PGlite variant declares no required capabilities", scenario.ID)
		}
		assertUniqueStrings(t, scenario.ID+" pglite.capabilities", scenario.PGlite.Capabilities)
		usedOwners[scenario.PGlite.OwnerID] = true
		uses[scenario.PGlite.OwnerID]++
	case "not_eligible":
		requireText(t, scenario.ID+" pglite.reason", scenario.PGlite.Reason)
		if scenario.PGlite.OwnerID != "" || scenario.PGlite.NativeOwnerID != "" || len(scenario.PGlite.Capabilities) != 0 {
			t.Errorf("scenario %s ineligible PGlite entry must not name owners or capabilities", scenario.ID)
		}
	default:
		t.Errorf("scenario %s PGlite eligibility = %q, want eligible, eligible_planned, or not_eligible", scenario.ID, scenario.PGlite.Eligibility)
	}
}

func validateDimensions(t *testing.T, scenario coverageScenario, signatures map[string]string) {
	t.Helper()
	values := []string{
		scenario.Dimensions.DDL,
		scenario.Dimensions.Data,
		scenario.Dimensions.Client,
		scenario.Dimensions.Decision,
		scenario.Dimensions.PlanEvolution,
		scenario.Dimensions.Environment,
	}
	labels := []string{"ddl", "data", "client", "decision", "plan_evolution", "environment"}
	for i, value := range values {
		requireText(t, scenario.ID+" dimensions."+labels[i], value)
	}
	signature := strings.Join(values, "\x00")
	if previous, ok := signatures[signature]; ok {
		t.Errorf("scenario %s duplicates the complete dimension signature of %s; add a real DDL, data, client, decision, plan-evolution, or environment dimension", scenario.ID, previous)
		return
	}
	signatures[signature] = scenario.ID
}

func validateCheckpoints(t *testing.T, scenario coverageScenario, mandatory []string, owners map[string]coverageOwner, nativeOwners map[string]bool, usedOwners map[string]bool) {
	t.Helper()
	want := make(map[string]bool, len(mandatory))
	for _, checkpoint := range mandatory {
		want[checkpoint] = true
	}
	for checkpoint := range scenario.Checkpoints {
		if !want[checkpoint] {
			t.Errorf("scenario %s has unknown checkpoint %s", scenario.ID, checkpoint)
		}
	}
	for _, checkpointName := range mandatory {
		checkpoint, ok := scenario.Checkpoints[checkpointName]
		if !ok {
			t.Errorf("scenario %s omits mandatory checkpoint %s without a reason", scenario.ID, checkpointName)
			continue
		}
		hasOwners := len(checkpoint.OwnerIDs) > 0
		hasReason := strings.TrimSpace(checkpoint.OmittedReason) != ""
		if hasOwners == hasReason {
			t.Errorf("scenario %s checkpoint %s must have owner_ids or omitted_reason, exclusively", scenario.ID, checkpointName)
			continue
		}
		if hasReason {
			continue
		}
		assertUniqueStrings(t, scenario.ID+" checkpoint "+checkpointName+" owner_ids", checkpoint.OwnerIDs)
		for _, ownerID := range checkpoint.OwnerIDs {
			owner, ok := owners[ownerID]
			if !ok {
				t.Errorf("scenario %s checkpoint %s references unknown owner %s", scenario.ID, checkpointName, ownerID)
				continue
			}
			if owner.Engine != "native_postgres" {
				t.Errorf("scenario %s checkpoint %s is owned by non-authoritative %s engine", scenario.ID, checkpointName, owner.Engine)
			}
			if owner.Status != "existing" {
				t.Errorf("scenario %s checkpoint %s is owned by %s, but that owner is only %s", scenario.ID, checkpointName, ownerID, owner.Status)
			}
			if !nativeOwners[ownerID] {
				t.Errorf("scenario %s checkpoint %s owner %s is not declared in native_owner_ids", scenario.ID, checkpointName, ownerID)
			}
			usedOwners[ownerID] = true
		}
	}
}
