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
//
// Version is optional. When set, the administrator installs exactly that
// version; when empty it installs the server's default version. For a listed
// extension the installed version comes from this entry, never from a VERSION
// clause in project DDL: the tool does not parse SQL, and the project's own
// CREATE EXTENSION IF NOT EXISTS is a no-op once the extension exists.
type AdminExtension struct {
	Name    string `toml:"name" json:"name"`
	Schema  string `toml:"schema" json:"schema"`
	Version string `toml:"version" json:"version,omitempty"`
}

var (
	extensionNamePattern    = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,62}$`)
	extensionVersionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,62}$`)
)

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
		if extension.Version != "" && !extensionVersionPattern.MatchString(extension.Version) {
			return fmt.Errorf("entry %d (%s): version %q must be letters, digits, '.', '_', '+' or '-' (at most 63 bytes)", index+1, extension.Name, extension.Version)
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
	name, ok := deniedExtension(err)
	if !ok {
		return err
	}
	return fmt.Errorf("%w; hint: the scratch role is restricted and may create only trusted extensions; if the project needs %q in disposable databases, name it under scratch_admin_extensions in .onwardpg.toml (for example scratch_admin_extensions = [{ name = %q, schema = \"public\" }]) so the scratch administrator installs it", err, name, name)
}

// deniedExtension reports the extension that PostgreSQL's own CREATE EXTENSION
// path refused to a non-superuser. Message wording is localized and project SQL
// can raise any SQLSTATE with any text (RAISE ... USING ERRCODE = '42501'), so
// the evidence is the non-localized source location the server attaches: file
// extension.c, routine execute_extension_script, for SQLSTATE 42501. A
// user-raised error comes from pl_exec.c and cannot match. These values were
// observed with a real refusal on PostgreSQL 15, 16, 17 and 18; the quoted
// extension name is the first identifier of the message PostgreSQL built.
func deniedExtension(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" || pgErr.File != "extension.c" || pgErr.Routine != "execute_extension_script" {
		return "", false
	}
	start := strings.IndexByte(pgErr.Message, '"')
	if start < 0 {
		return "", false
	}
	end := strings.IndexByte(pgErr.Message[start+1:], '"')
	if end < 0 {
		return "", false
	}
	return pgErr.Message[start+1 : start+1+end], true
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
	name, refused := deniedExtension(err)
	if !refused {
		return false, nil
	}
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
		if name, refused := deniedExtension(err); refused && d != nil && d.extensions[name].Name != "" {
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

// InstalledByAdministrator lists the extensions, including dependencies, that
// the scratch administrator created in this database, sorted by name. They are
// owned by the restricted role, exactly as if it had created them.
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
	before, err := extensionNames(ctx, transaction)
	if err != nil {
		return false, err
	}
	if before[extension.Name] {
		return false, nil
	}
	// PostgreSQL has no ALTER EXTENSION ... OWNER TO and REASSIGN OWNED refuses
	// the bootstrap superuser, so a transient NOLOGIN superuser installs the
	// extension and REASSIGN OWNED then hands the extension and its member
	// objects to the restricted role. Afterwards the database looks as if the
	// restricted role had created the extension itself: nothing foreign-owned
	// reaches the catalog reader, and the project's own later DROP EXTENSION or
	// ALTER EXTENSION works. The helper is created, used, and dropped inside this
	// one transaction; no login or membership ever reaches the restricted role.
	helper := d.Role + "_ext"
	steps := []string{
		"CREATE ROLE " + quoteIdentifier(helper) + " NOLOGIN SUPERUSER",
		"SET LOCAL ROLE " + quoteIdentifier(helper),
	}
	for _, step := range steps {
		if _, err := transaction.Exec(ctx, step); err != nil {
			return false, fmt.Errorf("prepare extension installation (the scratch administrator must be a superuser): %w", err)
		}
	}
	if _, err := d.installClosure(ctx, transaction, extension, map[string]bool{}); err != nil {
		return false, err
	}
	if _, err := transaction.Exec(ctx, "RESET ROLE"); err != nil {
		return false, err
	}
	// Only a superuser may own a foreign-data wrapper (dblink, postgres_fdw,
	// file_fdw) or an event trigger, so REASSIGN OWNED would refuse them. Hand
	// those to the administrator first. That matches what a trusted extension
	// leaves behind: its members belong to a superuser.
	superuserOnly, err := transaction.Query(ctx, `
SELECT format('ALTER FOREIGN DATA WRAPPER %I OWNER TO %I', f.fdwname, current_user)
FROM pg_foreign_data_wrapper f JOIN pg_roles r ON r.oid = f.fdwowner WHERE r.rolname = $1
UNION ALL
SELECT format('ALTER EVENT TRIGGER %I OWNER TO %I', e.evtname, current_user)
FROM pg_event_trigger e JOIN pg_roles r ON r.oid = e.evtowner WHERE r.rolname = $1`, helper)
	if err != nil {
		return false, err
	}
	var handOver []string
	for superuserOnly.Next() {
		var statement string
		if err := superuserOnly.Scan(&statement); err != nil {
			superuserOnly.Close()
			return false, err
		}
		handOver = append(handOver, statement)
	}
	superuserOnly.Close()
	if err := superuserOnly.Err(); err != nil {
		return false, err
	}
	for _, statement := range handOver {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return false, fmt.Errorf("keep a superuser-only member of extension %q with the administrator: %w", extension.Name, err)
		}
	}
	for _, step := range []string{
		"REASSIGN OWNED BY " + quoteIdentifier(helper) + " TO " + quoteIdentifier(d.Role),
		"DROP OWNED BY " + quoteIdentifier(helper),
		"DROP ROLE " + quoteIdentifier(helper),
	} {
		if _, err := transaction.Exec(ctx, step); err != nil {
			return false, fmt.Errorf("hand extension %q to the restricted role: %w", extension.Name, err)
		}
	}
	after, err := extensionNames(ctx, transaction)
	if err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit installation of extension %q: %w", extension.Name, err)
	}
	var created []string
	for name := range after {
		if !before[name] {
			created = append(created, name)
		}
	}
	d.installed = append(d.installed, created...)
	sort.Strings(d.installed)
	if d.onInstall != nil {
		d.onInstall(append([]string(nil), d.installed...))
	}
	return true, nil
}

