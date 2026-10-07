// Package contractcheck checks contract preconditions without providing any
// path that can execute migration SQL against the caller's database.
package contractcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/jokull/onwardpg/internal/sqlcheck"
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
	// NextActions gives exact steps for a finding that the caller can
	// resolve with SQL, such as the creation of a valid observer role.
	NextActions []NextAction `json:"next_actions,omitempty"`
}

// NextAction is one way to resolve a finding. SQL is a template: names in
// angle brackets are for the caller to replace.
type NextAction struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	SQL    string `json:"sql,omitempty"`
}

type ObserverProjection struct {
	Role          string `json:"role"`
	DatabaseOwner string `json:"database_owner"`
	Mode          string `json:"mode"`
	// BypassRLS reports that the observer role has BYPASSRLS. On a role that
	// the guard accepted, it adds reading only: the role sees the rows that
	// row-level security hides from other readers.
	BypassRLS       bool     `json:"bypass_rls,omitempty"`
	ProjectedAccess []string `json:"projected_access,omitempty"`
	// LiveIgnored lists the unsupported-state selectors, observed in this
	// catalog, that the target's live_ignore list acknowledged. They are
	// environmental state the reader still sees, but not a blocker.
	LiveIgnored []string `json:"live_ignored,omitempty"`
	// LiveIgnoreUnmatched lists configured live_ignore selectors that matched
	// nothing in this catalog. It is information only: the same configuration
	// serves several environments, and a typo shows up here.
	LiveIgnoreUnmatched []string `json:"live_ignore_unmatched,omitempty"`
	// ObservedFingerprint is the fingerprint of the catalog before LiveIgnored
	// selectors were removed, so a reader can tell that two runs saw the same
	// catalog. The reported actual fingerprint is the one after removal, which
	// is what is compared. Present only when LiveIgnored is not empty.
	ObservedFingerprint string `json:"observed_fingerprint,omitempty"`
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
	Unsupported         []string                  `json:"unsupported,omitempty"`
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
	Options          graphplan.Options
	// LiveIgnore holds the target's live_ignore selectors. It can only
	// acknowledge unsupported state; it cannot hide an object from the
	// receipted checkpoint comparison.
	LiveIgnore []string
}

// observerAccess says what a command reads through the observer connection.
type observerAccess int

const (
	// catalogAccess reads system catalogs only, as drift check does.
	// PostgreSQL lets every role read them, and the catalog inspection calls
	// no function whose result depends on a privilege of the caller. The
	// observer therefore needs no privilege on an application object, and the
	// inspected graph is the same as the one the database owner reads.
	catalogAccess observerAccess = iota
	// dataAccess also reads application rows: contract check runs Boolean
	// data gates and proves that no row is hidden from them. The observer
	// then needs USAGE on each application schema and SELECT on each
	// relation of the database.
	dataAccess
)

// predefinedReadRoles are the PostgreSQL predefined roles that give read
// access only. An observer can be a member of them. pg_monitor is itself a
// member of the three roles after it. No other predefined role is accepted:
// each of the others can write data, signal or configure the server, or read
// or write server files.
var predefinedReadRoles = map[string]bool{
	"pg_read_all_data":     true,
	"pg_monitor":           true,
	"pg_read_all_settings": true,
	"pg_read_all_stats":    true,
	"pg_stat_scan_tables":  true,
}

type observerContext struct {
	Role                  string
	DatabaseOwner         string
	ContextualUnsupported map[string]bool
	// AccessRoles are the roles whose own grants are the access of the
	// observer and nothing else: the observer, and a dedicated NOLOGIN role
	// that it is a direct member of. Their exact read-only grants are removed
	// from the inspected graph. A predefined role is never one of them: a
	// grant to a predefined role is application state and stays in the graph.
	AccessRoles map[string]bool
	// PredefinedRoles are the predefined read-only roles that the observer
	// is a member of, directly or through another of them.
	PredefinedRoles []string
	// BypassRLS is the BYPASSRLS attribute of the observer role.
	BypassRLS bool
	// refused is set when the guard did not accept the role.
	refused bool
}

func (o observerContext) projection() ObserverProjection {
	return ObserverProjection{Role: o.Role, DatabaseOwner: o.DatabaseOwner, Mode: o.Mode(), BypassRLS: o.BypassRLS}
}

