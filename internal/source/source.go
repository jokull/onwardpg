// Package source turns supported PostgreSQL inputs into typed graph snapshots.
package source

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

// Spec identifies either a live PostgreSQL URL or a declarative DDL file.
// DDL files are materialized in a disposable PostgreSQL database before their
// catalog graph is inspected; onwardpg does not maintain a partial SQL parser.
type Spec struct {
	Kind  string
	Value string
}

func Parse(value string) Spec {
	if strings.HasPrefix(value, "file://") {
		return Spec{Kind: "ddl", Value: strings.TrimPrefix(value, "file://")}
	}
	return Spec{Kind: "database", Value: value}
}

// LoadGraph loads either a live PostgreSQL database or a declarative DDL file
// into the sole internal schema representation: a typed graph snapshot.
func LoadGraph(ctx context.Context, spec Spec, devURL string, ignores []string) (*pgschema.Snapshot, error) {
	return loadGraph(ctx, spec, devURL, ignores, true)
}

// LoadGraphForComparison defers ignore-selector validation until both source
// snapshots have been read. Call ValidateIgnoreSelectors on their union.
func LoadGraphForComparison(ctx context.Context, spec Spec, devURL string, ignores []string) (*pgschema.Snapshot, error) {
	return loadGraph(ctx, spec, devURL, ignores, false)
}

// LoadDDLGraphForComparison materializes adapter-produced CREATE statements
// directly, avoiding an unreceipted intermediate file while retaining the
// same PostgreSQL catalog authority as a file:// source.
func LoadDDLGraphForComparison(ctx context.Context, ddl []byte, provenance, devURL string, ignores []string) (*pgschema.Snapshot, error) {
	if devURL == "" {
		return nil, fmt.Errorf("adapter DDL requires a dev database URL")
	}
	if strings.TrimSpace(provenance) == "" || strings.Contains(provenance, "://") {
		return nil, fmt.Errorf("adapter DDL provenance must be non-secret")
	}
	return materializeDDLBytesGraph(ctx, ddl, provenance, devURL, ignores, false)
}

func loadGraph(ctx context.Context, spec Spec, devURL string, ignores []string, validateIgnores bool) (*pgschema.Snapshot, error) {
	switch spec.Kind {
	case "database":
		config, err := pgx.ParseConfig(spec.Value)
		if err != nil {
			return nil, fmt.Errorf("parse database URL: %w", err)
		}
		return inspectGraphConfig(ctx, config, ignores, validateIgnores)
	case "ddl":
		if devURL == "" {
			return nil, fmt.Errorf("a file:// source requires --dev-url")
		}
		return materializeDDLGraph(ctx, spec.Value, devURL, ignores, validateIgnores)
	default:
		return nil, fmt.Errorf("unknown source kind %q", spec.Kind)
	}
}

// ValidateIgnoreSelectors rejects selectors unused by both compared sources.
func ValidateIgnoreSelectors(selectors []string, snapshots ...*pgschema.Snapshot) error {
	tracker, err := newIgnoreTracker(selectors)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		for _, actual := range snapshot.Ignored() {
			kind := strings.SplitN(actual, ":", 2)[0]
			for _, requested := range tracker.requested {
				if requested == actual || requested == kind+":*" {
					tracker.used[requested] = true
				}
			}
		}
	}
	return tracker.Validate()
}

func temporaryName() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return fmt.Sprintf("onwardpg_ddl_%x", bytes), nil
}

func quote(identifier string) string { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