func extensionNames(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, "SELECT extname FROM pg_extension")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
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
JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = COALESCE(NULLIF($2, ''), a.default_version)
WHERE a.name = $1`, extension.Name, extension.Version).Scan(&requires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errUnavailable(extension)
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
	create := "CREATE EXTENSION " + quoteIdentifier(extension.Name) + " WITH SCHEMA " + quoteIdentifier(extension.Schema)
	if extension.Version != "" {
		create += " VERSION " + quoteLiteral(extension.Version)
	}
	if _, err := tx.Exec(ctx, create+" CASCADE"); err != nil {
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
JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = COALESCE(NULLIF($2, ''), a.default_version)
WHERE a.name = $1`, extension.Name, extension.Version).Scan(&needsAdministrator)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errUnavailable(extension)
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

func errUnavailable(extension AdminExtension) error {
	if extension.Version != "" {
		return fmt.Errorf("scratch_admin_extensions names extension %q version %q, which this PostgreSQL server does not provide", extension.Name, extension.Version)
	}
	return fmt.Errorf("scratch_admin_extensions names extension %q, which this PostgreSQL server does not provide", extension.Name)
}

// UseAdminExtensions replaces the allowlist for statements run from now on.
// Replaying accepted history applies each bundle's own receipted list this
// way; what the administrator already installed stays installed.
func (d *Database) UseAdminExtensions(extensions []AdminExtension) error {
	if err := ValidateAdminExtensions(extensions); err != nil {
		return fmt.Errorf("scratch administrator extensions: %w", err)
	}
	d.extensions = nil
	if len(extensions) > 0 {
		d.extensions = make(map[string]AdminExtension, len(extensions))
		for _, extension := range extensions {
			d.extensions[extension.Name] = extension
		}
	}
	return nil
}
