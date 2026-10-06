package scratchdb

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AdminExtension names one extension that the scratch administrative role may
// install into a disposable database, and the schema the project installs it
// into. PostgreSQL lets a non-superuser create only trusted extensions, so a
// project that uses an untrusted contrib extension (earthdistance, dblink, ...)
// cannot be materialized by the restricted owner role. Listing the extension
// here is an explicit, reviewed grant: its install script runs as the scratch
// administrator, in a database that is dropped afterwards, and only when the
// restricted role's own CREATE EXTENSION was refused for that exact name.
type AdminExtension struct {
	Name   string `toml:"name" json:"name"`
	Schema string `toml:"schema" json:"schema"`
}

var extensionNamePattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,62}$`)

// ValidateAdminExtensions rejects entries that cannot name an extension and a
// schema, and duplicate names. It never consults a server.
func ValidateAdminExtensions(extensions []AdminExtension) error {
	seen := make(map[string]bool, len(extensions))
	for index, extension := range extensions {
		if !extensionNamePattern.MatchString(extension.Name) {
			return fmt.Errorf("entry %d: extension name %q must be lowercase letters, digits, '_' or '-' (at most 63 bytes)", index+1, extension.Name)
		}
		if extension.Name == "plpgsql" {
			return fmt.Errorf("entry %d: plpgsql is installed in every database and cannot be listed", index+1)
		}
		if err := validateSchemaName(extension.Schema); err != nil {
			return fmt.Errorf("entry %d (%s): %w", index+1, extension.Name, err)
		}
		if seen[extension.Name] {
			return fmt.Errorf("entry %d: extension %q is listed more than once", index+1, extension.Name)
		}
		seen[extension.Name] = true
	}
	return nil
}

func validateSchemaName(schema string) error {
	switch {
	case schema == "":
		return errors.New("schema is required: name the schema the project installs the extension into (for example \"public\")")
	case len(schema) > 63:
		return fmt.Errorf("schema %q is longer than 63 bytes", schema)
	case strings.HasPrefix(schema, "pg_") || schema == "information_schema":
		return fmt.Errorf("schema %q is reserved by PostgreSQL", schema)
	}
	for _, r := range schema {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("schema %q contains a control character", schema)
		}
	}
	return nil
}

// NormalizeAdminExtensions returns a copy sorted by name, the order recorded in
// bundle receipts. Nil and empty lists both normalize to nil.
func NormalizeAdminExtensions(extensions []AdminExtension) []AdminExtension {
	if len(extensions) == 0 {
		return nil
	}
	normalized := append([]AdminExtension(nil), extensions...)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	return normalized
}

// Option customizes Create.
type Option func(*settings)

type settings struct {
	extensions []AdminExtension
	onInstall  func([]string)
}

// WithInstallObserver reports, after each administrator installation, every
// extension (including dependencies) that the administrator owns in that
// database. Diagnostics such as config check use it to show what a run used.
func WithInstallObserver(observe func(installed []string)) Option {
	return func(s *settings) { s.onInstall = observe }
}

// WithAdminExtensions grants the scratch administrator the right to install the
// listed extensions on demand. The empty list is the default and grants nothing.
func WithAdminExtensions(extensions []AdminExtension) Option {
	return func(s *settings) { s.extensions = NormalizeAdminExtensions(extensions) }
}

// ExplainDenied adds the configuration hint to PostgreSQL's refusal to let the
// restricted scratch role create an untrusted extension. Any other error is
// returned unchanged. The original error stays in the chain.
func ExplainDenied(err error) error {
	pgErr, name := deniedExtension(err)
	if pgErr == nil || !strings.Contains(strings.ToLower(pgErr.Message), "create extension") {
		return err
	}
	return fmt.Errorf("%w; hint: the scratch role is restricted and may create only trusted extensions; if the project needs %q in disposable databases, name it under scratch_admin_extensions in .onwardpg.toml (for example scratch_admin_extensions = [{ name = %q, schema = \"public\" }]) so the scratch administrator installs it", err, name, name)
}

// deniedExtension returns the PostgreSQL error and the first quoted name in
// its message when err is an insufficient-privilege failure (SQLSTATE 42501).
// The SQLSTATE and the quoted name are stable; the message text is localized.
func deniedExtension(err error) (*pgconn.PgError, string) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		return nil, ""
	}
	start := strings.IndexByte(pgErr.Message, '"')
	if start < 0 {
		return nil, ""
	}
	end := strings.IndexByte(pgErr.Message[start+1:], '"')
	if end < 0 {
		return nil, ""
	}
	return pgErr, pgErr.Message[start+1 : start+1+end]
}

// Recover handles one refused CREATE EXTENSION. When err is PostgreSQL denying
// the restricted role an extension that the allowlist names (and that is not
// installed yet), the scratch administrator installs it, with its declared
// dependencies, into the configured schema, and Recover reports true so the
// caller can retry the failed unit of work. Every other error reports false.
func (d *Database) Recover(ctx context.Context, err error) (bool, error) {
	if d == nil || len(d.extensions) == 0 {
		return false, nil
	}
	_, name := deniedExtension(err)
	extension, listed := d.extensions[name]
	if !listed {
		return false, nil
	}
	return d.install(ctx, extension)
}

// Retrying runs unit and, each time PostgreSQL refuses an allowlisted
// extension, installs it as the administrator and runs unit again. A unit must
// be safe to repeat after failure: a transaction that rolled back, or one
// simple-protocol multi-statement message (an implicit transaction). Retries
// are bounded by the allowlist because each extension is installed once.
// Remaining refusals gain the configuration hint.
func (d *Database) Retrying(ctx context.Context, unit func() error) error {
	installedAny := false
	for {
		err := unit()
		if err == nil {
			return nil
		}
		recovered, recoverErr := d.Recover(ctx, err)
		if recoverErr != nil {
			return errors.Join(ExplainDenied(err), recoverErr)
		}
		if recovered {
			installedAny = true
			continue
		}
		if _, name := deniedExtension(err); d != nil && d.extensions[name].Name != "" {
			// Listed, yet still refused: the hint would send the reader back to
			// a setting that is already in place.
			return err
		}
		if installedAny {
			return d.explainRetry(err)
		}
		return ExplainDenied(err)
	}
}

// explainRetry names the one cause that only exists because the administrator
// installed an extension first: the repeated script now meets an object that
// the first attempt would have created itself.
func (d *Database) explainRetry(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "42P06":
		return fmt.Errorf("%w; hint: the scratch administrator created the schema of an extension named in scratch_admin_extensions, so write CREATE SCHEMA IF NOT EXISTS for it (generated plans: pass --if-not-exists)", err)
	case "42710":
		return fmt.Errorf("%w; hint: the scratch administrator installed an extension named in scratch_admin_extensions, so write CREATE EXTENSION IF NOT EXISTS for it (generated plans: pass --if-not-exists)", err)
	}
	return err
}

// OwnershipExemptions lists the exact catalog-ownership selectors,
// ownership:extension:NAME=ADMIN, for extensions the scratch administrator
// created in this database. PostgreSQL has no ALTER EXTENSION ... OWNER TO and
// REASSIGN OWNED cannot move objects owned by the bootstrap superuser, so these
// extensions stay administrator-owned. The typed graph models an extension by
// name, version, and schema only; a graph reader that knows these selectors
// does not report them as foreign ownership, which keeps the fingerprint equal
// to one built where the owner created a trusted extension.
func (d *Database) OwnershipExemptions() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.exemptions...)
}

// InstalledByAdministrator lists the extensions, including dependencies, that
// the scratch administrator created in this database, sorted by name.
func (d *Database) InstalledByAdministrator() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.installed...)
}

func (d *Database) install(ctx context.Context, extension AdminExtension) (bool, error) {
	if d.adminConfig == nil {
		return false, fmt.Errorf("scratch administrator is not available to install extension %q", extension.Name)
	}
	config := d.adminConfig.Copy()
	config.Database = d.Name
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return false, fmt.Errorf("connect scratch administrator to install extension %q: %w", extension.Name, err)
	}
	defer func() { _ = closeAdmin(connection) }()
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(context.Background())
	installed, err := d.installClosure(ctx, transaction, extension, map[string]bool{})
	if err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	rows, err := transaction.Query(ctx, `
