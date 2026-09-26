// Package scratchdb creates disposable PostgreSQL databases whose SQL
// execution role has no cluster-global authority.
package scratchdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Database owns one random database and its short-lived login role. Admin is
// retained only for cleanup; project DDL must use Config or Connect.
type Database struct {
	admin       *pgx.Conn
	adminConfig *pgx.ConnConfig
	Name        string
	Role        string
	Config      *pgx.ConnConfig
}

// Each DROP has its own deadline, leaving room for checkpoint and filesystem
// work on a large scratch database. Tests shorten this to exercise timeouts.
// A timed out pgx connection is discarded before the next operation.
var cleanupOperationTimeout = 30 * time.Second

const cleanupCloseTimeout = 2 * time.Second

type databaseEnvironment struct {
	Encoding         string
	Collate          string
	CType            string
	Provider         string
	Locale           string
	CollationVersion string
}

func Create(ctx context.Context, adminURL, prefix string) (_ *Database, resultErr error) {
	if adminURL == "" {
		return nil, fmt.Errorf("scratch administrative URL is required")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect scratch administrator: %w", err)
	}
	defer func() {
		if admin != nil {
			resultErr = errors.Join(resultErr, closeAdmin(admin))
		}
	}()
	environment, err := readDatabaseEnvironment(ctx, admin)
	if err != nil {
		return nil, err
	}
	suffix, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	prefix = sanitizePrefix(prefix)
	name := prefix + "_" + suffix
	role := prefix + "_role_" + suffix
	password, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	resources := &Database{admin: admin, adminConfig: admin.Config().Copy(), Name: name, Role: role}
	// Even a failed CREATE can have committed before its response was lost.
	// Both generated names are unique and cleanup is safe to repeat.
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, resources.Close())
			admin = nil
		}
	}()
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	roleSQL := "CREATE ROLE " + quoteIdentifier(role) + " LOGIN PASSWORD " + quoteLiteral(password) +
		" VALID UNTIL " + quoteLiteral(expires) +
		" NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"
	if _, err := admin.Exec(ctx, roleSQL); err != nil {
		return nil, fmt.Errorf("create restricted scratch role: %w", err)
	}
	if _, err := admin.Exec(ctx, createDatabaseSQL(name, role, environment)); err != nil {
		return nil, fmt.Errorf("create restricted scratch database: %w", err)
	}
	config, err := pgx.ParseConfig(adminURL)
	if err != nil {
		return nil, fmt.Errorf("parse scratch administrative URL: %w", err)
	}
	// PostgreSQL 15 owns the public schema through the implicit
	// pg_database_owner role. A deliberately NOINHERIT database owner cannot
	// use that schema until bootstrap gives it direct database-local ownership.
	// Do this through the administrative connection, then never expose those
	// credentials to project DDL.
	adminDatabaseConfig := config.Copy()
	adminDatabaseConfig.Database = name
	adminDatabase, err := pgx.ConnectConfig(ctx, adminDatabaseConfig)
	if err != nil {
		return nil, fmt.Errorf("connect scratch database for bootstrap: %w", err)
	}
	defer func() {
		if adminDatabase != nil {
			resultErr = errors.Join(resultErr, closeAdmin(adminDatabase))
		}
	}()
	createdEnvironment, err := readDatabaseEnvironment(ctx, adminDatabase)
	if err != nil {
		return nil, err
	}
	if createdEnvironment != environment {
		return nil, fmt.Errorf("scratch database environment %#v does not match source environment %#v", createdEnvironment, environment)
	}
	if _, err := adminDatabase.Exec(ctx, "ALTER SCHEMA public OWNER TO "+quoteIdentifier(role)); err != nil {
		return nil, fmt.Errorf("grant restricted scratch role ownership of public schema: %w", err)
	}
	if err := closeAdmin(adminDatabase); err != nil {
		adminDatabase = nil
		return nil, fmt.Errorf("close scratch bootstrap connection: %w", err)
	}
	adminDatabase = nil
	config.Database = name
	config.User = role
	config.Password = password
	resources.Config = config
	admin = nil // resources retains the administrative connection for Close.
	return resources, nil
}