func (o observerContext) Mode() string {
	if o.Role == o.DatabaseOwner {
		return "database_owner"
	}
	if o.refused {
		return "refused"
	}
	if len(o.PredefinedRoles) > 0 {
		return "predefined_read_role"
	}
	return "dedicated_read_only"
}

// heldRolesCTE lists every membership that gives the current user a role,
// directly or through another role.
const heldRolesCTE = `
WITH RECURSIVE held AS (
  SELECT membership.roleid, membership.member, membership.admin_option
  FROM pg_auth_members membership
  JOIN pg_roles member ON member.oid = membership.member
  WHERE member.rolname = current_user
  UNION
  SELECT membership.roleid, membership.member, membership.admin_option
  FROM pg_auth_members membership
  JOIN held ON held.roleid = membership.member
)`

const observerRoleRule = "use the database owner, or a login role that is NOSUPERUSER/NOCREATEDB/NOCREATEROLE/NOREPLICATION (BYPASSRLS is permitted) and whose memberships, without ADMIN OPTION, are only predefined read-only roles (pg_read_all_data, pg_monitor, pg_read_all_settings, pg_read_all_stats, pg_stat_scan_tables) or dedicated NOLOGIN roles that have no capabilities and no memberships of their own"

// inspectObserver runs the role guard. It needs only the live connection and
// a few short queries, so callers run it before any slower work.
func inspectObserver(ctx context.Context, tx pgx.Tx, access observerAccess) (observerContext, *Finding, error) {
	observer, finding, err := inspectObserverRole(ctx, tx, access)
	// A role without the access that data gates need is still a valid kind
	// of observer. Every other finding means the role itself is refused.
	if finding != nil && finding.Code != "observer_access_incomplete" {
		observer.refused = true
	}
	return observer, finding, err
}

