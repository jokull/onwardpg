// Package bundle turns a planner result into a deterministic, reviewable
// migration receipt directory. It does not inspect Git, invoke schema tools,
// or apply SQL; those lifecycle concerns build on this integrity boundary.
package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
)

const Version = "onwardpg.bundle/v1"

var rootHistoryDigest = digestFrames([]byte("onwardpg.history/v1"), []byte("root"))

func HistoryRootDigest() string { return rootHistoryDigest }

var (
	fingerprintPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	secretDSNPattern   = regexp.MustCompile(`(?i)(?:^|[\s;])(?:password|passfile|sslkey)\s*=`)
)

type SourceReceipt struct {
	Kind          string `json:"kind"`
	Description   string `json:"description"`
	Fingerprint   string `json:"fingerprint"`
	GitCommit     string `json:"git_commit,omitempty"`
	PostgresMajor int    `json:"postgres_major,omitempty"`
}

type PlannerOptions struct {
	ConcurrentIndexes bool    `json:"concurrent_indexes,omitempty"`
	IfNotExists       bool    `json:"if_not_exists,omitempty"`
	IfExists          bool    `json:"if_exists,omitempty"`
	CascadeDrops      bool    `json:"cascade_drops,omitempty"`
	SchemaQualifier   *string `json:"schema_qualifier,omitempty"`
}

type PlannerReceipt struct {
	Version         string         `json:"version"`
	Options         PlannerOptions `json:"options"`
	IgnoreSelectors []string       `json:"ignore_selectors,omitempty"`
}

type SchemaSquareReceipt struct {
	BaseCodeFingerprint    string `json:"base_code_fingerprint"`
	BaseHistoryFingerprint string `json:"base_history_fingerprint"`
	HeadCodeFingerprint    string `json:"head_code_fingerprint"`
	HeadHistoryFingerprint string `json:"head_history_fingerprint,omitempty"`
	BaseHistoryDigest      string `json:"base_history_digest,omitempty"`
	BaseIntegrity          string `json:"base_integrity"`
	HeadHistoryFidelity    string `json:"head_history_fidelity"`
}

// HistoryReceipt links a bundle to the exact protected history head it was
// planned from. EntryDigest commits to the complete manifest receipt (with the
// entry digest itself cleared), so directory names and filesystem ordering are
// never part of migration order.
type HistoryReceipt struct {
	ParentDigest string `json:"parent_digest"`
	EntryDigest  string `json:"entry_digest"`
}

type Lineage struct {
	Relationship string `json:"relationship"`
	BundleID     string `json:"bundle_id"`
	Generation   int    `json:"generation"`
}

type PhaseArtifact struct {
	Path          string `json:"path"`
	Digest        string `json:"digest"`
	Transactional *bool  `json:"transactional,omitempty"`
}

type FileReceipt struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type Manifest struct {
	ProtocolVersion string `json:"protocol_version"`
	BundleID        string `json:"bundle_id"`
	Generation      int    `json:"generation"`
	Target          string `json:"target"`
	Purpose         string `json:"purpose"`
	Mode            string `json:"mode"`
	State           string `json:"state"`

	BaseRef      string `json:"base_ref,omitempty"`
	BaseCommit   string `json:"base_commit,omitempty"`
	HeadRevision string `json:"head_revision,omitempty"`

	BaselineSource SourceReceipt        `json:"baseline_source"`
	DesiredSource  SourceReceipt        `json:"desired_source"`
	Planner        PlannerReceipt       `json:"planner"`
	SchemaSquare   *SchemaSquareReceipt `json:"schema_square,omitempty"`
	History        *HistoryReceipt      `json:"history,omitempty"`

	ResultDigest  string                   `json:"result_digest"`
	PlanDigest    string                   `json:"plan_digest,omitempty"`
	Decisions     []FileReceipt            `json:"decisions,omitempty"`
	AnswersDigest string                   `json:"answers_digest,omitempty"`
	IntentDigest  string                   `json:"intent_digest,omitempty"`
	Phases        map[string]PhaseArtifact `json:"phases,omitempty"`
	Lineage       *Lineage                 `json:"lineage,omitempty"`
}

type Metadata struct {
	BundleID            string
	Generation          int
	Target              string
	Purpose             string
	Mode                string
	BaseRef             string
	BaseCommit          string
	HeadRevision        string
	BaselineSource      SourceReceipt
	DesiredSource       SourceReceipt
	Planner             PlannerReceipt
	SchemaSquare        *SchemaSquareReceipt
	HistoryParentDigest string
	Lineage             *Lineage
}