func readDatabaseEnvironment(ctx context.Context, connection *pgx.Conn) (databaseEnvironment, error) {
	var environment databaseEnvironment
	err := connection.QueryRow(ctx, `
SELECT pg_encoding_to_char(d.encoding), d.datcollate, d.datctype,
       d.datlocprovider::text,
       COALESCE(to_jsonb(d)->>'datlocale', to_jsonb(d)->>'daticulocale', ''),
       COALESCE(d.datcollversion, '')
FROM pg_database d
WHERE d.datname = current_database()`).Scan(
		&environment.Encoding, &environment.Collate, &environment.CType,
		&environment.Provider, &environment.Locale, &environment.CollationVersion,
	)
	if err != nil {
		return databaseEnvironment{}, fmt.Errorf("inspect scratch database environment: %w", err)
	}
	return environment, nil
}

func createDatabaseSQL(name, role string, environment databaseEnvironment) string {
	options := []string{
		"OWNER " + quoteIdentifier(role),
		"TEMPLATE template0",
		"ENCODING " + quoteLiteral(environment.Encoding),
		"LC_COLLATE " + quoteLiteral(environment.Collate),
		"LC_CTYPE " + quoteLiteral(environment.CType),
	}
	switch environment.Provider {
	case "i":
		options = append(options, "LOCALE_PROVIDER icu")
		if environment.Locale != "" {
			options = append(options, "ICU_LOCALE "+quoteLiteral(environment.Locale))
		}
	case "b":
		options = append(options, "LOCALE_PROVIDER builtin")
		if environment.Locale != "" {
			options = append(options, "BUILTIN_LOCALE "+quoteLiteral(environment.Locale))
		}
	default:
		options = append(options, "LOCALE_PROVIDER libc")
	}
	return "CREATE DATABASE " + quoteIdentifier(name) + " WITH " + strings.Join(options, " ")
}

func (d *Database) Connect(ctx context.Context) (*pgx.Conn, error) {
	if d == nil || d.Config == nil {
		return nil, fmt.Errorf("scratch database is not initialized")
	}
	connection, err := pgx.ConnectConfig(ctx, d.Config)
	if err != nil {
		return nil, fmt.Errorf("connect restricted scratch database: %w", err)
	}
	return connection, nil
}

// Close force-drops the database before removing its login role. It is safe to
// retry after an error: either DROP may have completed before its reply was
// lost. Each attempt uses fresh deadlines independent of Create's context.
func (d *Database) Close() error {
	if d == nil || (d.admin == nil && d.adminConfig == nil) {
		return nil
	}
	var failures []error
	if err := d.drop("DROP DATABASE IF EXISTS " + quoteIdentifier(d.Name) + " WITH (FORCE)"); err != nil {
		failures = append(failures, fmt.Errorf("drop database %q: %w", d.Name, err))
	}
	if err := d.drop("DROP ROLE IF EXISTS " + quoteIdentifier(d.Role)); err != nil {
		failures = append(failures, fmt.Errorf("drop role %q: %w", d.Role, err))
	}
	if err := closeAdmin(d.admin); err != nil {
		failures = append(failures, fmt.Errorf("close administrator: %w", err))
	}
	d.admin = nil
	if len(failures) != 0 {
		return fmt.Errorf("scratch cleanup incomplete or unconfirmed for database %q and role %q; inspect these exact resources on the dedicated scratch cluster and retry cleanup: %w", d.Name, d.Role, errors.Join(failures...))
	}
	d.adminConfig = nil
	return nil
}

func (d *Database) drop(sql string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupOperationTimeout)
	defer cancel()
	if err := d.ensureAdmin(ctx); err != nil {
		return err
	}
	_, err := d.admin.Exec(ctx, sql)
	if err != nil && ctx.Err() == nil && d.admin.IsClosed() {
		// The server may have committed the DROP before the connection died.
		// IF EXISTS makes one reconnect and retry safe.
		if reconnectErr := d.ensureAdmin(ctx); reconnectErr != nil {
			return errors.Join(err, reconnectErr)
		}
		_, err = d.admin.Exec(ctx, sql)
	}
	return err
}

func (d *Database) ensureAdmin(ctx context.Context) error {
	if d.admin != nil && !d.admin.IsClosed() {
		return nil
	}
	if d.admin != nil {
		_ = closeAdmin(d.admin)
	}
	var err error
	d.admin, err = pgx.ConnectConfig(ctx, d.adminConfig)
	return err
}

func closeAdmin(connection *pgx.Conn) error {
	if connection == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupCloseTimeout)
	defer cancel()
	return connection.Close(ctx)
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate scratch identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func sanitizePrefix(value string) string {
	var result strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			result.WriteRune(r)
		}
	}
	if result.Len() == 0 {
		return "onwardpg"
	}
	const maxPrefix = 24
	if result.Len() > maxPrefix {
		return result.String()[:maxPrefix]
	}
	return result.String()
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
