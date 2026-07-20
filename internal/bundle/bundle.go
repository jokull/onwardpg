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

const Version = "onwardpg.bundle/v3"

const CatalogCheckpointVersion = "onwardpg.catalog-checkpoint/v1"

var rootHistoryDigest = digestFrames([]byte("onwardpg.history/v1"), []byte("root"))

func HistoryRootDigest() string { return rootHistoryDigest }

var (
	fingerprintPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	secretDSNPattern   = regexp.MustCompile(`(?i)(?:^|[\s;])(?:password|passfile|sslkey)\s*=`)
	gateIDPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]*$`)
)

type SourceReceipt struct {
	Kind          string `json:"kind"`
	Description   string `json:"description"`
	Fingerprint   string `json:"fingerprint"`
	PostgresMajor int    `json:"postgres_major,omitempty"`
}

type PlannerOptions struct {
	ConcurrentIndexes       bool     `json:"concurrent_indexes,omitempty"`
	IfNotExists             bool     `json:"if_not_exists,omitempty"`
	IfExists                bool     `json:"if_exists,omitempty"`
	CascadeDrops            bool     `json:"cascade_drops,omitempty"`
	SchemaQualifier         *string  `json:"schema_qualifier,omitempty"`
	IgnoreExtensionVersions []string `json:"ignore_extension_versions,omitempty"`
}

type BuildIdentity struct {
	Version                 string `json:"version"`
	Commit                  string `json:"commit"`
	Dirty                   bool   `json:"dirty"`
	BuildTime               string `json:"build_time,omitempty"`
	GoVersion               string `json:"go_version"`
	SupportedPostgresMajors []int  `json:"supported_postgres_majors"`
}

type PlannerReceipt struct {
	Version         string         `json:"version"`
	Build           *BuildIdentity `json:"build,omitempty"`
	Options         PlannerOptions `json:"options"`
	IgnoreSelectors []string       `json:"ignore_selectors,omitempty"`
}

// HistoryReceipt links a bundle to the exact protected history head it was
// planned from. EntryDigest commits to the complete manifest receipt (with the
// entry digest itself cleared), so directory names and filesystem ordering are
// never part of migration order.
type HistoryReceipt struct {
	ParentDigest string `json:"parent_digest"`
	EntryDigest  string `json:"entry_digest"`
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

// CatalogCheckpoint receipts the exact graph observed after accepted history
// plus this bundle's expand phase were executed in disposable PostgreSQL.
// Production comparison is read-only and uses this as the overlap authority.
type CatalogCheckpoint struct {
	ProtocolVersion     string `json:"protocol_version"`
	BundleID            string `json:"bundle_id"`
	PlanID              string `json:"plan_id"`
	Generation          int    `json:"generation"`
	BaselineFingerprint string `json:"baseline_fingerprint"`
	ExpandFingerprint   string `json:"expand_fingerprint"`
	DesiredFingerprint  string `json:"desired_fingerprint"`
}

type Manifest struct {
	ProtocolVersion string `json:"protocol_version"`
	BundleID        string `json:"bundle_id"`
	PlanID          string `json:"plan_id,omitempty"`
	Generation      int    `json:"generation"`
	Target          string `json:"target"`
	Purpose         string `json:"purpose"`
	State           string `json:"state"`

	BaselineSource SourceReceipt   `json:"baseline_source"`
	DesiredSource  SourceReceipt   `json:"desired_source"`
	Planner        PlannerReceipt  `json:"planner"`
	History        *HistoryReceipt `json:"history,omitempty"`

	ResultDigest                string                   `json:"result_digest"`
	PlanDigest                  string                   `json:"plan_digest,omitempty"`
	Decisions                   []FileReceipt            `json:"decisions,omitempty"`
	AnswersDigest               string                   `json:"answers_digest,omitempty"`
	QuestionsDigest             string                   `json:"questions_digest,omitempty"`
	SemanticDigest              string                   `json:"semantic_decisions_digest,omitempty"`
	ContractGatesDigest         string                   `json:"contract_gates_digest,omitempty"`
	ContractGateOverridesDigest string                   `json:"contract_gate_overrides_digest,omitempty"`
	ExpandCheckpointDigest      string                   `json:"expand_checkpoint_digest,omitempty"`
	Operations                  []FileReceipt            `json:"operations,omitempty"`
	PhaseSource                 string                   `json:"phase_source,omitempty"`
	VerificationDigest          string                   `json:"verification_digest,omitempty"`
	Phases                      map[string]PhaseArtifact `json:"phases,omitempty"`
}

type Metadata struct {
	BundleID            string
	PlanID              string
	Generation          int
	Target              string
	Purpose             string
	BaselineSource      SourceReceipt
	DesiredSource       SourceReceipt
	Planner             PlannerReceipt
	HistoryParentDigest string
}

type Input struct {
	Metadata  Metadata
	Result    protocol.Result
	Answers   *protocol.Answers
	Hints     []protocol.Hint
	Questions []protocol.Question
	Attempt   int
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
	case string(protocol.Planned), string(protocol.NeedsSQLEdits):
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
		if len(result.Operations) != len(a.Manifest.Operations) {
			return fmt.Errorf("operation receipts do not match plan.json")
		}
		operationReceipts := make(map[string]FileReceipt, len(a.Manifest.Operations))
		for _, receipt := range a.Manifest.Operations {
			if _, duplicate := operationReceipts[receipt.Path]; duplicate {
				return fmt.Errorf("operation receipt path %q is duplicated", receipt.Path)
			}
			operationReceipts[receipt.Path] = receipt
			required[receipt.Path] = receipt.Digest
		}
		for _, operation := range result.Operations {
			operationPath := path.Join("operations", operation.ID+".json")
			receipt, exists := operationReceipts[operationPath]
			if !exists {
				return fmt.Errorf("operation %q has no receipt", operation.ID)
			}
			body, err := jsonDocument(operation)
			if err != nil || receipt.Digest != Digest(body) || string(a.Files[operationPath]) != string(body) {
				return fmt.Errorf("operation artifact %s does not match plan.json", operationPath)
			}
		}
		if len(result.ContractGates) > 0 {
			required["contract-gates.json"] = a.Manifest.ContractGatesDigest
			gateBytes, err := jsonDocument(result.ContractGates)
			if err != nil {
				return fmt.Errorf("encode contract gates: %w", err)
			}
			if a.Manifest.ContractGatesDigest == "" || string(a.Files["contract-gates.json"]) != string(gateBytes) {
				return fmt.Errorf("contract gate receipt does not match plan.json")
			}
		} else if a.Manifest.ContractGatesDigest != "" {
			return fmt.Errorf("manifest receipts contract gates absent from plan.json")
		}
		if a.Manifest.ContractGateOverridesDigest != "" {
			required["contract-gate-overrides.json"] = a.Manifest.ContractGateOverridesDigest
			if a.Manifest.PhaseSource != "edited" {
				return fmt.Errorf("only edited phase SQL may override placeholder contract gates")
			}
		} else if _, exists := a.Files["contract-gate-overrides.json"]; exists {
			return fmt.Errorf("bundle contains unreceipted contract gate overrides")
		}
		if _, err := effectiveContractGates(a, result); err != nil {
			return err
		}
		switch a.Manifest.PhaseSource {
		case "", "generated":
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
		case "edited":
			if err := validateEditedPhaseReceipts(a.Manifest.Phases); err != nil {
				return err
			}
		default:
			return fmt.Errorf("phase_source %q is invalid", a.Manifest.PhaseSource)
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
	var answers *protocol.Answers
	if a.Manifest.AnswersDigest != "" {
		required["answers.json"] = a.Manifest.AnswersDigest
		var decoded protocol.Answers
		if err := json.Unmarshal(a.Files["answers.json"], &decoded); err != nil {
			return fmt.Errorf("decode answers.json: %w", err)
		}
		if decoded.ProtocolVersion != protocol.Version || decoded.CurrentFingerprint != a.Manifest.BaselineSource.Fingerprint || decoded.DesiredFingerprint != a.Manifest.DesiredSource.Fingerprint {
			return fmt.Errorf("answers.json does not match the bundle fingerprints")
		}
		answers = &decoded
	}
	if a.Manifest.QuestionsDigest != "" {
		required["questions.json"] = a.Manifest.QuestionsDigest
		var questions []protocol.Question
		if err := json.Unmarshal(a.Files["questions.json"], &questions); err != nil {
			return fmt.Errorf("decode questions.json: %w", err)
		}
		if err := validateQuestions(questions, a.Manifest.BaselineSource.Fingerprint, a.Manifest.DesiredSource.Fingerprint); err != nil {
			return fmt.Errorf("validate questions.json: %w", err)
		}
		if answers != nil {
			if err := validateAnswerQuestions(*answers, questions); err != nil {
				return fmt.Errorf("validate answer questions: %w", err)
			}
		}
	}
	if a.Manifest.SemanticDigest != "" {
		required["decisions.json"] = a.Manifest.SemanticDigest
		var receipt protocol.DecisionReceipt
		if err := json.Unmarshal(a.Files["decisions.json"], &receipt); err != nil {
			return fmt.Errorf("decode decisions.json: %w", err)
		}
		if receipt.Protocol != protocol.DecisionsVersion {
			return fmt.Errorf("decisions.json protocol is %q, want %q", receipt.Protocol, protocol.DecisionsVersion)
		}
		canonical, err := protocol.CanonicalHints(receipt.Hints)
		if err != nil {
			return fmt.Errorf("validate decisions.json hints: %w", err)
		}
		if !reflect.DeepEqual(canonical, receipt.Hints) {
			return fmt.Errorf("decisions.json hints are not in canonical order")
		}
		if answers == nil || !reflect.DeepEqual(*answers, receipt.Answers) {
			return fmt.Errorf("decisions.json answers do not match answers.json")
		}
	}
	if a.Manifest.VerificationDigest != "" {
		required["verify.sql"] = a.Manifest.VerificationDigest
	}
	if a.Manifest.ExpandCheckpointDigest != "" {
		required["expand-checkpoint.json"] = a.Manifest.ExpandCheckpointDigest
		checkpoint, err := ReadCatalogCheckpoint(a)
		if err != nil {
			return err
		}
		if checkpoint.BundleID != a.Manifest.BundleID || checkpoint.PlanID != a.Manifest.PlanID || checkpoint.Generation != a.Manifest.Generation ||
			checkpoint.BaselineFingerprint != a.Manifest.BaselineSource.Fingerprint || checkpoint.DesiredFingerprint != a.Manifest.DesiredSource.Fingerprint {
			return fmt.Errorf("expand checkpoint does not match bundle identity and schema receipts")
		}
	} else if _, exists := a.Files["expand-checkpoint.json"]; exists {
		return fmt.Errorf("bundle contains an unreceipted expand checkpoint")
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

func validateEditedPhaseReceipts(phases map[string]PhaseArtifact) error {
	for phase, artifact := range phases {
		if !protocol.ValidPhase(phase) {
			return fmt.Errorf("edited phase %q is invalid", phase)
		}
		if artifact.Path != path.Join("phases", phase+".sql") {
			return fmt.Errorf("edited phase %q has invalid path %q", phase, artifact.Path)
		}
		if !fingerprintPattern.MatchString(artifact.Digest) {
			return fmt.Errorf("edited phase %q has invalid digest", phase)
		}
	}
	return nil
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
	if input.Result.Status != protocol.Planned && input.Result.Status != protocol.NeedsSQLEdits && input.Result.Status != protocol.NeedsInput && input.Result.Status != protocol.Unsupported {
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
		BundleID:        input.Metadata.BundleID, PlanID: input.Metadata.PlanID, Generation: input.Metadata.Generation,
		Target: input.Metadata.Target, Purpose: input.Metadata.Purpose,
		State:          string(input.Result.Status),
		BaselineSource: input.Metadata.BaselineSource, DesiredSource: input.Metadata.DesiredSource,
		Planner: input.Metadata.Planner, ResultDigest: Digest(resultBytes),
	}
	files := make(map[string][]byte)
	switch input.Result.Status {
	case protocol.NeedsInput, protocol.Unsupported:
		name := fmt.Sprintf("decisions/attempt-%03d.json", input.Attempt)
		files[name] = resultBytes
		manifest.Decisions = []FileReceipt{{Path: name, Digest: Digest(resultBytes)}}
	case protocol.Planned, protocol.NeedsSQLEdits:
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
		manifest.PhaseSource = "generated"
		for name, data := range phaseFiles {
			files[name] = data
		}
		if len(input.Result.ContractGates) > 0 {
			gateBytes, err := jsonDocument(input.Result.ContractGates)
			if err != nil {
				return Artifact{}, fmt.Errorf("encode contract gates: %w", err)
			}
			files["contract-gates.json"] = gateBytes
			manifest.ContractGatesDigest = Digest(gateBytes)
		}
		for _, operation := range input.Result.Operations {
			body, err := jsonDocument(operation)
			if err != nil {
				return Artifact{}, fmt.Errorf("encode operation %q: %w", operation.ID, err)
			}
			name := path.Join("operations", operation.ID+".json")
			files[name] = body
			manifest.Operations = append(manifest.Operations, FileReceipt{Path: name, Digest: Digest(body)})
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
	if len(input.Hints) > 0 {
		if input.Answers == nil {
			return Artifact{}, fmt.Errorf("semantic decisions require an internal answer receipt")
		}
		hints, err := protocol.CanonicalHints(input.Hints)
		if err != nil {
			return Artifact{}, fmt.Errorf("validate semantic decisions: %w", err)
		}
		receiptBytes, err := jsonDocument(protocol.DecisionReceipt{
			Protocol: protocol.DecisionsVersion, Hints: hints, Answers: *input.Answers,
		})
		if err != nil {
			return Artifact{}, fmt.Errorf("encode semantic decisions: %w", err)
		}
		files["decisions.json"] = receiptBytes
		manifest.SemanticDigest = Digest(receiptBytes)
	}
	if len(input.Questions) > 0 {
		if err := validateQuestions(input.Questions, input.Result.CurrentFingerprint, input.Result.DesiredFingerprint); err != nil {
			return Artifact{}, err
		}
		questionBytes, err := jsonDocument(input.Questions)
		if err != nil {
			return Artifact{}, fmt.Errorf("encode questions: %w", err)
		}
		files["questions.json"] = questionBytes
		manifest.QuestionsDigest = Digest(questionBytes)
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
	if artifact.Manifest.QuestionsDigest != "" {
		var questions []protocol.Question
		if err := json.Unmarshal(artifact.Files["questions.json"], &questions); err != nil {
			return nil, fmt.Errorf("decode questions.json: %w", err)
		}
		if err := validateQuestions(questions, artifact.Manifest.BaselineSource.Fingerprint, artifact.Manifest.DesiredSource.Fingerprint); err != nil {
			return nil, err
		}
		return questions, nil
	}
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

func validateQuestions(questions []protocol.Question, currentFingerprint, desiredFingerprint string) error {
	seen := make(map[string]bool, len(questions))
	for _, question := range questions {
		id := question.Kind + ":" + question.Key
		if question.Kind == "" || question.Key == "" || question.ScopeFingerprint == "" {
			return fmt.Errorf("question kind, key, and scope fingerprint are required")
		}
		if seen[id] {
			return fmt.Errorf("question %s is duplicated", id)
		}
		if question.CurrentFingerprint != currentFingerprint || question.DesiredFingerprint != desiredFingerprint {
			return fmt.Errorf("question %s fingerprints do not match the bundle", id)
		}
		seen[id] = true
	}
	return nil
}

func validateAnswerQuestions(answers protocol.Answers, questions []protocol.Question) error {
	byID := make(map[string]protocol.Question, len(questions))
	for _, question := range questions {
		byID[question.Kind+":"+question.Key] = question
	}
	for _, answer := range answers.Answers {
		id := answer.Kind + ":" + answer.Key
		question, exists := byID[id]
		if !exists {
			return fmt.Errorf("answer %s has no canonical question", id)
		}
		if answer.QuestionFingerprint != "" && answer.QuestionFingerprint != question.ScopeFingerprint {
			return fmt.Errorf("answer %s has a stale question fingerprint", id)
		}
	}
	return nil
}

func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func validatePlanStatements(result protocol.Result) error {
	if err := validateContractGates(result); err != nil {
		return err
	}
	if err := validateOperations(result); err != nil {
		return err
	}
	ids := make(map[string]protocol.Statement, len(result.Statements))
	for _, statement := range result.Statements {
		if !protocol.ValidPhase(statement.Phase) {
			return fmt.Errorf("planned statement %q uses unsupported phase %q; regenerate the bundle with this onwardpg version", statement.ID, statement.Phase)
		}
		if statement.ID == "" {
			return fmt.Errorf("planned statement is missing a stable id")
		}
		if statement.ContractDisposition != "" && statement.ContractDisposition != "write_fence" && statement.ContractDisposition != "gated_restoration" && statement.ContractDisposition != "catalog_proven_invariant" && statement.ContractDisposition != "catalog_proven_index" && statement.ContractDisposition != "catalog_derived_relation" && statement.ContractDisposition != "postgres_atomic_validation" && statement.ContractDisposition != "operator_owned_manual" {
			return fmt.Errorf("planned statement %q has invalid contract disposition %q", statement.ID, statement.ContractDisposition)
		}
		if bundleRestoresEnforcement(statement) && statement.ContractDisposition == "" {
			return fmt.Errorf("contract enforcement statement %q has no typed gate disposition", statement.ID)
		}
		if _, exists := ids[statement.ID]; exists {
			return fmt.Errorf("planned statement id %q is duplicated", statement.ID)
		}
		ids[statement.ID] = statement
	}
	seen := make(map[string]bool, len(ids))
	for _, batch := range result.Batches {
		if !protocol.ValidPhase(batch.Phase) {
			return fmt.Errorf("planned batch %q uses unsupported phase %q; regenerate the bundle with this onwardpg version", batch.ID, batch.Phase)
		}
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

func validateOperations(result protocol.Result) error {
	gates := make(map[string]protocol.ContractGate, len(result.ContractGates))
	for _, gate := range result.ContractGates {
		gates[gate.ID] = gate
	}
	seen := make(map[string]bool, len(result.Operations))
	for _, operation := range result.Operations {
		if !gateIDPattern.MatchString(operation.ID) || seen[operation.ID] {
			return fmt.Errorf("operation id %q is invalid or duplicated", operation.ID)
		}
		seen[operation.ID] = true
		if strings.TrimSpace(operation.TransitionID) == "" || strings.TrimSpace(operation.Summary) == "" {
			return fmt.Errorf("operation %q requires transition_id and summary", operation.ID)
		}
		if operation.Timing != "after_expand_before_contract" && operation.Timing != "post_drain_before_contract_enforcement" {
			return fmt.Errorf("operation %q has invalid timing %q", operation.ID, operation.Timing)
		}
		switch operation.ExecutionMode {
		case protocol.ManualOperatorBatched:
			if len(operation.BatchTemplate) == 0 || strings.TrimSpace(operation.ProgressKey) == "" || strings.TrimSpace(operation.CompletionSQL) == "" || strings.TrimSpace(operation.IdempotencyNotes) == "" || len(operation.RequiredEvidence) > 0 || len(operation.CompletionGateIDs) != 1 {
				return fmt.Errorf("operator_batched operation %q is incomplete", operation.ID)
			}
		case protocol.ManualExternalAttestation:
			if len(operation.BatchTemplate) > 0 || operation.ProgressKey != "" || operation.CompletionSQL != "" || operation.IdempotencyNotes != "" || len(operation.RequiredEvidence) == 0 || len(operation.CompletionGateIDs) != 1 {
				return fmt.Errorf("external_attestation operation %q is invalid", operation.ID)
			}
		default:
			return fmt.Errorf("operation %q has invalid execution mode %q", operation.ID, operation.ExecutionMode)
		}
		for _, gateID := range operation.CompletionGateIDs {
			gate, exists := gates[gateID]
			if !exists {
				return fmt.Errorf("operation %q references missing gate %q", operation.ID, gateID)
			}
			if operation.ExecutionMode == protocol.ManualOperatorBatched && (gate.Kind != "manual_reconciliation" || strings.TrimSpace(gate.BooleanSQL) != strings.TrimSpace(operation.CompletionSQL)) {
				return fmt.Errorf("operator_batched operation %q completion SQL does not match its manual reconciliation gate", operation.ID)
			}
			if operation.ExecutionMode == protocol.ManualExternalAttestation && (gate.Kind != "operation_attestation" || !reflect.DeepEqual(gate.RequiredEvidence, operation.RequiredEvidence)) {
				return fmt.Errorf("external_attestation operation %q evidence does not match its attestation gate", operation.ID)
			}
		}
	}
	return nil
}

func bundleRestoresEnforcement(statement protocol.Statement) bool {
	if statement.Phase != protocol.PhaseContract {
		return false
	}
	upper := strings.ToUpper(strings.TrimSpace(statement.SQL))
	if strings.Contains(upper, "ADD CONSTRAINT") && strings.Contains(upper, "NOT VALID") {
		return false
	}
	if strings.Contains(upper, "VALIDATE CONSTRAINT") || strings.Contains(upper, " SET NOT NULL") || strings.HasPrefix(upper, "CREATE UNIQUE INDEX") {
		return true
	}
	return strings.Contains(upper, "ADD CONSTRAINT") && (strings.Contains(upper, " UNIQUE ") || strings.Contains(upper, " PRIMARY KEY ") || strings.Contains(upper, " EXCLUDE ") || strings.Contains(upper, " CHECK ") || strings.Contains(upper, " FOREIGN KEY "))
}

func validateContractGates(result protocol.Result) error {
	gates := make(map[string]protocol.ContractGate, len(result.ContractGates))
	for _, gate := range result.ContractGates {
		if !gateIDPattern.MatchString(gate.ID) || gates[gate.ID].ID != "" {
			return fmt.Errorf("contract gate id %q is invalid or duplicated", gate.ID)
		}
		if !fingerprintPattern.MatchString(gate.ScopeFingerprint) {
			return fmt.Errorf("contract gate %q has invalid scope fingerprint", gate.ID)
		}
		if strings.TrimSpace(gate.Reason) == "" || strings.ContainsAny(gate.Reason, "\r\n") {
			return fmt.Errorf("contract gate %q requires a one-line reason", gate.ID)
		}
		switch gate.Kind {
		case "catalog_checkpoint", "data_assertion", "manual_reconciliation":
			if strings.TrimSpace(gate.BooleanSQL) == "" || len(gate.RequiredEvidence) != 0 {
				return fmt.Errorf("contract gate %q requires boolean_sql and no external evidence", gate.ID)
			}
			upper := strings.ToUpper(strings.TrimSpace(gate.BooleanSQL))
			if !strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "WITH ") {
				return fmt.Errorf("contract gate %q boolean_sql must be a read-only SELECT", gate.ID)
			}
		case "writer_attestation", "operation_attestation":
			if strings.TrimSpace(gate.BooleanSQL) != "" || len(gate.RequiredEvidence) == 0 {
				return fmt.Errorf("writer attestation gate %q requires external evidence and no boolean_sql", gate.ID)
			}
			previous := ""
			for _, category := range gate.RequiredEvidence {
				if strings.TrimSpace(category) == "" || previous != "" && category <= previous {
					return fmt.Errorf("writer attestation gate %q evidence categories must be sorted and unique", gate.ID)
				}
				previous = category
			}
		default:
			return fmt.Errorf("contract gate %q has invalid kind %q", gate.ID, gate.Kind)
		}
		gates[gate.ID] = gate
	}
	referenced := make(map[string]bool, len(gates))
	for _, statement := range result.Statements {
		seen := make(map[string]bool, len(statement.RequiresGates))
		for _, id := range statement.RequiresGates {
			if _, exists := gates[id]; !exists || seen[id] {
				return fmt.Errorf("statement %q references missing or duplicate contract gate %q", statement.ID, id)
			}
			if statement.Phase != protocol.PhaseContract {
				return fmt.Errorf("expand statement %q cannot require contract gate %q", statement.ID, id)
			}
			seen[id], referenced[id] = true, true
		}
	}
	seenTransitions := make(map[string]bool, len(result.Reconciliations))
	for _, reconciliation := range result.Reconciliations {
		if strings.TrimSpace(reconciliation.TransitionID) == "" || seenTransitions[reconciliation.TransitionID] {
			return fmt.Errorf("reconciliation transition id %q is empty or duplicated", reconciliation.TransitionID)
		}
		seenTransitions[reconciliation.TransitionID] = true
		switch reconciliation.Strategy {
		case "assert_only":
			if reconciliation.Work != nil {
				return fmt.Errorf("assert-only reconciliation %q cannot include manual work", reconciliation.TransitionID)
			}
		case "manual_sql", "generated_sql":
			if reconciliation.Work == nil || len(reconciliation.Work.Statements) == 0 || len(reconciliation.Work.VerificationSQL) == 0 {
				return fmt.Errorf("manual reconciliation %q requires statements and boolean verification", reconciliation.TransitionID)
			}
		default:
			return fmt.Errorf("planned reconciliation %q has invalid strategy %q", reconciliation.TransitionID, reconciliation.Strategy)
		}
		if len(reconciliation.GateIDs) == 0 {
			return fmt.Errorf("reconciliation %q has no contract gates", reconciliation.TransitionID)
		}
		for _, id := range reconciliation.GateIDs {
			if _, exists := gates[id]; !exists {
				return fmt.Errorf("reconciliation %q references missing gate %q", reconciliation.TransitionID, id)
			}
			referenced[id] = true
		}
	}
	for _, operation := range result.Operations {
		for _, id := range operation.CompletionGateIDs {
			if _, exists := gates[id]; !exists {
				return fmt.Errorf("operation %q references missing gate %q", operation.ID, id)
			}
			referenced[id] = true
		}
	}
	for id := range gates {
		if !referenced[id] {
			return fmt.Errorf("contract gate %q is not referenced", id)
		}
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
	for _, phase := range []string{protocol.PhaseExpand, protocol.PhaseContract} {
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
// them, in lifecycle order. It is used to prove a draft before its bundle is
// written.
func RenderReplaySQL(result protocol.Result) ([]byte, error) {
	_, files, err := renderPhases(result)
	if err != nil {
		return nil, err
	}
	var sql strings.Builder
	for _, phase := range []string{protocol.PhaseExpand, protocol.PhaseContract} {
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
		return fmt.Errorf("bundle protocol_version %q is unsupported; regenerate this developer-preview history with %s", m.ProtocolVersion, Version)
	}
	if !safeName(m.BundleID) || !safeName(m.Target) {
		return fmt.Errorf("bundle_id and target must contain only letters, numbers, dot, underscore, or dash")
	}
	if m.PlanID != "" && (!strings.HasPrefix(m.PlanID, "plan_") || len(m.PlanID) != len("plan_")+32 || !safeName(m.PlanID)) {
		return fmt.Errorf("plan_id %q is invalid", m.PlanID)
	}
	if m.Generation < 1 {
		return fmt.Errorf("bundle generation must be positive")
	}
	if m.Purpose != "feature" && m.Purpose != "repair" && m.Purpose != "contract" && m.Purpose != "baseline" {
		return fmt.Errorf("bundle purpose %q is invalid", m.Purpose)
	}
	if m.State != string(protocol.Planned) && m.State != string(protocol.NeedsSQLEdits) && m.State != string(protocol.NeedsInput) && m.State != string(protocol.Unsupported) {
		return fmt.Errorf("bundle state %q is invalid", m.State)
	}
	if m.PhaseSource != "" && m.PhaseSource != "generated" && m.PhaseSource != "edited" {
		return fmt.Errorf("phase_source %q is invalid", m.PhaseSource)
	}
	if m.State != string(protocol.Planned) && m.State != string(protocol.NeedsSQLEdits) && (m.PhaseSource != "" || m.VerificationDigest != "" || m.ExpandCheckpointDigest != "" || m.ContractGateOverridesDigest != "" || len(m.Operations) > 0) {
		return fmt.Errorf("only planned or needs_sql_edits bundles may contain phase receipts")
	}
	if m.ContractGateOverridesDigest != "" && (m.State != string(protocol.Planned) || m.PhaseSource != "edited") {
		return fmt.Errorf("contract gate overrides require a planned bundle with edited phase SQL")
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
	if m.Planner.Build != nil {
		identity := m.Planner.Build
		if identity.Version != m.Planner.Version || strings.TrimSpace(identity.Commit) == "" || strings.TrimSpace(identity.GoVersion) == "" || len(identity.SupportedPostgresMajors) == 0 {
			return fmt.Errorf("planner build identity must match planner version and include commit, Go version, and PostgreSQL majors")
		}
		previousMajor := 0
		for _, major := range identity.SupportedPostgresMajors {
			if major <= previousMajor {
				return fmt.Errorf("planner build PostgreSQL majors must be sorted and unique")
			}
			previousMajor = major
		}
	}
	previousIgnore := ""
	for _, selector := range m.Planner.IgnoreSelectors {
		if strings.TrimSpace(selector) == "" || strings.ContainsRune(selector, '\x00') || (previousIgnore != "" && selector <= previousIgnore) {
			return fmt.Errorf("planner ignore selectors must be non-empty, sorted, and unique")
		}
		previousIgnore = selector
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
		"answers": m.AnswersDigest, "questions": m.QuestionsDigest, "semantic_decisions": m.SemanticDigest,
		"verification": m.VerificationDigest, "contract_gates": m.ContractGatesDigest,
		"contract_gate_overrides": m.ContractGateOverridesDigest,
		"expand_checkpoint":       m.ExpandCheckpointDigest,
	} {
		if digest != "" && !fingerprintPattern.MatchString(digest) {
			return fmt.Errorf("%s digest %q is invalid", name, digest)
		}
	}
	if (m.State == string(protocol.Planned) || m.State == string(protocol.NeedsSQLEdits)) && m.PlanDigest == "" {
		return fmt.Errorf("planned or needs_sql_edits bundle requires plan_digest")
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
		if !protocol.ValidPhase(phase) {
			return fmt.Errorf("unknown phase artifact %q", phase)
		}
		if artifact.Path != path.Join("phases", phase+".sql") || !fingerprintPattern.MatchString(artifact.Digest) {
			return fmt.Errorf("phase %q has invalid path or digest", phase)
		}
	}
	seenOperations := make(map[string]bool, len(m.Operations))
	for _, operation := range m.Operations {
		if !strings.HasPrefix(operation.Path, "operations/") || !strings.HasSuffix(operation.Path, ".json") || path.Clean(operation.Path) != operation.Path || !fingerprintPattern.MatchString(operation.Digest) || seenOperations[operation.Path] {
			return fmt.Errorf("invalid or duplicate operation receipt %q", operation.Path)
		}
		seenOperations[operation.Path] = true
	}
	return nil
}

// SemanticHints reads the compact product intent receipted by a bundle. The
// expanded fingerprint-bound answers remain internal implementation evidence.
func SemanticHints(artifact Artifact) ([]protocol.Hint, error) {
	if artifact.Manifest.SemanticDigest == "" {
		return nil, nil
	}
	var receipt protocol.DecisionReceipt
	if err := json.Unmarshal(artifact.Files["decisions.json"], &receipt); err != nil {
		return nil, fmt.Errorf("decode decisions.json: %w", err)
	}
	if receipt.Protocol != protocol.DecisionsVersion {
		return nil, fmt.Errorf("decisions.json protocol is %q, want %q", receipt.Protocol, protocol.DecisionsVersion)
	}
	return protocol.CanonicalHints(receipt.Hints)
}

func (s SourceReceipt) Validate() error {
	if s.Kind != "database" && s.Kind != "ddl" && s.Kind != "ddl_export" && s.Kind != "onwardpg_history" {
		return fmt.Errorf("kind %q is invalid", s.Kind)
	}
	if strings.TrimSpace(s.Description) == "" || strings.ContainsRune(s.Description, '\x00') || strings.Contains(s.Description, "://") || secretDSNPattern.MatchString(s.Description) {
		return fmt.Errorf("description is required and must not contain a connection URL or libpq secret")
	}
	if !fingerprintPattern.MatchString(s.Fingerprint) {
		return fmt.Errorf("fingerprint %q is invalid", s.Fingerprint)
	}
	if s.PostgresMajor != 0 && (s.PostgresMajor < 15 || s.PostgresMajor > 18) {
		return fmt.Errorf("postgres_major must be between 15 and 18")
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
