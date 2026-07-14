package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
)

var editablePhases = []string{"expand", "migrate", "contract"}

type SQLBatch struct {
	SQL           string
	Transactional bool
}

type Assertion struct {
	ID                   string
	SQL                  string
	DevSafePostcondition bool
}

type EditConflict struct {
	Path            string  `json:"path"`
	Reason          string  `json:"reason"`
	OldGeneratedSQL *string `json:"old_generated_sql,omitempty"`
	CurrentSQL      *string `json:"current_sql,omitempty"`
	NewGeneratedSQL *string `json:"new_generated_sql,omitempty"`
	Resolution      string  `json:"resolution"`
}

// EditReconciliation describes the conservative three-way reconciliation used
// when the structural plan changes beneath developer-owned phase SQL. The
// comparison is deliberately phase-grained: onwardpg preserves a phase only
// when the generator did not also change it, and never guesses how to merge
// two changes to the same phase.
type EditReconciliation struct {
	Outcome   string         `json:"outcome"`
	Preserved []string       `json:"preserved,omitempty"`
	Refreshed []string       `json:"refreshed,omitempty"`
	Conflicts []EditConflict `json:"conflicts,omitempty"`
}

// PrepareEdited reads a planned bundle whose phase files may have been edited,
// adds or refreshes their receipts, and returns the receipted artifact in
// memory. All non-SQL receipts remain strict. Nothing is written until
// InstallReceipts.
func PrepareEdited(directory string) (Artifact, error) {
	manifestData, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return Artifact{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return Artifact{}, fmt.Errorf("decode bundle manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate existing manifest: %w", err)
	}
	canonical, err := jsonDocument(manifest)
	if err != nil {
		return Artifact{}, err
	}
	if !bytes.Equal(manifestData, canonical) {
		return Artifact{}, fmt.Errorf("manifest.json is not the canonical receipted document")
	}
	if manifest.State != string(protocol.Planned) && manifest.State != string(protocol.NeedsSQLEdits) {
		return Artifact{}, fmt.Errorf("only a planned or needs_sql_edits bundle can receipt SQL edits")
	}

	files, err := readArtifactFiles(directory)
	if err != nil {
		return Artifact{}, err
	}
	return prepareEditedArtifact(manifest, files)
}

// PrepareEditedFiles replaces the editable files of a valid generated artifact
// and receipts their exact bytes. It is used when a selected feature draft is
// regenerated and its non-conflicting developer-owned SQL is carried forward.
func PrepareEditedFiles(base Artifact, editable map[string][]byte) (Artifact, error) {
	if err := base.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate generated bundle: %w", err)
	}
	if base.Manifest.State != string(protocol.Planned) && base.Manifest.State != string(protocol.NeedsSQLEdits) {
		return Artifact{}, fmt.Errorf("only a planned or needs_sql_edits bundle can receipt SQL edits")
	}
	files := make(map[string][]byte, len(base.Files)+len(editable))
	for name, body := range base.Files {
		files[name] = append([]byte(nil), body...)
	}
	for _, phase := range editablePhases {
		delete(files, filepath.ToSlash(filepath.Join("phases", phase+".sql")))
	}
	delete(files, "verify.sql")
	for name, body := range editable {
		if name != "verify.sql" && !isEditablePhasePath(name) {
			return Artifact{}, fmt.Errorf("editable artifact path %q is invalid", name)
		}
		files[name] = append([]byte(nil), body...)
	}
	return prepareEditedArtifact(base.Manifest, files)
}

