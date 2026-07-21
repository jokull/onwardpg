package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Observer is a dedicated login with inherited SELECT-only access to one test
// database. It exists to exercise the same production observation boundary
// documented for contract check without granting the test observer ownership.
type Observer struct {
	URL          string
	Role         string
	grantRole    string
	password     string
	adminURL     string
	database     string
	ownerURL     string
	schema       string
	schemaACL    *string
	relationACLs map[uint32]*string
}

func NewReadOnlyObserver(ctx context.Context, adminURL string, postgres *Postgres, schema string) (*Observer, error) {
	if adminURL == "" || postgres == nil || postgres.Config() == nil || schema == "" {
		return nil, fmt.Errorf("observer admin URL, test PostgreSQL database, and schema are required")
	}
	suffix, err := randomObserverSuffix()
	if err != nil {
		return nil, err
	}
	config := postgres.Config()
	observer := &Observer{
		Role: "onwardpg_observer_" + suffix, grantRole: "onwardpg_observer_grants_" + suffix,
		password: "onwardpg-observer-" + suffix, adminURL: adminURL,
		database: config.Database, ownerURL: connectionURL(config), schema: schema,
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect observer administrator: %w", err)
	}
	defer admin.Close(context.Background())
	createRoles := fmt.Sprintf(
		"CREATE ROLE %s NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; "+
			"CREATE ROLE %s LOGIN INHERIT PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; "+
			"ALTER ROLE %s SET default_transaction_read_only = on; GRANT %s TO %s",
		pgx.Identifier{observer.grantRole}.Sanitize(), pgx.Identifier{observer.Role}.Sanitize(),
		quoteObserverLiteral(observer.password), pgx.Identifier{observer.Role}.Sanitize(),
		pgx.Identifier{observer.grantRole}.Sanitize(), pgx.Identifier{observer.Role}.Sanitize(),
	)
	if _, err := admin.Exec(ctx, createRoles); err != nil {
		return nil, fmt.Errorf("create observer roles: %w", err)
	}
	created := true
	defer func() {
		if created {
			_ = observer.Close()
		}
	}()
	owner, err := postgres.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect database owner for observer grants: %w", err)
	}
	defer owner.Close(context.Background())
	if err := owner.QueryRow(ctx, "SELECT nspacl::text FROM pg_namespace WHERE nspname = $1", schema).Scan(&observer.schemaACL); err != nil {
		return nil, fmt.Errorf("snapshot observer schema ACL: %w", err)
	}
	observer.relationACLs = make(map[uint32]*string)
	rows, err := owner.Query(ctx, `SELECT c.oid, c.relacl::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1`, schema)
	if err != nil {
		return nil, fmt.Errorf("snapshot observer relation ACLs: %w", err)
	}
	for rows.Next() {
		var oid uint32
		var acl *string
		if err := rows.Scan(&oid, &acl); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan observer relation ACL: %w", err)
		}
		observer.relationACLs[oid] = acl
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate observer relation ACLs: %w", err)
	}
	rows.Close()
	grants := fmt.Sprintf(
		"GRANT USAGE ON SCHEMA %s TO %s; GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s",
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{observer.grantRole}.Sanitize(),
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{observer.grantRole}.Sanitize(),
	)
	if _, err := owner.Exec(ctx, grants); err != nil {
		return nil, fmt.Errorf("grant observer table access: %w", err)
	}
	observerConfig := config.Copy()
	observerConfig.User = observer.Role
	observerConfig.Password = observer.password
	observer.URL = connectionURL(observerConfig)
	created = false
	return observer, nil
}

func (o *Observer) Close() error {
	if o == nil || o.Role == "" {
		return nil
	}
	ctx := context.Background()
	var failures []string
	if owner, err := pgx.Connect(ctx, o.ownerURL); err == nil {
		_, revokeErr := owner.Exec(ctx, fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA %s FROM %s; REVOKE USAGE ON SCHEMA %s FROM %s",
			pgx.Identifier{o.schema}.Sanitize(), pgx.Identifier{o.grantRole}.Sanitize(),
			pgx.Identifier{o.schema}.Sanitize(), pgx.Identifier{o.grantRole}.Sanitize(),
		))
		if revokeErr != nil {
			failures = append(failures, revokeErr.Error())
		}
		_ = owner.Close(ctx)
	}
	admin, err := pgx.Connect(ctx, o.adminURL)
	if err != nil {
		return fmt.Errorf("connect observer cleanup administrator: %w", err)
	}
	defer admin.Close(ctx)
	cleanup := fmt.Sprintf(
		"REVOKE %s FROM %s; DROP ROLE IF EXISTS %s; DROP ROLE IF EXISTS %s",
		pgx.Identifier{o.grantRole}.Sanitize(), pgx.Identifier{o.Role}.Sanitize(),
		pgx.Identifier{o.Role}.Sanitize(), pgx.Identifier{o.grantRole}.Sanitize(),
	)
	if _, err := admin.Exec(ctx, cleanup); err != nil {
		failures = append(failures, err.Error())
	}
	// GRANT followed by REVOKE expands PostgreSQL's implicit NULL ACL into an
	// explicit owner-only ACL. Restore the exact pre-observer values inside the
	// disposable workload database so the test observer cannot create drift.
	adminConfig, configErr := pgx.ParseConfig(o.adminURL)
	if configErr == nil {
		adminConfig.Database = o.database
		if databaseAdmin, connectErr := pgx.ConnectConfig(ctx, adminConfig); connectErr == nil {
			tx, restoreErr := databaseAdmin.Begin(ctx)
			if restoreErr == nil {
				_, restoreErr = tx.Exec(ctx, "UPDATE pg_catalog.pg_namespace SET nspacl = $1::aclitem[] WHERE nspname = $2", o.schemaACL, o.schema)
				for oid, acl := range o.relationACLs {
					if restoreErr != nil {
						break
					}
					_, restoreErr = tx.Exec(ctx, "UPDATE pg_catalog.pg_class SET relacl = $1::aclitem[] WHERE oid = $2", acl, oid)
				}
				if restoreErr == nil {
					restoreErr = tx.Commit(ctx)
				} else {
					_ = tx.Rollback(ctx)
				}
			}
			if restoreErr != nil {
				failures = append(failures, "restore observer ACLs: "+restoreErr.Error())
			}
			_ = databaseAdmin.Close(ctx)
		} else {
			failures = append(failures, "connect observer ACL restore: "+connectErr.Error())
		}
	} else {
		failures = append(failures, "parse observer ACL restore administrator: "+configErr.Error())
	}
	o.Role = ""
	if len(failures) > 0 {
		return fmt.Errorf("clean observer: %s", strings.Join(failures, "; "))
	}
	return nil
}

func randomObserverSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate observer identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func quoteObserverLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
