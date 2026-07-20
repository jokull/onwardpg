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
	"github.com/jokull/onwardpg/pgschema"
)

type Evidence struct {
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

type ObserverProjection struct {
	Role            string   `json:"role"`
	DatabaseOwner   string   `json:"database_owner"`
	Mode            string   `json:"mode"`
	ProjectedAccess []string `json:"projected_access,omitempty"`
}

type ReconciliationReadiness struct {
	TransitionID  string   `json:"transition_id"`
	Summary       string   `json:"summary"`
	ExecutionMode string   `json:"execution_mode"`
	GateIDs       []string `json:"gate_ids"`
	PhasePath     string   `json:"phase_path"`
}

type Report struct {
	Status              string                    `json:"status"`
	Target              string                    `json:"target"`
	Environment         string                    `json:"environment"`
	BundleID            string                    `json:"bundle_id"`
	PlanID              string                    `json:"plan_id"`
	Generation          int                       `json:"generation"`
	BundleEntryDigest   string                    `json:"bundle_entry_digest"`
	ExpectedFingerprint string                    `json:"expected_expand_fingerprint"`
	ActualFingerprint   string                    `json:"actual_fingerprint,omitempty"`
	CheckedAt           string                    `json:"checked_at"`
	Observer            ObserverProjection        `json:"observer"`
	GateResults         []GateResult              `json:"gates,omitempty"`
	Reconciliations     []ReconciliationReadiness `json:"reconciliations,omitempty"`
	Findings            []Finding                 `json:"findings,omitempty"`
	Digest              string                    `json:"digest"`
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

type observerContext struct {
	Role                  string
	DatabaseOwner         string
	ContextualUnsupported map[string]bool
	AccessRoles           map[string]bool
}

func (o observerContext) Mode() string {
	if o.Role == o.DatabaseOwner {
		return "database_owner"
	}
	return "dedicated_read_only"
}

func inspectObserver(ctx context.Context, tx pgx.Tx) (observerContext, *Finding, error) {
	var observer observerContext
	var superuser, createDB, createRole, replication, bypassRLS bool
	err := tx.QueryRow(ctx, `
SELECT current_user, pg_get_userbyid(d.datdba),
       r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls
FROM pg_database d
JOIN pg_roles r ON r.rolname = current_user
WHERE d.datname = current_database()`).Scan(
		&observer.Role, &observer.DatabaseOwner,
		&superuser, &createDB, &createRole, &replication, &bypassRLS,
	)
	if err != nil {
		return observer, nil, err
	}
	observer.ContextualUnsupported = make(map[string]bool)
	observer.AccessRoles = map[string]bool{observer.Role: true}
	if observer.Role == observer.DatabaseOwner {
		return observer, nil, nil
	}

	var elevated []string
	for name, enabled := range map[string]bool{
		"SUPERUSER": superuser, "CREATEDB": createDB, "CREATEROLE": createRole,
		"REPLICATION": replication, "BYPASSRLS": bypassRLS,
	} {
		if enabled {
			elevated = append(elevated, name)
		}
	}
	sort.Strings(elevated)
	if len(elevated) > 0 {
		return observer, &Finding{
			Code:        "observer_role_elevated",
			Message:     observer.Role + " is not a dedicated least-privilege observer; prohibited capabilities: " + strings.Join(elevated, ", "),
			Remediation: "use a NOLOGIN/NOSUPERUSER/NOCREATEDB/NOCREATEROLE/NOREPLICATION/NOBYPASSRLS membership-free login with only schema USAGE and relation SELECT",
		}, nil
	}

	memberships, err := tx.Query(ctx, `
SELECT parent.rolname, parent.rolcanlogin, parent.rolsuper, parent.rolcreatedb,
       parent.rolcreaterole, parent.rolreplication, parent.rolbypassrls,
       EXISTS (SELECT 1 FROM pg_auth_members nested WHERE nested.member = parent.oid)
FROM pg_auth_members membership
JOIN pg_roles member ON member.oid = membership.member
JOIN pg_roles parent ON parent.oid = membership.roleid
WHERE member.rolname = current_user
ORDER BY parent.rolname`)
	if err != nil {
		return observer, nil, err
	}
	for memberships.Next() {
		var role string
		var canLogin, roleSuper, roleCreateDB, roleCreateRole, roleReplication, roleBypassRLS, nested bool
		if err := memberships.Scan(&role, &canLogin, &roleSuper, &roleCreateDB, &roleCreateRole, &roleReplication, &roleBypassRLS, &nested); err != nil {
			memberships.Close()
			return observer, nil, err
		}
		if canLogin || roleSuper || roleCreateDB || roleCreateRole || roleReplication || roleBypassRLS || nested || strings.HasPrefix(role, "pg_") {
			memberships.Close()
			return observer, &Finding{
				Code: "observer_role_elevated", Message: observer.Role + " inherits unsafe role " + role,
				Remediation: "inherit only a dedicated NOLOGIN/NOSUPERUSER/NOCREATEDB/NOCREATEROLE/NOREPLICATION/NOBYPASSRLS role with no nested memberships and direct USAGE/SELECT grants",
			}, nil
		}
		observer.AccessRoles[role] = true
	}
	if err := memberships.Err(); err != nil {
		memberships.Close()
		return observer, nil, err
	}
	memberships.Close()

	var unsafeSchemaAccess []string
	rows, err := tx.Query(ctx, `
WITH access_roles AS (
  SELECT oid FROM pg_roles WHERE rolname = current_user
  UNION
  SELECT membership.roleid FROM pg_auth_members membership
  JOIN pg_roles member ON member.oid = membership.member
  WHERE member.rolname = current_user
)
SELECT quote_ident(grantee.rolname) || ':' || quote_ident(n.nspname) || ':' || access.privilege_type ||
       CASE WHEN access.is_grantable THEN ':grantable' ELSE '' END
FROM pg_namespace n
CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) access
JOIN pg_roles grantee ON grantee.oid = access.grantee
WHERE access.grantee IN (SELECT oid FROM access_roles)
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND (access.privilege_type <> 'USAGE' OR access.is_grantable)
ORDER BY 1`)
	if err != nil {
		return observer, nil, err
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return observer, nil, err
		}
		unsafeSchemaAccess = append(unsafeSchemaAccess, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observer, nil, err
	}
	rows.Close()
	if len(unsafeSchemaAccess) > 0 {
		return observer, &Finding{
			Code:        "observer_access_policy_unsafe",
			Message:     "observer has schema privileges beyond non-grantable USAGE: " + strings.Join(unsafeSchemaAccess, ", "),
			Remediation: "revoke CREATE and grant options from the dedicated observer role",
		}, nil
	}

	var missingAccess []string
	rows, err = tx.Query(ctx, `
SELECT 'schema:' || quote_ident(n.nspname)
FROM pg_namespace n
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT has_schema_privilege(current_user, n.oid, 'USAGE')
UNION ALL
SELECT 'relation:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT has_table_privilege(current_user, c.oid, 'SELECT')
ORDER BY 1`)
	if err != nil {
		return observer, nil, err
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return observer, nil, err
		}
		missingAccess = append(missingAccess, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observer, nil, err
	}
	rows.Close()
	if len(missingAccess) > 0 {
		return observer, &Finding{
			Code:        "observer_access_incomplete",
			Message:     "observer cannot inspect every application schema/relation: " + strings.Join(missingAccess, ", "),
			Remediation: "grant non-grantable USAGE on application schemas and SELECT on their tables and views",
		}, nil
	}

	rows, err = tx.Query(ctx, contextualUnsupportedQuery)
	if err != nil {
		return observer, nil, err
	}
	for rows.Next() {
		var selector string
		if err := rows.Scan(&selector); err != nil {
			rows.Close()
			return observer, nil, err
		}
		observer.ContextualUnsupported[selector] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observer, nil, err
	}
	rows.Close()
	return observer, nil, nil
}

func projectObserverSnapshot(snapshot *pgschema.Snapshot, observer observerContext) (*pgschema.Snapshot, []string, *Finding, error) {
	if observer.Role == observer.DatabaseOwner {
		return snapshot, nil, nil, nil
	}
	var projected []string
	var unsafe []string
	result, err := snapshot.Project(func(object pgschema.Object) (pgschema.Object, bool) {
		switch value := object.(type) {
		case pgschema.Table:
			if value.Owner == observer.DatabaseOwner {
				value.Owner = ""
			}
			return value, true
		case pgschema.View:
			if value.Owner == observer.DatabaseOwner {
				value.Owner = ""
			}
			return value, true
		case pgschema.TablePrivilege:
			if !observer.AccessRoles[value.Grantee] {
				return value, true
			}
			if value.Privilege != "SELECT" || value.Grantable || value.Grantor != "@owner" {
				unsafe = append(unsafe, value.ObjectID().String())
				return value, true
			}
			projected = append(projected, value.ObjectID().String())
			return nil, false
		default:
			return object, true
		}
	}, func(selector string) bool {
		if observer.ContextualUnsupported[selector] {
			projected = append(projected, selector)
			return false
		}
		return true
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("project dedicated observer catalog overlay: %w", err)
	}
	sort.Strings(projected)
	sort.Strings(unsafe)
	if len(unsafe) > 0 {
		return result, projected, &Finding{
			Code:        "observer_access_policy_unsafe",
			Message:     "observer has relation privileges beyond owner-granted, non-grantable SELECT: " + strings.Join(unsafe, ", "),
			Remediation: "use a dedicated observer with direct, non-grantable SELECT only",
		}, nil
	}
	return result, projected, nil, nil
}

const contextualUnsupportedQuery = `
WITH database_identity AS (
  SELECT datdba FROM pg_database WHERE datname = current_database()
), observer_identity AS (
  SELECT oid FROM pg_roles WHERE rolname = current_user
  UNION
  SELECT membership.roleid FROM pg_auth_members membership
  JOIN pg_roles member ON member.oid = membership.member
  WHERE member.rolname = current_user
)
SELECT 'ownership:schema:' || quote_ident(n.nspname) || '=' || quote_ident(owner.rolname)
FROM pg_namespace n
JOIN pg_roles owner ON owner.oid = n.nspowner
JOIN database_identity database ON database.datdba = owner.oid
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT (n.nspname = 'public' AND (owner.rolname = 'pg_database_owner' OR owner.oid = database.datdba))
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:relation:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || quote_ident(owner.rolname)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_roles owner ON owner.oid = c.relowner
JOIN database_identity database ON database.datdba = owner.oid
WHERE c.relkind IN ('S', 'f') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:type:' || quote_ident(n.nspname) || '.' || quote_ident(t.typname) || '=' || quote_ident(owner.rolname)
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_roles owner ON owner.oid = t.typowner
JOIN database_identity database ON database.datdba = owner.oid
WHERE t.typtype IN ('e', 'd', 'c', 'r', 'm') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype = t.oid AND c.relkind IN ('r', 'p', 'v', 'm'))
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:routine:' || quote_ident(n.nspname) || '.' || quote_ident(p.proname) || '(' || pg_get_function_identity_arguments(p.oid) || ')=' || quote_ident(owner.rolname)
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
JOIN pg_roles owner ON owner.oid = p.proowner
JOIN database_identity database ON database.datdba = owner.oid
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype IN ('e', 'i'))
UNION ALL
SELECT 'ownership:extension:' || quote_ident(e.extname) || '=' || quote_ident(owner.rolname)
FROM pg_extension e
JOIN pg_roles owner ON owner.oid = e.extowner
JOIN database_identity database ON database.datdba = owner.oid
WHERE e.extname <> 'plpgsql'
UNION ALL
SELECT 'acl:schema:' || quote_ident(n.nspname)
FROM pg_namespace n
CROSS JOIN database_identity database
WHERE n.nspacl IS NOT NULL AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND (SELECT count(*) FROM aclexplode(n.nspacl) access
       WHERE access.grantee IN (SELECT oid FROM observer_identity) AND access.privilege_type = 'USAGE'
         AND NOT access.is_grantable AND access.grantor IN (n.nspowner, database.datdba)) = 1
  AND NOT EXISTS (SELECT 1 FROM aclexplode(n.nspacl) access
                  WHERE access.grantee IN (SELECT oid FROM observer_identity)
                    AND (access.privilege_type <> 'USAGE' OR access.is_grantable OR access.grantor NOT IN (n.nspowner, database.datdba)))
  AND NOT EXISTS (SELECT 1 FROM aclexplode(n.nspacl) access
                  WHERE access.grantee NOT IN (SELECT oid FROM observer_identity)
                    AND NOT (access.grantee = n.nspowner AND access.privilege_type IN ('USAGE', 'CREATE') AND NOT access.is_grantable
                             OR n.nspname = 'public' AND access.grantee = 0 AND access.privilege_type = 'USAGE' AND NOT access.is_grantable))
  AND (SELECT count(*) FROM aclexplode(n.nspacl) access WHERE access.grantee NOT IN (SELECT oid FROM observer_identity))
      = CASE WHEN n.nspname = 'public' THEN 3 ELSE 2 END
ORDER BY 1`

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
		Status: "blocked", Target: manifest.Target, Environment: input.Environment,
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
	observer, finding, err := inspectObserver(ctx, tx)
	if err != nil {
		return Report{}, fmt.Errorf("inspect readiness observer: %w", err)
	}
	report.Observer = ObserverProjection{Role: observer.Role, DatabaseOwner: observer.DatabaseOwner, Mode: observer.Mode()}
	if finding != nil {
		report.Findings = append(report.Findings, *finding)
		return finalize(report), nil
	}
	actual, err := source.InspectGraphTransaction(ctx, tx, input.Ignores, false)
	if err != nil {
		return Report{}, fmt.Errorf("inspect production catalog read-only: %w", err)
	}
	actual, projected, finding, err := projectObserverSnapshot(actual, observer)
	if err != nil {
		return Report{}, err
	}
	report.Observer.ProjectedAccess = projected
	if finding != nil {
		report.Findings = append(report.Findings, *finding)
		return finalize(report), nil
	}
	if observer.Role != observer.DatabaseOwner {
		for _, object := range actual.Objects() {
			rowSecurity, ok := object.(pgschema.RowSecurity)
			if !ok || !rowSecurity.Enabled {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Code:        "observer_rls_incomplete",
				Message:     rowSecurity.Table.String() + " has row-level security enabled; a dedicated observer cannot prove complete-table data gates",
				Remediation: "run contract check through the database owner, or remove data-gate dependence on RLS-filtered tables",
			})
			return finalize(report), nil
		}
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
	var evidenceGates []protocol.ContractGate
	var failedDataGateIDs, pendingManualGateIDs []string
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
				if gate.Kind == "manual_reconciliation" {
					pendingManualGateIDs = append(pendingManualGateIDs, gate.ID)
				} else {
					failedDataGateIDs = append(failedDataGateIDs, gate.ID)
				}
			}
		case "writer_attestation", "operation_attestation":
			evidenceGates = append(evidenceGates, gate)
		}
	}
	if len(evidenceGates) > 0 {
		if len(bytes.TrimSpace(input.Evidence)) == 0 {
			report.Status = "needs_evidence"
			report.Findings = append(report.Findings, Finding{Code: "contract_evidence_missing", Message: "writer-drain or operation evidence is required before contract can become ready"})
			return finalize(report), nil
		}
		evidence, err := DecodeEvidence(input.Evidence)
		if err != nil {
			report.Findings = append(report.Findings, Finding{Code: "writer_evidence_invalid", Message: err.Error()})
			return finalize(report), nil
		}
		status, finding := validateEvidence(evidence, manifest, input.Environment, input.Now, requiredEvidenceCategories(gates))
		if finding != nil {
			report.Status = status
			report.Findings = append(report.Findings, *finding)
			return finalize(report), nil
		}
		for _, gate := range evidenceGates {
			report.GateResults = append(report.GateResults, GateResult{ID: gate.ID, Kind: gate.Kind, Passed: true})
		}
	}
	if len(failedDataGateIDs) > 0 {
		sort.Strings(failedDataGateIDs)
		report.Findings = append(report.Findings, Finding{
			Code: "data_gate_failed", Message: "contract assertions are false: " + strings.Join(failedDataGateIDs, ", "),
			Remediation: "resolve the data invariant or choose a receipted manual reconciliation, then repeat contract check",
		})
		return finalize(report), nil
	}
	if len(pendingManualGateIDs) > 0 {
		reconciliations, err := pendingReconciliations(input.Artifact, pendingManualGateIDs)
		if err != nil {
			return Report{}, err
		}
		report.Status = "reconciliation_required"
		report.Reconciliations = reconciliations
		report.Findings = append(report.Findings, Finding{
			Code: "reconciliation_required", Message: "writer evidence is satisfied; receipted post-drain reconciliation must run before contract enforcement",
			Remediation: "run the named reconciliation from phases/contract.sql through the deployment executor, then repeat contract check",
		})
		return finalize(report), nil
	}
	report.Status = "ready"
	return finalize(report), nil
}