// ReconcileEditedDraft carries developer-owned SQL from previous into a newly
// generated artifact. plan.json reconstructs the generator's previous output,
// giving us three versions for each phase: old generated, old edited, and new
// generated. verify.sql is always developer-owned. Conflicting phase changes
// are reported without producing a replacement artifact.
func ReconcileEditedDraft(previous, generated Artifact) (Artifact, EditReconciliation, error) {
	report := EditReconciliation{Outcome: "conflict"}
	if err := previous.Validate(); err != nil {
		return Artifact{}, report, fmt.Errorf("validate previous edited bundle: %w", err)
	}
	if previous.Manifest.PhaseSource != "edited" || previous.Manifest.State != "planned" {
		return Artifact{}, report, fmt.Errorf("previous bundle is not a receipted edited draft")
	}
	if err := generated.Validate(); err != nil {
		return Artifact{}, report, fmt.Errorf("validate newly generated bundle: %w", err)
	}
	if generated.Manifest.PhaseSource != "generated" || generated.Manifest.State != string(protocol.Planned) && generated.Manifest.State != string(protocol.NeedsSQLEdits) {
		return Artifact{}, report, fmt.Errorf("replacement bundle is not a generated draft eligible for SQL reconciliation")
	}
	if previous.Manifest.BundleID != generated.Manifest.BundleID || previous.Manifest.Target != generated.Manifest.Target {
		return Artifact{}, report, fmt.Errorf("edited and generated bundle identities differ")
	}

	var oldPlan protocol.Result
	if err := json.Unmarshal(previous.Files["plan.json"], &oldPlan); err != nil {
		return Artifact{}, report, fmt.Errorf("decode previous plan.json: %w", err)
	}
	_, oldGenerated, err := renderPhases(oldPlan)
	if err != nil {
		return Artifact{}, report, fmt.Errorf("render previous generated phases: %w", err)
	}

	merged := make(map[string][]byte)
	paths := make([]string, 0, len(editablePhases)+1)
	for _, phase := range editablePhases {
		paths = append(paths, filepath.ToSlash(filepath.Join("phases", phase+".sql")))
	}
	paths = append(paths, "verify.sql")
	for _, name := range paths {
		oldGeneratedBody, oldGeneratedExists := oldGenerated[name]
		previousBody, previousExists := previous.Files[name]
		newGeneratedBody, newGeneratedExists := generated.Files[name]

		switch {
		case sameOptionalBytes(previousBody, previousExists, oldGeneratedBody, oldGeneratedExists):
			if newGeneratedExists {
				merged[name] = append([]byte(nil), newGeneratedBody...)
			}
			if !sameOptionalBytes(newGeneratedBody, newGeneratedExists, oldGeneratedBody, oldGeneratedExists) {
				report.Refreshed = append(report.Refreshed, name)
			}
		case sameOptionalBytes(newGeneratedBody, newGeneratedExists, oldGeneratedBody, oldGeneratedExists):
			if previousExists {
				merged[name] = append([]byte(nil), previousBody...)
			}
			report.Preserved = append(report.Preserved, name)
		case sameOptionalBytes(previousBody, previousExists, newGeneratedBody, newGeneratedExists):
			if newGeneratedExists {
				merged[name] = append([]byte(nil), newGeneratedBody...)
			}
			report.Preserved = append(report.Preserved, name)
		default:
			report.Conflicts = append(report.Conflicts, EditConflict{
				Path:            name,
				Reason:          "developer and generator both changed this phase from the previous generated version",
				OldGeneratedSQL: optionalSQL(oldGeneratedBody, oldGeneratedExists),
				CurrentSQL:      optionalSQL(previousBody, previousExists),
				NewGeneratedSQL: optionalSQL(newGeneratedBody, newGeneratedExists),
				Resolution:      "edit this path to preserve the intended current SQL and incorporate the new generated structural work, then run onwardpg verify",
			})
		}
	}
	if len(report.Conflicts) > 0 {
		return Artifact{}, report, nil
	}
	artifact, err := PrepareEditedFiles(generated, merged)
	if err != nil {
		return Artifact{}, report, err
	}
	report.Outcome = "reconciled"
	return artifact, report, nil
}

func optionalSQL(body []byte, exists bool) *string {
	if !exists {
		return nil
	}
	value := string(body)
	return &value
}

func sameOptionalBytes(left []byte, leftExists bool, right []byte, rightExists bool) bool {
	return leftExists == rightExists && (!leftExists || bytes.Equal(left, right))
}