type Input struct {
	Metadata Metadata
	Result   protocol.Result
	Answers  *protocol.Answers
	Intent   string
	Attempt  int
}

type Artifact struct {
	Manifest Manifest
	Files    map[string][]byte
}

func (a Artifact) Validate() error {
	if err := a.Manifest.Validate(); err != nil {
		return err
	}
	manifestBytes, err := jsonDocument(a.Manifest)
	if err != nil {
		return err
	}
	if string(a.Files["manifest.json"]) != string(manifestBytes) {
		return fmt.Errorf("manifest.json does not match the in-memory manifest")
	}
	required := map[string]string{"manifest.json": Digest(manifestBytes)}
	switch a.Manifest.State {
	case string(protocol.Planned):
		required["plan.json"] = a.Manifest.PlanDigest
		if a.Manifest.ResultDigest != a.Manifest.PlanDigest {
			return fmt.Errorf("planned result_digest must equal plan_digest")
		}
		var result protocol.Result
		if err := json.Unmarshal(a.Files["plan.json"], &result); err != nil {
			return fmt.Errorf("decode plan.json: %w", err)
		}
		if err := validatePlanStatements(result); err != nil {
			return fmt.Errorf("validate plan.json: %w", err)
		}
		phases, phaseFiles, err := renderPhases(result)
		if err != nil {
			return fmt.Errorf("render plan phases: %w", err)
		}
		if !samePhaseReceipts(phases, a.Manifest.Phases) {
			return fmt.Errorf("phase receipts do not match plan.json")
		}
		for name, expected := range phaseFiles {
			if string(a.Files[name]) != string(expected) {
				return fmt.Errorf("phase artifact %s does not match manifest digest or plan.json", name)
			}
		}
	case string(protocol.NeedsInput):
		if !decisionDigestExists(a.Manifest.Decisions, a.Manifest.ResultDigest) {
			return fmt.Errorf("needs_input result_digest must match a decision receipt")
		}
	case string(protocol.Unsupported):
		if !decisionDigestExists(a.Manifest.Decisions, a.Manifest.ResultDigest) {
			return fmt.Errorf("unsupported result_digest must match a decision receipt")
		}
	}
	for _, decision := range a.Manifest.Decisions {
		required[decision.Path] = decision.Digest
	}
	if a.Manifest.AnswersDigest != "" {
		required["answers.json"] = a.Manifest.AnswersDigest
	}
	if a.Manifest.IntentDigest != "" {
		required["intent.md"] = a.Manifest.IntentDigest
	}
	for _, phase := range a.Manifest.Phases {
		required[phase.Path] = phase.Digest
	}
	if len(required) != len(a.Files) {
		return fmt.Errorf("bundle contains unexpected or unrecorded files")
	}
	for name, digest := range required {
		data, exists := a.Files[name]
		if !exists {
			return fmt.Errorf("bundle artifact %s is missing", name)
		}
		if name != "manifest.json" && Digest(data) != digest {
			return fmt.Errorf("bundle artifact %s does not match manifest digest", name)
		}
	}
	return nil
}

func samePhaseReceipts(first, second map[string]PhaseArtifact) bool {
	if len(first) != len(second) {
		return false
	}
	for phase, artifact := range first {
		if other, ok := second[phase]; !ok || !reflect.DeepEqual(artifact, other) {
			return false
		}
	}
	return true
}

