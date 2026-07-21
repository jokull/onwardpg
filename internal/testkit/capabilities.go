package testkit

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type Capability string

const (
	CapabilityTableDDL                   Capability = "table_ddl"
	CapabilityCheckConstraint            Capability = "check_constraint"
	CapabilityTrigger                    Capability = "trigger"
	CapabilityView                       Capability = "view"
	CapabilityMaterializedView           Capability = "materialized_view"
	CapabilityExplicitPrepareSameBackend Capability = "explicit_prepare_same_backend"
	CapabilityDistinctBackends           Capability = "distinct_backends"
	CapabilityConcurrentIndex            Capability = "concurrent_index"
	CapabilityRolesAndOwnership          Capability = "roles_and_ownership"
	CapabilityDatabaseIsolation          Capability = "database_isolation"
	CapabilityPostgreSQLMajorMatrix      Capability = "postgresql_major_matrix"
)

var knownPGliteCapabilities = []Capability{
	CapabilityTableDDL,
	CapabilityCheckConstraint,
	CapabilityTrigger,
	CapabilityView,
	CapabilityMaterializedView,
	CapabilityExplicitPrepareSameBackend,
	CapabilityDistinctBackends,
	CapabilityConcurrentIndex,
	CapabilityRolesAndOwnership,
	CapabilityDatabaseIsolation,
	CapabilityPostgreSQLMajorMatrix,
}

type CapabilityReport struct {
	Version             string              `json:"version"`
	ServerVersionNumber int                 `json:"server_version_number"`
	BackendPIDs         []int32             `json:"backend_pids"`
	SharedBackend       bool                `json:"shared_backend"`
	AvailableExtensions []string            `json:"available_extensions"`
	Supported           map[Capability]bool `json:"supported"`
}

func (r CapabilityReport) Supports(capability Capability) bool {
	return r.Supported[capability]
}

type capabilityProbe struct {
	capability Capability
	statements []string
	assertion  string
}