SELECT e.extname, 'ownership:extension:' || quote_ident(e.extname) || '=' || quote_ident(r.rolname)
FROM pg_extension e JOIN pg_roles r ON r.oid = e.extowner
WHERE r.rolname = current_user AND e.extname <> 'plpgsql'
ORDER BY e.extname`)
	if err != nil {
		return false, err
	}
	var names, exemptions []string
	for rows.Next() {
		var name, exemption string
		if err := rows.Scan(&name, &exemption); err != nil {
			rows.Close()
			return false, err
		}
		names, exemptions = append(names, name), append(exemptions, exemption)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit installation of extension %q: %w", extension.Name, err)
	}
	d.installed, d.exemptions = names, exemptions
	if d.onInstall != nil {
		d.onInstall(append([]string(nil), names...))
	}
	return true, nil
}

// installClosure installs extension after any allowlisted dependency that is
// missing, each into its own configured schema. Dependencies that are not
// allowlisted must not need superuser rights: the grant never reaches an
// extension the project did not name. It reports false when extension already
// exists, which makes a repeated refusal terminate instead of looping.
func (d *Database) installClosure(ctx context.Context, tx pgx.Tx, extension AdminExtension, visiting map[string]bool) (bool, error) {
	if visiting[extension.Name] {
		return false, fmt.Errorf("extension %q depends on itself", extension.Name)
	}
	visiting[extension.Name] = true
	defer delete(visiting, extension.Name)
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", extension.Name).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	var requires []string
	err := tx.QueryRow(ctx, `