func Build(input Input) (Artifact, error) {
	if input.Attempt == 0 {
		input.Attempt = 1
	}
	if input.Metadata.Generation == 0 {
		input.Metadata.Generation = 1
	}
	if input.Result.ProtocolVersion != protocol.Version {
		return Artifact{}, fmt.Errorf("result protocol_version is %q, want %q", input.Result.ProtocolVersion, protocol.Version)
	}
	if input.Result.Status != protocol.Planned && input.Result.Status != protocol.NeedsInput && input.Result.Status != protocol.Unsupported {
		return Artifact{}, fmt.Errorf("cannot bundle planner status %q", input.Result.Status)
	}
	if input.Metadata.BaselineSource.Fingerprint != input.Result.CurrentFingerprint {
		return Artifact{}, fmt.Errorf("baseline source fingerprint does not match planner current fingerprint")
	}
	if input.Metadata.DesiredSource.Fingerprint != input.Result.DesiredFingerprint {
		return Artifact{}, fmt.Errorf("desired source fingerprint does not match planner desired fingerprint")
	}

	resultBytes, err := jsonDocument(input.Result)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode planner result: %w", err)
	}
	manifest := Manifest{
		ProtocolVersion: Version,
		BundleID:        input.Metadata.BundleID, Generation: input.Metadata.Generation,
		Target: input.Metadata.Target, Purpose: input.Metadata.Purpose, Mode: input.Metadata.Mode,
		State: string(input.Result.Status), BaseRef: input.Metadata.BaseRef,
		BaseCommit: input.Metadata.BaseCommit, HeadRevision: input.Metadata.HeadRevision,
		BaselineSource: input.Metadata.BaselineSource, DesiredSource: input.Metadata.DesiredSource,
		Planner: input.Metadata.Planner, SchemaSquare: input.Metadata.SchemaSquare,
		ResultDigest: Digest(resultBytes), Lineage: input.Metadata.Lineage,
	}
	files := make(map[string][]byte)
	switch input.Result.Status {
	case protocol.NeedsInput, protocol.Unsupported:
		name := fmt.Sprintf("decisions/attempt-%03d.json", input.Attempt)
		files[name] = resultBytes
		manifest.Decisions = []FileReceipt{{Path: name, Digest: Digest(resultBytes)}}
	case protocol.Planned:
		if err := validatePlanStatements(input.Result); err != nil {
			return Artifact{}, err
		}
		files["plan.json"] = resultBytes
		manifest.PlanDigest = Digest(resultBytes)
		phases, phaseFiles, err := renderPhases(input.Result)
		if err != nil {
			return Artifact{}, err
		}
		manifest.Phases = phases
		for name, data := range phaseFiles {
			files[name] = data
		}
	}
	if input.Answers != nil {
		if input.Answers.ProtocolVersion != protocol.Version || input.Answers.CurrentFingerprint != input.Result.CurrentFingerprint || input.Answers.DesiredFingerprint != input.Result.DesiredFingerprint {
			return Artifact{}, fmt.Errorf("answer receipt does not match bundled planner result")
		}
		answerBytes, err := jsonDocument(input.Answers)
		if err != nil {
			return Artifact{}, fmt.Errorf("encode answers: %w", err)
		}
		files["answers.json"] = answerBytes
		manifest.AnswersDigest = Digest(answerBytes)
	}
	if strings.TrimSpace(input.Intent) != "" {
		intent := []byte(strings.TrimRight(input.Intent, "\n") + "\n")
		files["intent.md"] = intent
		manifest.IntentDigest = Digest(intent)
	}
	if input.Metadata.HistoryParentDigest != "" {
		manifest.History = &HistoryReceipt{ParentDigest: input.Metadata.HistoryParentDigest}
		entryDigest, err := HistoryEntryDigest(manifest)
		if err != nil {
			return Artifact{}, fmt.Errorf("compute history entry digest: %w", err)
		}
		manifest.History.EntryDigest = entryDigest
	}
	if err := manifest.Validate(); err != nil {
		return Artifact{}, err
	}
	manifestBytes, err := jsonDocument(manifest)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode bundle manifest: %w", err)
	}
	files["manifest.json"] = manifestBytes
	artifact := Artifact{Manifest: manifest, Files: files}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestFrames(parts ...[]byte) string {
	hash := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(part)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// HistoryEntryDigest returns the canonical hash-chain identity for a manifest.
// The manifest must already contain a parent digest; the entry digest field is
// cleared before encoding to avoid a recursive hash.
func HistoryEntryDigest(manifest Manifest) (string, error) {
	if manifest.History == nil || !fingerprintPattern.MatchString(manifest.History.ParentDigest) {
		return "", fmt.Errorf("history parent digest is required")
	}
	copy := manifest
	history := *manifest.History
	history.EntryDigest = ""
	copy.History = &history
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	return digestFrames([]byte(Version), []byte(history.ParentDigest), data), nil
}

// ResultDigest returns the canonical digest used by bundle manifests and
// freshness observations for a planner result.
func ResultDigest(result protocol.Result) (string, error) {
	data, err := jsonDocument(result)
	if err != nil {
		return "", err
	}
	return Digest(data), nil
}

// DecisionQuestions returns the unique staged questions recorded by a bundle.
// Conflicting receipts within one generation are rejected rather than allowing
// answer rebinding to choose one implicitly.
func DecisionQuestions(artifact Artifact) ([]protocol.Question, error) {
	questions := make(map[string]protocol.Question)
	for _, receipt := range artifact.Manifest.Decisions {
		var result protocol.Result
		if err := json.Unmarshal(artifact.Files[receipt.Path], &result); err != nil {
			return nil, fmt.Errorf("decode %s: %w", receipt.Path, err)
		}
		for _, question := range result.Questions {
			id := question.Kind + ":" + question.Key
			if previous, exists := questions[id]; exists && !reflect.DeepEqual(previous, question) {
				return nil, fmt.Errorf("decision receipts contain conflicting question %s", id)
			}
			questions[id] = question
		}
	}
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]protocol.Question, 0, len(ids))
	for _, id := range ids {
		result = append(result, questions[id])
	}
	return result, nil
}