// ProbePGliteCapabilities records the engine identity and directly exercises
// the SQL features admitted to the fast lane. It also verifies PGlite's shared
// backend limitation so an upgrade cannot silently broaden a test claim.
func ProbePGliteCapabilities(ctx context.Context, databaseURL string) (CapabilityReport, error) {
	report := CapabilityReport{Supported: make(map[Capability]bool, len(knownPGliteCapabilities))}
	for _, capability := range knownPGliteCapabilities {
		report.Supported[capability] = false
	}

	first, err := connectPGlite(ctx, databaseURL)
	if err != nil {
		return report, err
	}
	defer first.Close(context.Background())

	var firstPID int32
	if err := first.QueryRow(ctx, "SELECT version(), current_setting('server_version_num')::int, pg_backend_pid()").Scan(
		&report.Version, &report.ServerVersionNumber, &firstPID,
	); err != nil {
		return report, fmt.Errorf("read PGlite engine identity: %w", err)
	}
	report.BackendPIDs = append(report.BackendPIDs, firstPID)

	second, err := connectPGlite(ctx, databaseURL)
	if err != nil {
		return report, fmt.Errorf("open second logical PGlite client: %w", err)
	}
	var secondPID int32
	if err := second.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&secondPID); err != nil {
		second.Close(context.Background())
		return report, fmt.Errorf("read second PGlite backend identity: %w", err)
	}
	second.Close(context.Background())
	report.BackendPIDs = append(report.BackendPIDs, secondPID)
	report.SharedBackend = len(report.BackendPIDs) == 2 && report.BackendPIDs[0] == report.BackendPIDs[1]

	rows, err := first.Query(ctx, "SELECT name FROM pg_available_extensions ORDER BY name")
	if err != nil {
		return report, fmt.Errorf("list PGlite available extensions: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return report, fmt.Errorf("scan PGlite available extension: %w", err)
		}
		report.AvailableExtensions = append(report.AvailableExtensions, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, fmt.Errorf("iterate PGlite available extensions: %w", err)
	}
	rows.Close()

	probes := []capabilityProbe{
		{
			capability: CapabilityTableDDL,
			statements: []string{
				"CREATE SCHEMA onwardpg_probe_table",
				"CREATE TABLE onwardpg_probe_table.items (id bigint PRIMARY KEY)",
				"ALTER TABLE onwardpg_probe_table.items ADD COLUMN note text",
				"INSERT INTO onwardpg_probe_table.items VALUES (1, 'ok')",
			},
			assertion: "SELECT count(*) = 1 FROM onwardpg_probe_table.items WHERE note = 'ok'",
		},
		{
			capability: CapabilityCheckConstraint,
			statements: []string{
				"CREATE SCHEMA onwardpg_probe_check",
				"CREATE TABLE onwardpg_probe_check.items (state text CONSTRAINT state_check CHECK (state IN ('old', 'new')))",
				"INSERT INTO onwardpg_probe_check.items VALUES ('old'), ('new')",
			},
			assertion: "SELECT count(*) = 2 FROM onwardpg_probe_check.items",
		},
		{
			capability: CapabilityTrigger,
			statements: []string{
				"CREATE SCHEMA onwardpg_probe_trigger",
				"CREATE TABLE onwardpg_probe_trigger.items (source text, copy text)",
				"CREATE FUNCTION onwardpg_probe_trigger.sync_copy() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.copy := NEW.source; RETURN NEW; END $$",
				"CREATE TRIGGER sync_copy BEFORE INSERT OR UPDATE ON onwardpg_probe_trigger.items FOR EACH ROW EXECUTE FUNCTION onwardpg_probe_trigger.sync_copy()",
				"INSERT INTO onwardpg_probe_trigger.items (source) VALUES ('ok')",
			},
			assertion: "SELECT source = copy FROM onwardpg_probe_trigger.items",
		},
		{
			capability: CapabilityView,
			statements: []string{
				"CREATE SCHEMA onwardpg_probe_view",
				"CREATE TABLE onwardpg_probe_view.items (id bigint, note text)",
				"CREATE VIEW onwardpg_probe_view.item_view AS SELECT id FROM onwardpg_probe_view.items",
				"CREATE OR REPLACE VIEW onwardpg_probe_view.item_view AS SELECT id, note FROM onwardpg_probe_view.items",
				"INSERT INTO onwardpg_probe_view.items VALUES (1, 'ok')",
			},
			assertion: "SELECT id = 1 AND note = 'ok' FROM onwardpg_probe_view.item_view",
		},
		{
			capability: CapabilityMaterializedView,
			statements: []string{
				"CREATE SCHEMA onwardpg_probe_matview",
				"CREATE TABLE onwardpg_probe_matview.items (id bigint)",
				"INSERT INTO onwardpg_probe_matview.items VALUES (1)",
				"CREATE MATERIALIZED VIEW onwardpg_probe_matview.item_rollup AS SELECT count(*) AS total FROM onwardpg_probe_matview.items",
				"REFRESH MATERIALIZED VIEW onwardpg_probe_matview.item_rollup",
			},
			assertion: "SELECT total = 1 FROM onwardpg_probe_matview.item_rollup",
		},
	}

	for _, probe := range probes {
		supported, err := runCapabilityProbe(ctx, first, probe)
		if err != nil {
			return report, fmt.Errorf("probe PGlite capability %s: %w", probe.capability, err)
		}
		report.Supported[probe.capability] = supported
	}

	prepared, err := probeExplicitPrepare(ctx, first)
	if err != nil {
		return report, fmt.Errorf("probe PGlite explicit prepare: %w", err)
	}
	report.Supported[CapabilityExplicitPrepareSameBackend] = prepared

	expectedMajor := report.ServerVersionNumber/10000 == 17
	if !expectedMajor || !strings.Contains(report.Version, "PostgreSQL 17.") {
		return report, fmt.Errorf("pinned PGlite engine changed: got %q (%d), want PostgreSQL 17.x", report.Version, report.ServerVersionNumber)
	}
	if !report.SharedBackend {
		return report, fmt.Errorf("pinned PGlite backend model changed: logical client PIDs = %v, want one shared backend", report.BackendPIDs)
	}
	if len(report.AvailableExtensions) != 1 || report.AvailableExtensions[0] != "plpgsql" {
		return report, fmt.Errorf("pinned PGlite extension surface changed: got %v, want [plpgsql]", report.AvailableExtensions)
	}
	for _, required := range []Capability{
		CapabilityTableDDL,
		CapabilityCheckConstraint,
		CapabilityTrigger,
		CapabilityView,
		CapabilityMaterializedView,
		CapabilityExplicitPrepareSameBackend,
	} {
		if !report.Supports(required) {
			return report, fmt.Errorf("pinned PGlite lost required preflight capability %s", required)
		}
	}
	return report, nil
}

func connectPGlite(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	config, err := PGliteConnConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect PGlite capability probe: %w", err)
	}
	return connection, nil
}

func runCapabilityProbe(ctx context.Context, connection *pgx.Conn, probe capabilityProbe) (bool, error) {
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(context.Background())
	for _, statement := range probe.statements {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return false, nil
		}
	}
	var valid bool
	if err := transaction.QueryRow(ctx, probe.assertion).Scan(&valid); err != nil {
		return false, nil
	}
	return valid, nil
}

