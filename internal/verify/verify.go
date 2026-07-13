// Package verify executes reviewed onwardpg history only in databases it
// creates and destroys itself. It has no API for applying to an existing
// database.
package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

const Version = "onwardpg.verify/v1"

var phases = []string{"expand", "migrate", "contract"}

type Failure struct {
	Code          string `json:"code"`
	BundleID      string `json:"bundle_id"`
	BatchID       string `json:"batch_id,omitempty"`
	Phase         string `json:"phase"`
	CheckID       string `json:"check_id,omitempty"`
	ExecutionMode string `json:"execution_mode,omitempty"`
	Message       string `json:"message"`
	Remediation   string `json:"remediation"`
}

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

type Report struct {
	ProtocolVersion     string           `json:"protocol_version"`
	Outcome             string           `json:"outcome"`
	Target              string           `json:"target"`
	BundleID            string           `json:"bundle_id"`
	HistoryHead         string           `json:"history_head"`
	ThroughPhase        string           `json:"through_phase"`
	ExecutedBatches     int              `json:"executed_batches"`
	ObservedFingerprint string           `json:"observed_fingerprint,omitempty"`
	DesiredFingerprint  string           `json:"desired_fingerprint,omitempty"`
	WorkingFingerprint  string           `json:"working_fingerprint,omitempty"`
	Residual            *protocol.Result `json:"residual,omitempty"`
	Failure             *Failure         `json:"failure,omitempty"`
	Findings            []Finding        `json:"findings,omitempty"`
	ReceiptsUpdated     bool             `json:"receipts_updated,omitempty"`
}

type Input struct {
	AdminURL     string
	Chain        history.Chain
	BundleID     string
	ThroughPhase string
	Ignores      []string
	Options      graphplan.Options
}

func Run(ctx context.Context, input Input) (Report, error) {
	if input.AdminURL == "" {
		return Report{}, fmt.Errorf("disposable database admin URL is required")
	}
	if input.ThroughPhase == "" {
		input.ThroughPhase = "contract"
	}
	if phaseIndex(input.ThroughPhase) < 0 {
		return Report{}, fmt.Errorf("through phase %q is invalid", input.ThroughPhase)
	}
	chain, err := input.Chain.Through(input.BundleID)
	if err != nil {
		return Report{}, err
	}
	postgresMajor, err := source.PostgresMajor(ctx, input.AdminURL)
	if err != nil {
		return Report{}, err
	}
	for _, entry := range chain.Entries {
		recorded := entry.Artifact.Manifest.DesiredSource.PostgresMajor
		if recorded != 0 && recorded != postgresMajor {
			return Report{}, fmt.Errorf("history bundle %s targets PostgreSQL %d but the scratch server is PostgreSQL %d", entry.Directory, recorded, postgresMajor)
		}
	}
	report := Report{
		ProtocolVersion: Version, Outcome: "failed", Target: chain.Target,
		BundleID: input.BundleID, HistoryHead: chain.HeadDigest, ThroughPhase: input.ThroughPhase,
	}
	observed, batches, failure, err := executeDisposable(ctx, input.AdminURL, chain, input.BundleID, input.ThroughPhase, input.Ignores)
	report.ExecutedBatches = batches
	if err != nil {
		return Report{}, err
	}
	if failure != nil {
		report.Failure = failure
		report.Findings = []Finding{{Code: failure.Code, Message: failure.Message, Remediation: failure.Remediation}}
		return report, nil
	}
	desired, _, failure, err := executeDisposable(ctx, input.AdminURL, chain, input.BundleID, "contract", input.Ignores)
	if err != nil {
		return Report{}, err
	}
	if failure != nil {
		report.Failure = failure
		report.Findings = []Finding{{Code: failure.Code, Message: failure.Message, Remediation: failure.Remediation}}
		return report, nil
	}
	if err := source.ValidateIgnoreSelectors(input.Ignores, observed, desired); err != nil {
		return Report{}, err
	}
	report.ObservedFingerprint, err = observed.Fingerprint()
	if err != nil {
		return Report{}, err
	}
	report.DesiredFingerprint, err = desired.Fingerprint()
	if err != nil {
		return Report{}, err
	}
	target := chain.Entries[len(chain.Entries)-1].Artifact.Manifest
	if target.DesiredSource.Fingerprint != report.DesiredFingerprint {
		return Report{}, fmt.Errorf("full history fingerprint %s does not match bundle desired fingerprint %s", report.DesiredFingerprint, target.DesiredSource.Fingerprint)
	}
	residual, err := graphplan.Build(observed, desired, protocol.Answers{}, input.Options)
	if err != nil {
		return Report{}, fmt.Errorf("plan verification residual: %w", err)
	}
	report.Residual = &residual
	if report.ObservedFingerprint == report.DesiredFingerprint && residual.Status == protocol.Planned && len(residual.Statements) == 0 {
		report.Outcome = "verified"
	} else {
		report.Outcome = "residual"
	}
	return report, nil
}