func pendingReconciliations(artifact bundle.Artifact, pendingGateIDs []string) ([]ReconciliationReadiness, error) {
	var plan protocol.Result
	if err := json.Unmarshal(artifact.Files["plan.json"], &plan); err != nil {
		return nil, fmt.Errorf("decode reconciliation plan: %w", err)
	}
	pending := make(map[string]bool, len(pendingGateIDs))
	for _, id := range pendingGateIDs {
		pending[id] = true
	}
	covered := make(map[string]bool, len(pending))
	var result []ReconciliationReadiness
	for _, reconciliation := range plan.Reconciliations {
		if reconciliation.Strategy != "manual_sql" && reconciliation.Strategy != "generated_sql" || reconciliation.Work == nil {
			continue
		}
		matches := false
		for _, id := range reconciliation.GateIDs {
			if pending[id] {
				matches, covered[id] = true, true
			}
		}
		if !matches {
			continue
		}
		summary := strings.TrimSpace(reconciliation.Work.Summary)
		if summary == "" {
			summary = "operator-authored reconciliation"
		}
		phasePath := "phases/contract.sql"
		for _, operation := range plan.Operations {
			if operation.TransitionID != reconciliation.TransitionID {
				continue
			}
			for _, gateID := range operation.CompletionGateIDs {
				if pending[gateID] {
					phasePath = "operations/" + operation.ID + ".json"
				}
			}
		}
		result = append(result, ReconciliationReadiness{
			TransitionID: reconciliation.TransitionID, Summary: summary,
			ExecutionMode: reconciliation.Work.ExecutionMode,
			GateIDs:       append([]string(nil), reconciliation.GateIDs...), PhasePath: phasePath,
		})
	}
	for id := range pending {
		if !covered[id] {
			return nil, fmt.Errorf("manual reconciliation gate %q is not bound to receipted manual work", id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TransitionID < result[j].TransitionID })
	return result, nil
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
	if evidence.Target == "" || evidence.Environment == "" || evidence.PlanID == "" || evidence.BundleEntryDigest == "" || !sha256Fingerprint(evidence.DesiredFingerprint) || evidence.Generation < 1 || evidence.Release == "" || len(evidence.Cohorts) == 0 {
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
		case "upgraded", "drained", "isolated", "read_only", "completed":
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

func requiredEvidenceCategories(gates []protocol.ContractGate) []string {
	seen := make(map[string]bool)
	for _, gate := range gates {
		if gate.Kind == "writer_attestation" || gate.Kind == "operation_attestation" {
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
