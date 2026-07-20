// Package contractcheck proves contract preconditions without providing any
// path that can execute migration SQL against the caller's database.
package contractcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

const (
	EvidenceVersion = "onwardpg.writer-evidence/v1"
	ReportVersion   = "onwardpg.contract-readiness/v1"
)

type Evidence struct {
	ProtocolVersion    string   `json:"protocol_version"`
	Target             string   `json:"target"`
	Environment        string   `json:"environment"`
	PlanID             string   `json:"plan_id"`
	BundleEntryDigest  string   `json:"bundle_entry_digest"`
	DesiredFingerprint string   `json:"desired_fingerprint"`
	Generation         int      `json:"generation"`
	Release            string   `json:"release"`
	ObservedAt         string   `json:"observed_at"`
	ExpiresAt          string   `json:"expires_at"`
	Cohorts            []Cohort `json:"cohorts"`
}

type Cohort struct {
	Category   string `json:"category"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	SourceKind string `json:"source_kind"`
	Source     string `json:"source"`
}

type GateResult struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	ProtocolVersion     string       `json:"protocol_version"`
	Status              string       `json:"status"`
	Target              string       `json:"target"`
	Environment         string       `json:"environment"`
	BundleID            string       `json:"bundle_id"`
	PlanID              string       `json:"plan_id"`
	Generation          int          `json:"generation"`
	BundleEntryDigest   string       `json:"bundle_entry_digest"`
	ExpectedFingerprint string       `json:"expected_expand_fingerprint"`
	ActualFingerprint   string       `json:"actual_fingerprint,omitempty"`
	CheckedAt           string       `json:"checked_at"`
	GateResults         []GateResult `json:"gates,omitempty"`
	Findings            []Finding    `json:"findings,omitempty"`
	Digest              string       `json:"digest"`
}

type Input struct {
	Artifact         bundle.Artifact
	ExpectedHead     string
	DatabaseURL      string
	Environment      string
	Evidence         []byte
	Now              time.Time
	StatementTimeout time.Duration
	Ignores          []string
	Options          graphplan.Options
}

func Run(ctx context.Context, input Input) (Report, error) {
	if err := input.Artifact.Validate(); err != nil {
		return Report{}, fmt.Errorf("validate bundle: %w", err)
	}
	manifest := input.Artifact.Manifest
	if manifest.History == nil || input.ExpectedHead == "" || manifest.History.EntryDigest != input.ExpectedHead {
		return Report{}, fmt.Errorf("selected bundle is not the expected history chain head")
	}
	checkpoint, err := bundle.ReadCatalogCheckpoint(input.Artifact)
	if err != nil {
		return Report{}, err
	}
	if input.DatabaseURL == "" || strings.TrimSpace(input.Environment) == "" {
		return Report{}, fmt.Errorf("database URL and environment are required")
	}
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.StatementTimeout <= 0 {
		input.StatementTimeout = 30 * time.Second
	}
	report := Report{
		ProtocolVersion: ReportVersion, Status: "blocked", Target: manifest.Target, Environment: input.Environment,
		BundleID: manifest.BundleID, PlanID: manifest.PlanID, Generation: manifest.Generation,
		BundleEntryDigest: manifest.History.EntryDigest, ExpectedFingerprint: checkpoint.ExpandFingerprint,
		CheckedAt: input.Now.UTC().Format(time.RFC3339),
	}

	config, err := pgx.ParseConfig(input.DatabaseURL)
	if err != nil {
		return Report{}, fmt.Errorf("parse production database URL: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return Report{}, fmt.Errorf("connect production database: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, fmt.Errorf("begin read-only readiness snapshot: %w", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", input.StatementTimeout.String()); err != nil {
		return Report{}, fmt.Errorf("set readiness statement timeout: %w", err)
	}
	actual, err := source.InspectGraphTransaction(ctx, tx, input.Ignores, false)
	if err != nil {
		return Report{}, fmt.Errorf("inspect production catalog read-only: %w", err)
	}
	report.ActualFingerprint, err = graphplan.Fingerprint(actual, input.Options)
	if err != nil {
		return Report{}, err
	}
	if report.ActualFingerprint != checkpoint.ExpandFingerprint {
		code, message := "catalog_drift", "production does not match the receipted post-expand catalog"
		switch report.ActualFingerprint {
		case checkpoint.BaselineFingerprint:
			code, message = "expand_not_applied", "production still matches the pre-expand baseline"
		case checkpoint.DesiredFingerprint:
			code, message = "contract_already_applied", "production already matches the desired post-contract catalog"
		}
		report.Findings = append(report.Findings, Finding{Code: code, Message: message, Remediation: "inspect deployment state and catalog drift before running contract"})
		return finalize(report), nil
	}

	gates, err := bundle.ContractGates(input.Artifact)
	if err != nil {
		return Report{}, err
	}
	var writerGateIDs []string
	for _, gate := range gates {
		switch gate.Kind {
		case "data_assertion", "manual_reconciliation":
			passed, checkErr := queryBoolean(ctx, tx, gate.BooleanSQL)
			result := GateResult{ID: gate.ID, Kind: gate.Kind, Passed: passed}
			if checkErr != nil {
				result.Message = checkErr.Error()
				report.GateResults = append(report.GateResults, result)
				report.Findings = append(report.Findings, Finding{Code: "data_gate_error", Message: gate.ID + ": " + checkErr.Error()})
				return finalize(report), nil
			}
			report.GateResults = append(report.GateResults, result)
			if !passed {
				report.Findings = append(report.Findings, Finding{Code: "data_gate_failed", Message: gate.ID + " is false", Remediation: "run the receipted reconciliation after writer drain, then repeat contract check"})
				return finalize(report), nil
			}
		case "writer_attestation":
			writerGateIDs = append(writerGateIDs, gate.ID)
		}
	}
	if len(writerGateIDs) == 0 {
		report.Status = "ready"
		return finalize(report), nil
	}
	if len(bytes.TrimSpace(input.Evidence)) == 0 {
		report.Status = "needs_evidence"
		report.Findings = append(report.Findings, Finding{Code: "writer_evidence_missing", Message: "writer-drain evidence is required"})
		return finalize(report), nil
	}
	evidence, err := DecodeEvidence(input.Evidence)
	if err != nil {
		report.Findings = append(report.Findings, Finding{Code: "writer_evidence_invalid", Message: err.Error()})
		return finalize(report), nil
	}
	status, finding := validateEvidence(evidence, manifest, input.Environment, input.Now, requiredWriterCategories(gates))
	if finding != nil {
		report.Status = status
		report.Findings = append(report.Findings, *finding)
		return finalize(report), nil
	}
	for _, gateID := range writerGateIDs {
		report.GateResults = append(report.GateResults, GateResult{ID: gateID, Kind: "writer_attestation", Passed: true})
	}
	report.Status = "ready"
	return finalize(report), nil
}

func DecodeEvidence(data []byte) (Evidence, error) {
	var evidence Evidence
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return evidence, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return evidence, fmt.Errorf("writer evidence contains more than one JSON value")
	}
	if evidence.ProtocolVersion != EvidenceVersion || evidence.Target == "" || evidence.Environment == "" || evidence.PlanID == "" || evidence.BundleEntryDigest == "" || !sha256Fingerprint(evidence.DesiredFingerprint) || evidence.Generation < 1 || evidence.Release == "" || len(evidence.Cohorts) == 0 {
		return evidence, fmt.Errorf("writer evidence identity, release, timestamps, and cohorts are required")
	}
	if _, err := time.Parse(time.RFC3339, evidence.ObservedAt); err != nil {
		return evidence, fmt.Errorf("observed_at must be RFC3339: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, evidence.ExpiresAt); err != nil {
		return evidence, fmt.Errorf("expires_at must be RFC3339: %w", err)
	}
	for index, cohort := range evidence.Cohorts {
		if cohort.Category == "" || cohort.Name == "" || cohort.Status == "" || cohort.SourceKind == "" || cohort.Source == "" {
			return evidence, fmt.Errorf("cohort %d requires category, name, status, source_kind, and source", index+1)
		}
	}
	return evidence, nil
}

func validateEvidence(evidence Evidence, manifest bundle.Manifest, environment string, now time.Time, required []string) (string, *Finding) {
	if evidence.Target != manifest.Target || evidence.Environment != environment || evidence.PlanID != manifest.PlanID || evidence.Generation != manifest.Generation || evidence.DesiredFingerprint != manifest.DesiredSource.Fingerprint || manifest.History == nil || evidence.BundleEntryDigest != manifest.History.EntryDigest {
		return "stale", &Finding{Code: "writer_evidence_binding_mismatch", Message: "writer evidence targets another plan, desired graph, generation, history entry, target, or environment"}
	}
	observed, _ := time.Parse(time.RFC3339, evidence.ObservedAt)
	expires, _ := time.Parse(time.RFC3339, evidence.ExpiresAt)
	if observed.After(now) || !expires.After(now) || !expires.After(observed) || now.Sub(observed) > 24*time.Hour || expires.Sub(observed) > 24*time.Hour {
		return "stale", &Finding{Code: "writer_evidence_expired", Message: "writer evidence is future-dated, expired, or has an invalid observation window"}
	}
	seen := make(map[string]bool)
	seenCohorts := make(map[string]bool)
	for _, cohort := range evidence.Cohorts {
		identity := cohort.Category + "\x00" + cohort.Name
		if seenCohorts[identity] {
			return "blocked", &Finding{Code: "writer_cohort_duplicate", Message: cohort.Category + "/" + cohort.Name + " is duplicated"}
		}
		seenCohorts[identity] = true
		seen[cohort.Category] = true
		switch cohort.Status {
		case "upgraded", "drained", "isolated", "read_only":
		default:
			return "blocked", &Finding{Code: "writer_cohort_unknown", Message: cohort.Category + "/" + cohort.Name + " has non-drained status " + cohort.Status}
		}
		if cohort.SourceKind != "provider" && cohort.SourceKind != "manual" {
			return "blocked", &Finding{Code: "writer_evidence_source_invalid", Message: cohort.Category + "/" + cohort.Name + " has unsupported evidence source " + cohort.SourceKind}
		}
	}
	for _, category := range required {
		if !seen[category] {
			return "blocked", &Finding{Code: "writer_cohort_missing", Message: "writer evidence omits required cohort category " + category}
		}
	}
	return "ready", nil
}

func sha256Fingerprint(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "sha256:") {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func queryBoolean(ctx context.Context, tx pgx.Tx, sql string) (bool, error) {
	trimmed := strings.TrimSpace(sql)
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "WITH ") {
		return false, fmt.Errorf("gate SQL must be one read-only SELECT")
	}
	rows, err := tx.Query(ctx, trimmed)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if len(rows.FieldDescriptions()) != 1 || !rows.Next() {
		return false, fmt.Errorf("gate must return exactly one Boolean column and one row")
	}
	var result bool
	if err := rows.Scan(&result); err != nil {
		return false, fmt.Errorf("scan Boolean gate: %w", err)
	}
	if rows.Next() {
		return false, fmt.Errorf("gate returned more than one row")
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return result, nil
}

func requiredWriterCategories(gates []protocol.ContractGate) []string {
	seen := make(map[string]bool)
	for _, gate := range gates {
		if gate.Kind == "writer_attestation" {
			for _, category := range gate.RequiredEvidence {
				seen[category] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for category := range seen {
		result = append(result, category)
	}
	sort.Strings(result)
	return result
}

func finalize(report Report) Report {
	report.Digest = ""
	body, _ := json.Marshal(report)
	sum := sha256.Sum256(body)
	report.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return report
}
