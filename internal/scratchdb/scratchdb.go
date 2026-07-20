// Package scratchdb creates disposable PostgreSQL databases whose SQL
// execution role has no cluster-global authority.
package scratchdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Database owns one random database and its short-lived login role. Admin is
// retained only for cleanup; project DDL must use Config or Connect.
type Database struct {
	admin  *pgx.Conn
	Name   string
	Role   string
	Config *pgx.ConnConfig
}

type databaseEnvironment struct {
	Encoding         string
	Collate          string
	CType            string
	Provider         string
	Locale           string
	CollationVersion string
}

func Create(ctx context.Context, adminURL, prefix string) (*Database, error) {
	if adminURL == "" {
		return nil, fmt.Errorf("scratch administrative URL is required")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect scratch administrator: %w", err)
	}
	environment, err := readDatabaseEnvironment(ctx, admin)
	if err != nil {
		admin.Close(context.Background())
		return nil, err
	}
	suffix, err := randomHex(12)
	if err != nil {
		admin.Close(context.Background())
		return nil, err
	}
	prefix = sanitizePrefix(prefix)
	name := prefix + "_" + suffix
	role := prefix + "_role_" + suffix
	password, err := randomHex(32)
	if err != nil {
		admin.Close(context.Background())
		return nil, err
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	roleSQL := "CREATE ROLE " + quoteIdentifier(role) + " LOGIN PASSWORD " + quoteLiteral(password) +
		" VALID UNTIL " + quoteLiteral(expires) +
		" NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"
	if _, err := admin.Exec(ctx, roleSQL); err != nil {
		admin.Close(context.Background())
		return nil, fmt.Errorf("create restricted scratch role: %w", err)
	}
	databaseCreated := false
	cleanup := func() {
		if databaseCreated {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
		}
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quoteIdentifier(role))
		_ = admin.Close(context.Background())
	}
	if _, err := admin.Exec(ctx, createDatabaseSQL(name, role, environment)); err != nil {
		cleanup()
		return nil, fmt.Errorf("create restricted scratch database: %w", err)
	}
	databaseCreated = true
	config, err := pgx.ParseConfig(adminURL)
	if err != nil {
		cleanup()
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
		cleanup()
		return nil, fmt.Errorf("connect scratch database for bootstrap: %w", err)
	}
	createdEnvironment, err := readDatabaseEnvironment(ctx, adminDatabase)
	if err != nil {
		adminDatabase.Close(context.Background())
		cleanup()
		return nil, err
	}
	if createdEnvironment != environment {
		adminDatabase.Close(context.Background())
		cleanup()
		return nil, fmt.Errorf("scratch database environment %#v does not match source environment %#v", createdEnvironment, environment)
	}
	if _, err := adminDatabase.Exec(ctx, "ALTER SCHEMA public OWNER TO "+quoteIdentifier(role)); err != nil {
		adminDatabase.Close(context.Background())
		cleanup()
		return nil, fmt.Errorf("grant restricted scratch role ownership of public schema: %w", err)
	}
	if err := adminDatabase.Close(ctx); err != nil {
		cleanup()
		return nil, fmt.Errorf("close scratch bootstrap connection: %w", err)
	}
	config.Database = name
	config.User = role
	config.Password = password
	return &Database{admin: admin, Name: name, Role: role, Config: config}, nil
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

// Close force-drops the database before removing its login role. Cleanup uses
// a background context so caller cancellation cannot strand an active login.
func (d *Database) Close() error {
	if d == nil || d.admin == nil {
		return nil
	}
	var failures []string
	if _, err := d.admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdentifier(d.Name)+" WITH (FORCE)"); err != nil {
		failures = append(failures, "drop database: "+err.Error())
	}
	if _, err := d.admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quoteIdentifier(d.Role)); err != nil {
		failures = append(failures, "drop role: "+err.Error())
	}
	if err := d.admin.Close(context.Background()); err != nil {
		failures = append(failures, "close administrator: "+err.Error())
	}
	d.admin = nil
	if len(failures) > 0 {
		return fmt.Errorf("clean up scratch database: %s", strings.Join(failures, "; "))
	}
	return nil
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