func inspectObserverRole(ctx context.Context, tx pgx.Tx, access observerAccess) (observerContext, *Finding, error) {
	var observer observerContext
	var database string
	var superuser, createDB, createRole, replication, bypassRLS bool
	err := tx.QueryRow(ctx, `
SELECT current_user, pg_get_userbyid(d.datdba), current_database(),
       r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls
FROM pg_database d
JOIN pg_roles r ON r.rolname = current_user
WHERE d.datname = current_database()`).Scan(
		&observer.Role, &observer.DatabaseOwner, &database,
		&superuser, &createDB, &createRole, &replication, &bypassRLS,
	)
	if err != nil {
		return observer, nil, err
	}
	observer.ContextualUnsupported = make(map[string]bool)
	observer.AccessRoles = map[string]bool{observer.Role: true}
	observer.BypassRLS = bypassRLS
	if observer.Role == observer.DatabaseOwner {
		return observer, nil, nil
	}
	roleActions := observerRoleActions(database, access)

	// BYPASSRLS is not in this list. It lets a role read the rows that
	// row-level security hides, and a team needs that to read tables whose
	// policies give a plain reader no row. It adds no privilege to write.
	// requireReadOnly below proves that the role has none by another route.
	var elevated []string
	for name, enabled := range map[string]bool{
		"SUPERUSER": superuser, "CREATEDB": createDB, "CREATEROLE": createRole,
		"REPLICATION": replication,
	} {
		if enabled {
			elevated = append(elevated, name)
		}
	}
	sort.Strings(elevated)
	if len(elevated) > 0 {
		return observer, &Finding{
			Code:        "observer_role_elevated",
			Message:     observer.Role + " is not a least-privilege observer; prohibited capabilities: " + strings.Join(elevated, ", "),
			Remediation: observerRoleRule,
			NextActions: roleActions,
		}, nil
	}

	type membership struct {
		parent, member  string
		direct, admin   bool
		parentElevated  bool
		parentCanLogin  bool
		parentIsBuiltin bool
	}
	memberships, err := tx.Query(ctx, heldRolesCTE+`
SELECT parent.rolname, member.rolname, member.rolname = current_user, held.admin_option,
       parent.rolcanlogin,
       parent.rolsuper OR parent.rolcreatedb OR parent.rolcreaterole OR parent.rolreplication OR parent.rolbypassrls
FROM held
JOIN pg_roles parent ON parent.oid = held.roleid
JOIN pg_roles member ON member.oid = held.member
ORDER BY parent.rolname, member.rolname, held.admin_option`)
	if err != nil {
		return observer, nil, err
	}
	var held []membership
	hasMemberships := make(map[string]bool)
	for memberships.Next() {
		var next membership
		if err := memberships.Scan(&next.parent, &next.member, &next.direct, &next.admin, &next.parentCanLogin, &next.parentElevated); err != nil {
			memberships.Close()
			return observer, nil, err
		}
		next.parentIsBuiltin = strings.HasPrefix(next.parent, "pg_")
		hasMemberships[next.member] = true
		held = append(held, next)
	}
	if err := memberships.Err(); err != nil {
		memberships.Close()
		return observer, nil, err
	}
	memberships.Close()
	unsafeRole := func(message string) (observerContext, *Finding, error) {
		return observer, &Finding{
			Code: "observer_role_elevated", Message: observer.Role + " " + message,
			Remediation: observerRoleRule, NextActions: roleActions,
		}, nil
	}
	predefined := make(map[string]bool)
	for _, next := range held {
		if next.admin {
			// The holder can give the role to other roles. That is role
			// administration, not observation.
			return unsafeRole("can grant membership in " + next.parent + " (ADMIN OPTION on the membership of " + next.member + ")")
		}
		if predefinedReadRoles[next.parent] {
			// A predefined read-only role may be held directly or through
			// another predefined read-only role, as pg_monitor holds three.
			if !next.direct && !predefinedReadRoles[next.member] {
				return unsafeRole("inherits unsafe role " + next.member)
			}
			predefined[next.parent] = true
			continue
		}
		// Any other role must be a dedicated grant role: held directly, not
		// built in, unable to log in, without capabilities, and without
		// memberships of its own.
		if !next.direct || next.parentIsBuiltin || next.parentCanLogin || next.parentElevated || hasMemberships[next.parent] {
			return unsafeRole("inherits unsafe role " + next.parent)
		}
		observer.AccessRoles[next.parent] = true
	}
	for role := range predefined {
		observer.PredefinedRoles = append(observer.PredefinedRoles, role)
	}
	sort.Strings(observer.PredefinedRoles)

	finding, err := requireReadOnly(ctx, tx)
	if err != nil || finding != nil {
		return observer, finding, err
	}

	// requireReadOnly refused CREATE. This query still refuses a grant option
	// on USAGE, which lets the holder give schema access to other roles.
	var unsafeSchemaAccess []string
	rows, err := tx.Query(ctx, heldRolesCTE+`, access_roles AS (
  SELECT oid FROM pg_roles WHERE rolname = current_user
  UNION
  SELECT roleid FROM held
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
			Remediation: "revoke CREATE and grant options from the observer role and from each role that it is a member of",
		}, nil
	}

	if access == dataAccess {
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
				Message:     "observer cannot read every application schema/relation, which the data gates of contract check need: " + strings.Join(missingAccess, ", "),
				Remediation: "make the observer a member of pg_read_all_data, or grant non-grantable USAGE on application schemas and SELECT on their tables and views",
				NextActions: roleActions,
			}, nil
		}
	}

	accessRoles := make([]string, 0, len(observer.AccessRoles))
	for role := range observer.AccessRoles {
		accessRoles = append(accessRoles, role)
	}
	sort.Strings(accessRoles)
	rows, err = tx.Query(ctx, contextualUnsupportedQuery, accessRoles)
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

// requireReadOnly proves that the observer cannot write or change the schema
// of this database by any route. It asks PostgreSQL for the effective
// privileges of the observer and of each role that it is a member of, so a
// direct grant, a grant to one of its roles, and a grant to PUBLIC all count.
// It also refuses ownership: an owner can do anything to its object.
//
// The check covers the database, each schema, and each relation and sequence
// outside the system schemas. It does not cover what a function does when the
// observer calls it, and it cannot see another database of the cluster.
func requireReadOnly(ctx context.Context, tx pgx.Tx) (*Finding, error) {
	var version int
	if err := tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		return nil, err
	}
	// MAINTAIN (VACUUM, REINDEX, REFRESH MATERIALIZED VIEW, LOCK TABLE) is a
	// privilege from PostgreSQL 17.
	relationPrivileges := "INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER"
	if version >= 170000 {
		relationPrivileges += ", MAINTAIN"
	}
	const limit = 20
	rows, err := tx.Query(ctx, heldRolesCTE+`, identity AS (
  SELECT oid, rolname FROM pg_roles WHERE rolname = current_user
  UNION
  SELECT role.oid, role.rolname FROM held JOIN pg_roles role ON role.oid = held.roleid
), this_database AS (
  SELECT oid FROM pg_database WHERE datname = current_database()
)
SELECT quote_ident(i.rolname) || ' has CREATE on database ' || quote_ident(current_database())
FROM identity i
WHERE has_database_privilege(i.oid, (SELECT oid FROM this_database), 'CREATE')
UNION ALL
SELECT quote_ident(i.rolname) || ' has CREATE on schema ' || quote_ident(n.nspname)
FROM identity i CROSS JOIN pg_namespace n
WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND has_schema_privilege(i.oid, n.oid, 'CREATE')
UNION ALL
SELECT quote_ident(i.rolname) || ' can write to relation ' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM identity i CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND (has_table_privilege(i.oid, c.oid, $1) OR has_any_column_privilege(i.oid, c.oid, 'INSERT, UPDATE, REFERENCES'))
UNION ALL
SELECT quote_ident(i.rolname) || ' can change sequence ' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM identity i CROSS JOIN pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND has_sequence_privilege(i.oid, c.oid, 'USAGE, UPDATE')
UNION ALL
SELECT quote_ident(i.rolname) || ' owns ' || pg_describe_object(d.classid, d.objid, d.objsubid)
FROM identity i
JOIN pg_shdepend d ON d.refclassid = 'pg_authid'::regclass AND d.refobjid = i.oid AND d.deptype = 'o'
WHERE d.dbid IN (0, (SELECT oid FROM this_database))
ORDER BY 1
LIMIT $2`, relationPrivileges, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var writes []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		writes = append(writes, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(writes) == 0 {
		return nil, nil
	}
	if len(writes) > limit {
		writes = append(writes[:limit], "and more")
	}
	return &Finding{
		Code:        "observer_access_policy_unsafe",
		Message:     "observer is not read-only: " + strings.Join(writes, ", "),
		Remediation: "revoke each privilege that is not CONNECT, USAGE on a schema, or SELECT, and give the objects another owner; a privilege that PUBLIC or a role of the observer holds counts as a privilege of the observer",
	}, nil
}

// observerRoleActions gives the SQL for the two forms of a valid observer.
// The names are examples. A statement about a schema is repeated for each
// application schema.
func observerRoleActions(database string, access observerAccess) []NextAction {
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	const attributes = "NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION"
	predefined := NextAction{
		Kind:   "create_observer_role",
		Reason: "a login role whose only membership is the predefined read-only role pg_read_all_data; on a managed provider, create a role that inherits pg_read_all_data with the provider's role tool; no grant is put on an application object, so nothing shows as drift; add BYPASSRLS (ALTER ROLE onwardpg_observer BYPASSRLS) when row-level security hides rows from the role",
		SQL: "CREATE ROLE onwardpg_observer LOGIN PASSWORD '<password>' " + attributes + " IN ROLE pg_read_all_data;\n" +
			"GRANT CONNECT ON DATABASE " + quotedDatabase + " TO onwardpg_observer;",
	}
	dedicated := NextAction{
		Kind: "create_observer_role",
		SQL: "CREATE ROLE onwardpg_observer LOGIN PASSWORD '<password>' " + attributes + ";\n" +
			"GRANT CONNECT ON DATABASE " + quotedDatabase + " TO onwardpg_observer;",
	}
	if access == catalogAccess {
		dedicated.Reason = "a login role without capabilities and without memberships; drift check reads system catalogs only, so the role needs no grant on an application object"
		return []NextAction{predefined, dedicated}
	}
	dedicated.Reason = "a login role that gets its access from a dedicated NOLOGIN role with non-grantable USAGE on each application schema and SELECT on each relation; a check that runs as this observer removes these exact grants from the graph and lists them in observer.projected_access, and a check that runs as another role reports them"
	dedicated.SQL = "CREATE ROLE onwardpg_observer_grants NOLOGIN " + attributes + ";\n" +
		"CREATE ROLE onwardpg_observer LOGIN PASSWORD '<password>' " + attributes + " IN ROLE onwardpg_observer_grants;\n" +
		"GRANT CONNECT ON DATABASE " + quotedDatabase + " TO onwardpg_observer;\n" +
		"GRANT USAGE ON SCHEMA <schema> TO onwardpg_observer_grants;\n" +
		"GRANT SELECT ON ALL TABLES IN SCHEMA <schema> TO onwardpg_observer_grants;"
	return []NextAction{predefined, dedicated}
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
  SELECT oid FROM pg_roles WHERE rolname = ANY($1::text[])
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
	observer, finding, err := inspectObserver(ctx, tx, dataAccess)
	if err != nil {
		return Report{}, fmt.Errorf("inspect readiness observer: %w", err)
	}
	report.Observer = observer.projection()
	if finding != nil {
		report.Findings = append(report.Findings, *finding)
		return finalize(report), nil
	}
	actual, err := source.InspectGraphTransaction(ctx, tx, manifest.Planner.ObserverIgnores(), false)
	if err != nil {
		return Report{}, fmt.Errorf("inspect production catalog read-only: %w", err)
	}
	actual, projected, finding, err := projectObserverSnapshot(actual, observer)
	if err != nil {
		return Report{}, err
	}
	report.Observer.ProjectedAccess = projected
	if finding != nil {
		report.Observer.Mode = "refused"
		report.Findings = append(report.Findings, *finding)
		return finalize(report), nil
	}
	unprojected := actual
	actual, report.Observer.LiveIgnored, report.Observer.LiveIgnoreUnmatched, err = source.ProjectLiveIgnored(actual, input.LiveIgnore)
	if err != nil {
		return Report{}, err
	}
	if err := sqlcheck.RequireUnfilteredRows(ctx, tx); err != nil {
		if !errors.Is(err, sqlcheck.ErrRowSecurity) {
			return Report{}, err
		}
		report.Findings = append(report.Findings, Finding{
			Code:        "observer_rls_incomplete",
			Message:     err.Error(),
			Remediation: "use an observer with complete row visibility for every RLS-enabled table; database ownership alone does not bypass FORCE RLS or policies on another role's tables",
		})
		return finalize(report), nil
	}
	actual, err = withoutObserverIgnoreReceipts(actual, input.Artifact.Manifest.Planner.ObserverIgnoreSelectors)
	if err != nil {
		return Report{}, err
	}
	report.ActualFingerprint, err = graphplan.Fingerprint(actual, input.Options)
	if err != nil {
		return Report{}, err
	}
	if len(report.Observer.LiveIgnored) > 0 {
		raw, err := withoutObserverIgnoreReceipts(unprojected, input.Artifact.Manifest.Planner.ObserverIgnoreSelectors)
		if err != nil {
			return Report{}, err
		}
		if report.Observer.ObservedFingerprint, err = graphplan.Fingerprint(raw, input.Options); err != nil {
			return Report{}, err
		}
	}
	// Unsupported state is part of the observed fingerprint, but a receipted
	// checkpoint never contains any. Classify catalog drift on the modeled
	// graph alone so that unsupported state cannot masquerade as drift, and
	// report that state by name.
	comparable := report.ActualFingerprint
	if unsupported := actual.Unsupported(); len(unsupported) > 0 {
		report.Status, report.Unsupported = "unsupported", unsupported
		report.Findings = append(report.Findings, Finding{
			Code:        "unsupported_catalog_state",
			Message:     "production holds catalog state the planner cannot model: " + strings.Join(unsupported, ", "),
			Remediation: "resolve the state; provider-owned extension, schema, and parameter ACL state can be acknowledged in the target's live_ignore list; diff and drift check refuse the same catalog",
		})
		modeled, err := actual.Project(nil, func(string) bool { return false })
		if err != nil {
			return Report{}, err
		}
		if comparable, err = graphplan.Fingerprint(modeled, input.Options); err != nil {
			return Report{}, err
		}
	}
	if comparable != checkpoint.ExpandFingerprint {
		code, message := "catalog_drift", "production does not match the receipted post-expand catalog"
		switch comparable {
		case checkpoint.BaselineFingerprint:
			code, message = "expand_not_applied", "production still matches the pre-expand baseline"
		case checkpoint.DesiredFingerprint:
			code, message = "contract_already_applied", "production already matches the desired post-contract catalog"
		}
		report.Findings = append(report.Findings, Finding{Code: code, Message: message, Remediation: "inspect deployment state and catalog drift before running contract"})
		return finalize(report), nil
	}
	if report.Status == "unsupported" {
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
	return sqlcheck.Boolean(ctx, tx, trimmed)
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