SELECT COALESCE(v.requires::text[], '{}')
FROM pg_available_extensions a
JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = $1`, extension.Name).Scan(&requires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("scratch_admin_extensions names extension %q, which this PostgreSQL server does not provide", extension.Name)
	}
	if err != nil {
		return false, err
	}
	for _, dependency := range requires {
		var present bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", dependency).Scan(&present); err != nil {
			return false, err
		}
		if present {
			continue
		}
		if listed, ok := d.extensions[dependency]; ok {
			if _, err := d.installClosure(ctx, tx, listed, visiting); err != nil {
				return false, err
			}
			continue
		}
		var needsAdministrator bool
		err := tx.QueryRow(ctx, `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a
JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = $1`, dependency).Scan(&needsAdministrator)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if needsAdministrator {
			return false, fmt.Errorf("extension %q requires %q, which is not trusted and not named in scratch_admin_extensions; add it with the schema the project installs it into", extension.Name, dependency)
		}
	}
	// The schema is created for the restricted role so that the project's own
	// CREATE SCHEMA IF NOT EXISTS is a no-op and the schema's owner and ACL match
	// what the project would have produced itself.
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdentifier(extension.Schema)+" AUTHORIZATION "+quoteIdentifier(d.Role)); err != nil {
		return false, fmt.Errorf("create schema %q for extension %q: %w", extension.Schema, extension.Name, err)
	}
	if _, err := tx.Exec(ctx, "CREATE EXTENSION "+quoteIdentifier(extension.Name)+" WITH SCHEMA "+quoteIdentifier(extension.Schema)+" CASCADE"); err != nil {
		return false, fmt.Errorf("scratch administrator could not install extension %q into schema %q (it needs a superuser administrator): %w", extension.Name, extension.Schema, err)
	}
	return true, nil
}

// CheckAdminExtensions probes the scratch server for each listed extension. A
// name the server does not provide is an error: the setting cannot work there.
// An entry for an extension the restricted role may already create (trusted, or
// not superuser-only) is returned as a note, not an error: one repository
// configuration must stay valid on servers that differ in what they trust, and
// the entry has no effect where the extension is trusted because the
// administrator only acts after the restricted role is refused.
func CheckAdminExtensions(ctx context.Context, adminURL string, extensions []AdminExtension) ([]string, error) {
	if len(extensions) == 0 {
		return nil, nil
	}
	connection, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect scratch administrator: %w", err)
	}
	defer func() { _ = closeAdmin(connection) }()
	var notes []string
	for _, extension := range NormalizeAdminExtensions(extensions) {
		var needsAdministrator bool
		err := connection.QueryRow(ctx, `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a
JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = $1`, extension.Name).Scan(&needsAdministrator)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("scratch_admin_extensions names extension %q, which this PostgreSQL server does not provide", extension.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("probe extension %q: %w", extension.Name, err)
		}
		if !needsAdministrator {
			notes = append(notes, fmt.Sprintf("scratch_admin_extensions entry %q is not needed on this scratch server: the restricted role can create it, so the entry has no effect here", extension.Name))
		}
	}
	return notes, nil
}