func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func validatePlanStatements(result protocol.Result) error {
	ids := make(map[string]protocol.Statement, len(result.Statements))
	for _, statement := range result.Statements {
		if statement.ID == "" {
			return fmt.Errorf("planned statement is missing a stable id")
		}
		if _, exists := ids[statement.ID]; exists {
			return fmt.Errorf("planned statement id %q is duplicated", statement.ID)
		}
		ids[statement.ID] = statement
	}
	seen := make(map[string]bool, len(ids))
	for _, batch := range result.Batches {
		if batch.ID == "" {
			return fmt.Errorf("planned batch is missing an id")
		}
		for _, statement := range batch.Statements {
			original, exists := ids[statement.ID]
			if !exists || !sameStatement(original, statement) {
				return fmt.Errorf("batch %q contains unknown or inconsistent statement %q", batch.ID, statement.ID)
			}
			if seen[statement.ID] {
				return fmt.Errorf("statement %q appears in more than one batch", statement.ID)
			}
			seen[statement.ID] = true
		}
	}
	if len(seen) != len(ids) {
		return fmt.Errorf("planned batches cover %d of %d statements", len(seen), len(ids))
	}
	return nil
}

func sameStatement(left, right protocol.Statement) bool {
	leftBytes, _ := json.Marshal(left)
	rightBytes, _ := json.Marshal(right)
	return string(leftBytes) == string(rightBytes)
}

func renderPhases(result protocol.Result) (map[string]PhaseArtifact, map[string][]byte, error) {
	phases := make(map[string]PhaseArtifact)
	files := make(map[string][]byte)
	for _, phase := range []string{"expand", "migrate", "manual", "contract"} {
		var batches []protocol.Batch
		var statements []protocol.Statement
		transactional := true
		mixed := false
		seenMode := false
		for _, batch := range result.Batches {
			if batch.Phase != phase {
				continue
			}
			batches = append(batches, batch)
			statements = append(statements, batch.Statements...)
			if seenMode && transactional != batch.Transactional {
				mixed = true
			}
			transactional, seenMode = batch.Transactional, true
		}
		if len(batches) == 0 {
			continue
		}
		rendered := protocol.RenderSQL(protocol.Result{Status: protocol.Planned, Statements: statements, Batches: batches}, "")
		data := []byte(strings.TrimRight(rendered, "\n") + "\n")
		name := path.Join("phases", phase+".sql")
		files[name] = data
		artifact := PhaseArtifact{Path: name, Digest: Digest(data)}
		if !mixed {
			value := transactional
			artifact.Transactional = &value
		}
		phases[phase] = artifact
	}
	return phases, files, nil
}

// RenderReplaySQL renders phase artifacts exactly as bundle history will replay
// them, in lifecycle order. It is used to prove the proposed side of the
// schema square before a ready bundle is written.
func RenderReplaySQL(result protocol.Result) ([]byte, error) {
	_, files, err := renderPhases(result)
	if err != nil {
		return nil, err
	}
	var sql strings.Builder
	for _, phase := range []string{"expand", "migrate", "manual", "contract"} {
		name := path.Join("phases", phase+".sql")
		body, exists := files[name]
		if !exists {
			continue
		}
		sql.WriteString("\n-- onwardpg proposed phase: " + phase + "\n")
		sql.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			sql.WriteByte('\n')
		}
	}
	return []byte(sql.String()), nil
}