func prepareEditedArtifact(manifest Manifest, files map[string][]byte) (Artifact, error) {
	previous := manifest.Phases
	manifest.Phases = make(map[string]PhaseArtifact)
	for _, phase := range editablePhases {
		name := filepath.ToSlash(filepath.Join("phases", phase+".sql"))
		body, exists := files[name]
		if !exists {
			continue
		}
		if bytes.Contains(body, []byte("ONWARDPG TODO")) {
			return Artifact{}, fmt.Errorf("%s contains an unresolved ONWARDPG TODO", name)
		}
		var fallback *bool
		if receipt, ok := previous[phase]; ok {
			fallback = receipt.Transactional
		}
		batches, err := ParsePhaseSQL(body, fallback)
		if err != nil {
			return Artifact{}, fmt.Errorf("parse %s: %w", name, err)
		}
		receipt := PhaseArtifact{Path: name, Digest: Digest(body)}
		if mode, uniform := uniformTransactionMode(batches); uniform {
			receipt.Transactional = &mode
		}
		manifest.Phases[phase] = receipt
	}
	manifest.PhaseSource = "edited"
	manifest.State = string(protocol.Planned)
	manifest.VerificationDigest = ""
	if verification, exists := files["verify.sql"]; exists {
		if _, err := ParseAssertions(verification); err != nil {
			return Artifact{}, fmt.Errorf("parse verify.sql: %w", err)
		}
		manifest.VerificationDigest = Digest(verification)
	}
	if manifest.History != nil {
		digest, err := HistoryEntryDigest(manifest)
		if err != nil {
			return Artifact{}, err
		}
		manifest.History.EntryDigest = digest
	}
	manifestData, err := jsonDocument(manifest)
	if err != nil {
		return Artifact{}, err
	}
	files["manifest.json"] = manifestData
	artifact := Artifact{Manifest: manifest, Files: files}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate edited bundle: %w", err)
	}
	return artifact, nil
}

func isEditablePhasePath(name string) bool {
	for _, phase := range editablePhases {
		if name == filepath.ToSlash(filepath.Join("phases", phase+".sql")) {
			return true
		}
	}
	return false
}

// InstallReceipts atomically installs only the manifest produced by
// PrepareEdited. It re-reads all files under a short filesystem lock so a
// concurrent edit cannot be receipted accidentally. This records evidence; it
// does not freeze the selected feature draft.
func InstallReceipts(directory string, expected Artifact) error {
	if expected.Manifest.PhaseSource != "edited" {
		return fmt.Errorf("only an edited bundle may install refreshed receipts")
	}
	lock := directory + ".onwardpg-receipt-lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("bundle receipt update is already in progress: %s", lock)
		}
		return fmt.Errorf("acquire bundle receipt lock: %w", err)
	}
	defer os.Remove(lock)
	current, err := PrepareEdited(directory)
	if err != nil {
		return err
	}
	if current.Manifest.History == nil || expected.Manifest.History == nil || current.Manifest.History.EntryDigest != expected.Manifest.History.EntryDigest {
		return fmt.Errorf("bundle changed after disposable verification")
	}
	temporary, err := os.CreateTemp(filepath.Dir(directory), ".onwardpg-manifest-")
	if err != nil {
		return fmt.Errorf("create temporary manifest: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(current.Files["manifest.json"]); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(directory, "manifest.json")); err != nil {
		return fmt.Errorf("install receipted manifest: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync receipted bundle directory: %w", err)
	}
	installed, err := Read(directory)
	if err != nil {
		return fmt.Errorf("validate bundle after receipt installation: %w", err)
	}
	if !artifactsEqual(installed, current) {
		return fmt.Errorf("bundle changed while receipts were installed")
	}
	return nil
}