func executeDisposable(ctx context.Context, adminURL string, chain history.Chain, targetBundle, throughPhase string, ignores []string) (snapshot *pgschema.Snapshot, batches int, failure *Failure, resultErr error) {
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("connect disposable database admin: %w", err)
	}
	defer admin.Close(context.Background())
	name, err := databaseName()
	if err != nil {
		return nil, 0, nil, err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
		return nil, 0, nil, fmt.Errorf("create disposable database: %w", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)"); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("drop disposable database %s: %w", name, err)
		}
	}()
	config, err := pgx.ParseConfig(adminURL)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("parse disposable database URL: %w", err)
	}
	config.Database = name
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("connect disposable database: %w", err)
	}
	for _, entry := range chain.Entries {
		data := entry.Artifact.Files["plan.json"]
		var plan protocol.Result
		if err := json.Unmarshal(data, &plan); err != nil {
			connection.Close(context.Background())
			return nil, batches, nil, fmt.Errorf("decode bundle %s plan: %w", entry.Directory, err)
		}
		limit := "contract"
		if entry.Artifact.Manifest.BundleID == targetBundle {
			limit = throughPhase
		}
		for _, phase := range phases {
			if phaseIndex(phase) > phaseIndex(limit) {
				break
			}
			if entry.Artifact.Manifest.PhaseSource == "edited" {
				receipt, exists := entry.Artifact.Manifest.Phases[phase]
				if !exists {
					continue
				}
				body := entry.Artifact.Files[receipt.Path]
				parsed, err := bundle.ParsePhaseSQL(body, receipt.Transactional)
				if err != nil {
					connection.Close(context.Background())
					return nil, batches, nil, fmt.Errorf("parse edited bundle %s phase %s: %w", entry.Directory, phase, err)
				}
				for index, batch := range parsed {
					batches++
					if err := executeRawBatch(ctx, connection, batch); err != nil {
						connection.Close(context.Background())
						return nil, batches, batchFailure(entry.Directory, fmt.Sprintf("edited-%s-%03d", phase, index+1), phase, batch.Transactional, err), nil
					}
				}
				for _, plannedBatch := range plan.Batches {
					if plannedBatch.Phase == phase {
						if err := executeVerification(ctx, connection, plannedBatch); err != nil {
							connection.Close(context.Background())
							return nil, batches, batchFailure(entry.Directory, plannedBatch.ID, phase, plannedBatch.Transactional, err), nil
						}
					}
				}
				continue
			}
			for _, batch := range plan.Batches {
				if batch.Phase != phase {
					continue
				}
				batches++
				if err := executeBatch(ctx, connection, batch); err != nil {
					connection.Close(context.Background())
					return nil, batches, batchFailure(entry.Directory, batch.ID, phase, batch.Transactional, err), nil
				}
			}
		}
		if entry.Artifact.Manifest.PhaseSource == "edited" && limit == "contract" && entry.Artifact.Manifest.VerificationDigest != "" {
			assertions, err := bundle.ParseAssertions(entry.Artifact.Files["verify.sql"])
			if err != nil {
				connection.Close(context.Background())
				return nil, batches, nil, fmt.Errorf("parse bundle %s verify.sql: %w", entry.Directory, err)
			}
			for _, assertion := range assertions {
				var passed bool
				if err := connection.QueryRow(ctx, assertion.SQL).Scan(&passed); err != nil {
					connection.Close(context.Background())
					return nil, batches, assertionFailure(entry.Directory, assertion.ID, "assertion_query_failed", err.Error()), nil
				}
				if !passed {
					connection.Close(context.Background())
					return nil, batches, assertionFailure(entry.Directory, assertion.ID, "assertion_false", "verification assertion returned false"), nil
				}
			}
		}
	}
	if err := connection.Close(ctx); err != nil {
		return nil, batches, nil, fmt.Errorf("close disposable execution connection: %w", err)
	}
	snapshot, err = source.LoadDatabaseGraphForComparison(ctx, config, ignores)
	if err != nil {
		return nil, batches, nil, fmt.Errorf("inspect disposable result: %w", err)
	}
	return snapshot, batches, nil, nil
}