func (m Manifest) Validate() error {
	if m.ProtocolVersion != Version {
		return fmt.Errorf("bundle protocol_version is %q, want %q", m.ProtocolVersion, Version)
	}
	if !safeName(m.BundleID) || !safeName(m.Target) {
		return fmt.Errorf("bundle_id and target must contain only letters, numbers, dot, underscore, or dash")
	}
	if m.Generation < 1 {
		return fmt.Errorf("bundle generation must be positive")
	}
	if m.Purpose != "feature" && m.Purpose != "repair" && m.Purpose != "contract" {
		return fmt.Errorf("bundle purpose %q is invalid", m.Purpose)
	}
	if m.Mode != "pr" && m.Mode != "release" && m.Mode != "verify" && m.Mode != "develop" {
		return fmt.Errorf("bundle mode %q is invalid", m.Mode)
	}
	if m.State != string(protocol.Planned) && m.State != string(protocol.NeedsInput) && m.State != string(protocol.Unsupported) {
		return fmt.Errorf("bundle state %q is invalid", m.State)
	}
	if m.Mode == "pr" {
		if m.BaseRef == "" || !commitPattern.MatchString(m.BaseCommit) || m.HeadRevision == "" {
			return fmt.Errorf("pr bundle requires base_ref, full base_commit, and head_revision")
		}
	}
	if err := m.BaselineSource.Validate(); err != nil {
		return fmt.Errorf("baseline source: %w", err)
	}
	if err := m.DesiredSource.Validate(); err != nil {
		return fmt.Errorf("desired source: %w", err)
	}
	if strings.TrimSpace(m.Planner.Version) == "" {
		return fmt.Errorf("planner version is required")
	}
	previousIgnore := ""
	for _, selector := range m.Planner.IgnoreSelectors {
		if strings.TrimSpace(selector) == "" || strings.ContainsRune(selector, '\x00') || (previousIgnore != "" && selector <= previousIgnore) {
			return fmt.Errorf("planner ignore selectors must be non-empty, sorted, and unique")
		}
		previousIgnore = selector
	}
	if m.SchemaSquare != nil {
		if err := m.SchemaSquare.Validate(m.BaselineSource.Fingerprint, m.DesiredSource.Fingerprint); err != nil {
			return fmt.Errorf("schema_square: %w", err)
		}
	}
	if m.History != nil {
		if !fingerprintPattern.MatchString(m.History.ParentDigest) || !fingerprintPattern.MatchString(m.History.EntryDigest) {
			return fmt.Errorf("history parent and entry digests must be SHA-256 fingerprints")
		}
		expected, err := HistoryEntryDigest(m)
		if err != nil {
			return fmt.Errorf("history: %w", err)
		}
		if m.History.EntryDigest != expected {
			return fmt.Errorf("history entry digest does not match manifest receipts")
		}
		if m.History.EntryDigest == m.History.ParentDigest {
			return fmt.Errorf("history entry cannot be its own parent")
		}
	}
	if !fingerprintPattern.MatchString(m.ResultDigest) {
		return fmt.Errorf("result digest %q is invalid", m.ResultDigest)
	}
	for name, digest := range map[string]string{
		"result": m.ResultDigest, "plan": m.PlanDigest,
		"answers": m.AnswersDigest, "intent": m.IntentDigest,
	} {
		if digest != "" && !fingerprintPattern.MatchString(digest) {
			return fmt.Errorf("%s digest %q is invalid", name, digest)
		}
	}
	if m.State == string(protocol.Planned) && m.PlanDigest == "" {
		return fmt.Errorf("planned bundle requires plan_digest")
	}
	seenDecisions := make(map[string]bool, len(m.Decisions))
	for _, decision := range m.Decisions {
		if !validDecisionPath(decision.Path) || !fingerprintPattern.MatchString(decision.Digest) || seenDecisions[decision.Path] {
			return fmt.Errorf("invalid or duplicate decision receipt %q", decision.Path)
		}
		seenDecisions[decision.Path] = true
	}
	if (m.State == string(protocol.NeedsInput) || m.State == string(protocol.Unsupported)) && !decisionDigestExists(m.Decisions, m.ResultDigest) {
		return fmt.Errorf("%s bundle requires a decision receipt matching result_digest", m.State)
	}
	for phase, artifact := range m.Phases {
		if phase != "expand" && phase != "migrate" && phase != "manual" && phase != "contract" {
			return fmt.Errorf("unknown phase artifact %q", phase)
		}
		if artifact.Path != path.Join("phases", phase+".sql") || !fingerprintPattern.MatchString(artifact.Digest) {
			return fmt.Errorf("phase %q has invalid path or digest", phase)
		}
	}
	if m.Lineage != nil {
		if m.Lineage.Relationship != "supersedes" && m.Lineage.Relationship != "continues" && m.Lineage.Relationship != "repairs" {
			return fmt.Errorf("lineage relationship %q is invalid", m.Lineage.Relationship)
		}
		if !safeName(m.Lineage.BundleID) || m.Lineage.Generation < 1 {
			return fmt.Errorf("lineage requires a valid bundle_id and positive generation")
		}
		if m.Lineage.BundleID == m.BundleID && m.Lineage.Generation >= m.Generation {
			return fmt.Errorf("bundle lineage cannot point to the same or a future generation")
		}
	}
	return nil
}