// ParsePhaseSQL splits an editable phase only at explicit onwardpg batch
// comments. Without directives, the whole file is one batch using fallback or
// a transactional default. SQL itself is preserved byte-for-byte.
func ParsePhaseSQL(body []byte, fallback *bool) ([]SQLBatch, error) {
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("SQL contains a NUL byte")
	}
	lines := strings.SplitAfter(string(body), "\n")
	var preamble, current strings.Builder
	var batches []SQLBatch
	seenDirective := false
	transactional := true
	flush := func() {
		if strings.TrimSpace(current.String()) == "" {
			current.Reset()
			return
		}
		batches = append(batches, SQLBatch{SQL: current.String(), Transactional: transactional})
		current.Reset()
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		mode, directive, invalid := parseBatchDirective(trimmed)
		if invalid {
			return nil, fmt.Errorf("invalid batch directive %q", trimmed)
		}
		if !directive {
			if seenDirective {
				current.WriteString(line)
			} else {
				preamble.WriteString(line)
			}
			continue
		}
		if seenDirective {
			flush()
		} else {
			current.WriteString(preamble.String())
			preamble.Reset()
			seenDirective = true
		}
		transactional = mode
		current.WriteString(line)
	}
	if seenDirective {
		flush()
		return batches, nil
	}
	if strings.TrimSpace(preamble.String()) == "" {
		return nil, nil
	}
	transactional = true
	if fallback != nil {
		transactional = *fallback
	}
	return []SQLBatch{{SQL: preamble.String(), Transactional: transactional}}, nil
}

func parseBatchDirective(line string) (transactional, directive, invalid bool) {
	switch line {
	case "-- onwardpg:batch transactional":
		return true, true, false
	case "-- onwardpg:batch nontransactional":
		return false, true, false
	default:
		if strings.HasPrefix(line, "-- onwardpg:batch") {
			return false, false, true
		}
		return false, false, false
	}
}

// ParseAssertions treats verify.sql as one boolean query unless explicit
// "-- onwardpg:assert ID" comments split it into multiple assertions. An
// assertion becomes eligible for caller-owned development evidence only with
// the explicit "-- onwardpg:dev-postcondition" marker. The marker does not
// authorize writes: callers still execute it in a PostgreSQL read-only
// transaction.
func ParseAssertions(body []byte) ([]Assertion, error) {
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("verification SQL contains a NUL byte")
	}
	lines := strings.SplitAfter(string(body), "\n")
	var preamble, current strings.Builder
	var assertions []Assertion
	seen := false
	id := "verification"
	devSafe := false
	flush := func() {
		if strings.TrimSpace(current.String()) == "" {
			current.Reset()
			return
		}
		assertions = append(assertions, Assertion{ID: id, SQL: current.String(), DevSafePostcondition: devSafe})
		current.Reset()
		devSafe = false
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- onwardpg:assert") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "-- onwardpg:assert"))
			if value == "" || strings.ContainsAny(value, " \t\r\n\x00") {
				return nil, fmt.Errorf("invalid assertion directive %q", trimmed)
			}
			if seen {
				flush()
			} else {
				current.WriteString(preamble.String())
				preamble.Reset()
				seen = true
			}
			id = value
			current.WriteString(line)
			continue
		}
		if trimmed == "-- onwardpg:dev-postcondition" {
			if !seen {
				return nil, fmt.Errorf("development postcondition marker requires an onwardpg assertion")
			}
			if devSafe {
				return nil, fmt.Errorf("duplicate development postcondition marker for assertion %q", id)
			}
			devSafe = true
			current.WriteString(line)
			continue
		}
		if seen {
			current.WriteString(line)
		} else {
			preamble.WriteString(line)
		}
	}
	if seen {
		flush()
	} else if strings.TrimSpace(preamble.String()) != "" {
		assertions = append(assertions, Assertion{ID: id, SQL: preamble.String()})
	}
	seenIDs := make(map[string]bool, len(assertions))
	for _, assertion := range assertions {
		if seenIDs[assertion.ID] {
			return nil, fmt.Errorf("assertion id %q is duplicated", assertion.ID)
		}
		seenIDs[assertion.ID] = true
	}
	return assertions, nil
}

func uniformTransactionMode(batches []SQLBatch) (bool, bool) {
	if len(batches) == 0 {
		return true, true
	}
	mode := batches[0].Transactional
	for _, batch := range batches[1:] {
		if batch.Transactional != mode {
			return false, false
		}
	}
	return mode, true
}

func readArtifactFiles(directory string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	err := filepath.WalkDir(directory, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == directory {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle contains symlink %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if err := validateArtifactPath(relative); err != nil {
			return err
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[relative] = body
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read bundle files: %w", err)
	}
	return files, nil
}