func probeExplicitPrepare(ctx context.Context, connection *pgx.Conn) (bool, error) {
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(ctx, "CREATE SCHEMA onwardpg_probe_prepare; CREATE TABLE onwardpg_probe_prepare.items (id bigint PRIMARY KEY); INSERT INTO onwardpg_probe_prepare.items VALUES (1)"); err != nil {
		return false, nil
	}
	const name = "onwardpg_pglite_explicit_prepare_probe"
	if _, err := connection.Prepare(ctx, name, "SELECT count(*) = 1 FROM onwardpg_probe_prepare.items WHERE id = $1"); err != nil {
		return false, nil
	}
	defer connection.Deallocate(context.Background(), name)
	if _, err := transaction.Exec(ctx, "ALTER TABLE onwardpg_probe_prepare.items ADD COLUMN note text"); err != nil {
		return false, nil
	}
	var valid bool
	if err := connection.QueryRow(ctx, name, int64(1)).Scan(&valid); err != nil {
		return false, nil
	}
	return valid, nil
}

type NativeOwner struct {
	ID      string
	Package string
	Test    string
}

type NativeOwnerRegistry struct {
	owners map[string]NativeOwner
}

type PGliteScenarioRegistration struct {
	VariantID     string
	NativeOwnerID string
	Invariant     string
	Requires      []Capability
}

var stableRegistrationID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

func RegisterNativeOwners(owners ...NativeOwner) (NativeOwnerRegistry, error) {
	registry := NativeOwnerRegistry{owners: make(map[string]NativeOwner, len(owners))}
	for _, owner := range owners {
		if !stableRegistrationID.MatchString(owner.ID) {
			return NativeOwnerRegistry{}, fmt.Errorf("native owner ID %q is not stable", owner.ID)
		}
		if owner.Package == "" || !strings.HasPrefix(owner.Test, "Test") {
			return NativeOwnerRegistry{}, fmt.Errorf("native owner %q must name a package and Go test", owner.ID)
		}
		if _, exists := registry.owners[owner.ID]; exists {
			return NativeOwnerRegistry{}, fmt.Errorf("duplicate native owner %q", owner.ID)
		}
		registry.owners[owner.ID] = owner
	}
	if len(registry.owners) == 0 {
		return NativeOwnerRegistry{}, fmt.Errorf("native owner registry is empty")
	}
	return registry, nil
}

// ValidatePGliteScenarios fails registration for duplicate variants, orphaned
// PGlite claims, unknown capabilities, and capabilities this engine did not
// prove. There is intentionally no skip or fallback path.
func ValidatePGliteScenarios(report CapabilityReport, owners NativeOwnerRegistry, scenarios []PGliteScenarioRegistration) error {
	known := make(map[Capability]struct{}, len(knownPGliteCapabilities))
	for _, capability := range knownPGliteCapabilities {
		known[capability] = struct{}{}
	}
	seenVariants := make(map[string]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if !stableRegistrationID.MatchString(scenario.VariantID) {
			return fmt.Errorf("PGlite variant ID %q is not stable", scenario.VariantID)
		}
		if _, exists := seenVariants[scenario.VariantID]; exists {
			return fmt.Errorf("duplicate PGlite variant %q", scenario.VariantID)
		}
		seenVariants[scenario.VariantID] = struct{}{}
		if _, exists := owners.owners[scenario.NativeOwnerID]; !exists {
			return fmt.Errorf("PGlite variant %q has unregistered native owner %q", scenario.VariantID, scenario.NativeOwnerID)
		}
		if strings.TrimSpace(scenario.Invariant) == "" {
			return fmt.Errorf("PGlite variant %q has no protected invariant", scenario.VariantID)
		}
		if len(scenario.Requires) == 0 {
			return fmt.Errorf("PGlite variant %q declares no capabilities", scenario.VariantID)
		}
		seenRequirements := make(map[Capability]struct{}, len(scenario.Requires))
		for _, capability := range scenario.Requires {
			if _, exists := known[capability]; !exists {
				return fmt.Errorf("PGlite variant %q requests unknown capability %q", scenario.VariantID, capability)
			}
			if _, duplicate := seenRequirements[capability]; duplicate {
				return fmt.Errorf("PGlite variant %q repeats capability %q", scenario.VariantID, capability)
			}
			seenRequirements[capability] = struct{}{}
			if !report.Supports(capability) {
				return fmt.Errorf("PGlite variant %q requires unsupported capability %q", scenario.VariantID, capability)
			}
		}
	}
	if len(scenarios) == 0 {
		return fmt.Errorf("PGlite scenario registry is empty")
	}
	return nil
}

func (r CapabilityReport) SupportedCapabilities() []Capability {
	result := make([]Capability, 0, len(r.Supported))
	for capability, supported := range r.Supported {
		if supported {
			result = append(result, capability)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}