func (s SchemaSquareReceipt) Validate(baselineFingerprint, desiredFingerprint string) error {
	for name, fingerprint := range map[string]string{
		"base_code": s.BaseCodeFingerprint, "base_history": s.BaseHistoryFingerprint,
		"head_code": s.HeadCodeFingerprint,
	} {
		if !fingerprintPattern.MatchString(fingerprint) {
			return fmt.Errorf("%s fingerprint %q is invalid", name, fingerprint)
		}
	}
	if s.HeadHistoryFingerprint != "" && !fingerprintPattern.MatchString(s.HeadHistoryFingerprint) {
		return fmt.Errorf("head history fingerprint %q is invalid", s.HeadHistoryFingerprint)
	}
	if s.BaseHistoryDigest != "" && !fingerprintPattern.MatchString(s.BaseHistoryDigest) {
		return fmt.Errorf("base history digest %q is invalid", s.BaseHistoryDigest)
	}
	if s.BaseCodeFingerprint != baselineFingerprint || s.HeadCodeFingerprint != desiredFingerprint {
		return fmt.Errorf("schema square does not match planner source receipts")
	}
	if s.BaseIntegrity != "matched" || s.BaseCodeFingerprint != s.BaseHistoryFingerprint {
		return fmt.Errorf("bundle requires matched base code and migration history")
	}
	switch s.HeadHistoryFidelity {
	case "not_replayed":
		if s.HeadHistoryFingerprint != "" {
			return fmt.Errorf("not_replayed head history must not have a fingerprint")
		}
	case "matched":
		if s.HeadHistoryFingerprint == "" || s.HeadHistoryFingerprint != s.HeadCodeFingerprint {
			return fmt.Errorf("matched head history must equal head code")
		}
	case "mismatched", "deferred":
	default:
		return fmt.Errorf("head history fidelity %q is invalid", s.HeadHistoryFidelity)
	}
	return nil
}

func (s SourceReceipt) Validate() error {
	if s.Kind != "database" && s.Kind != "ddl" && s.Kind != "ddl_export" && s.Kind != "adapter" && s.Kind != "git_migrations" && s.Kind != "onwardpg_history" && s.Kind != "typed_snapshot" {
		return fmt.Errorf("kind %q is invalid", s.Kind)
	}
	if strings.TrimSpace(s.Description) == "" || strings.ContainsRune(s.Description, '\x00') || strings.Contains(s.Description, "://") || secretDSNPattern.MatchString(s.Description) {
		return fmt.Errorf("description is required and must not contain a connection URL or libpq secret")
	}
	if !fingerprintPattern.MatchString(s.Fingerprint) {
		return fmt.Errorf("fingerprint %q is invalid", s.Fingerprint)
	}
	if s.GitCommit != "" && !commitPattern.MatchString(s.GitCommit) {
		return fmt.Errorf("git_commit must be a full lowercase Git object ID")
	}
	if s.PostgresMajor != 0 && (s.PostgresMajor < 14 || s.PostgresMajor > 18) {
		return fmt.Errorf("postgres_major must be between 14 and 18")
	}
	return nil
}

func safeName(value string) bool {
	if value == "" || strings.Trim(value, ".") == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validDecisionPath(value string) bool {
	if !strings.HasPrefix(value, "decisions/attempt-") || !strings.HasSuffix(value, ".json") {
		return false
	}
	number := strings.TrimSuffix(strings.TrimPrefix(value, "decisions/attempt-"), ".json")
	return len(number) == 3 && number[0] >= '0' && number[0] <= '9' && number[1] >= '0' && number[1] <= '9' && number[2] >= '0' && number[2] <= '9'
}

func decisionDigestExists(decisions []FileReceipt, digest string) bool {
	for _, decision := range decisions {
		if decision.Digest == digest {
			return true
		}
	}
	return false
}

func SortedFiles(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