func batchFailure(bundleID, batchID, phase string, transactional bool, err error) *Failure {
	mode, code := "non_transactional", "non_transactional_batch_failed"
	if transactional {
		mode, code = "transactional", "transactional_batch_failed"
	}
	return &Failure{
		Code: code, BundleID: bundleID, BatchID: batchID, Phase: phase,
		ExecutionMode: mode, Message: err.Error(),
		Remediation: "review and edit phases/" + phase + ".sql, then rerun onwardpg verify; execution occurred only in a disposable database",
	}
}

func assertionFailure(bundleID, checkID, code, message string) *Failure {
	return &Failure{
		Code: code, BundleID: bundleID, Phase: "verify", CheckID: checkID,
		Message:     message,
		Remediation: "correct the migration SQL or the named boolean assertion in verify.sql, then rerun onwardpg verify",
	}
}

func executeRawBatch(ctx context.Context, connection *pgx.Conn, batch bundle.SQLBatch) error {
	if !batch.Transactional {
		_, err := connection.PgConn().Exec(ctx, batch.SQL).ReadAll()
		return err
	}
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := transaction.Conn().PgConn().Exec(ctx, batch.SQL).ReadAll(); err != nil {
		_ = transaction.Rollback(context.Background())
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func executeBatch(ctx context.Context, connection *pgx.Conn, batch protocol.Batch) error {
	if !batch.Transactional {
		for _, statement := range batch.Statements {
			if _, err := connection.Exec(ctx, statement.SQL); err != nil {
				return err
			}
		}
		return executeVerification(ctx, connection, batch)
	}
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	for _, statement := range batch.Statements {
		if _, err := transaction.Exec(ctx, statement.SQL); err != nil {
			_ = transaction.Rollback(context.Background())
			return err
		}
	}
	if err := executeVerification(ctx, transaction, batch); err != nil {
		_ = transaction.Rollback(context.Background())
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	return nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func executeVerification(ctx context.Context, connection rowQuerier, batch protocol.Batch) error {
	for _, statement := range batch.Statements {
		if statement.Manual == nil {
			continue
		}
		for _, query := range statement.Manual.VerificationSQL {
			var passed bool
			if err := connection.QueryRow(ctx, query).Scan(&passed); err != nil {
				return fmt.Errorf("manual verification failed: %w", err)
			}
			if !passed {
				return fmt.Errorf("manual verification returned false")
			}
		}
	}
	return nil
}

func phaseIndex(phase string) int {
	for index, candidate := range phases {
		if phase == candidate {
			return index
		}
	}
	return -1
}

func databaseName() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "onwardpg_verify_" + hex.EncodeToString(data), nil
}

func quote(identifier string) string { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
