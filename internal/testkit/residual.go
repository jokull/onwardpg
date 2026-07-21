package testkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

type ResidualReport struct {
	Converged          bool
	DesiredFingerprint string
	ActualFingerprint  string
	Passes             int
}

func AssertZeroResidual(ctx context.Context, adminURL string, desiredDDL []byte, actualConfig *pgx.ConnConfig) (ResidualReport, error) {
	if actualConfig == nil {
		return ResidualReport{}, fmt.Errorf("actual database configuration is required")
	}
	var report ResidualReport
	options := graphplan.Options{}
	for pass := 1; pass <= 2; pass++ {
		desired, err := source.LoadDDLGraphForComparison(ctx, desiredDDL, "acceptance-desired.sql", adminURL, nil)
		if err != nil {
			return report, fmt.Errorf("materialize desired graph on pass %d: %w", pass, err)
		}
		actual, err := source.LoadDatabaseGraphForComparison(ctx, actualConfig.Copy(), nil)
		if err != nil {
			return report, fmt.Errorf("inspect workload graph on pass %d: %w", pass, err)
		}
		if unsupported := append(desired.Unsupported(), actual.Unsupported()...); len(unsupported) != 0 {
			return report, fmt.Errorf("zero-drift oracle encountered unsupported objects on pass %d: %v", pass, unsupported)
		}
		if ignored := append(desired.Ignored(), actual.Ignored()...); len(ignored) != 0 {
			return report, fmt.Errorf("zero-drift oracle ignored differences on pass %d: %v", pass, ignored)
		}
		residual, err := graphplan.Build(actual, desired, protocol.Answers{}, options)
		if err != nil {
			return report, fmt.Errorf("build residual on pass %d: %w", pass, err)
		}
		if residual.Status != protocol.Planned || len(residual.Statements) != 0 || len(residual.Batches) != 0 || len(residual.Questions) != 0 || len(residual.Operations) != 0 || len(residual.Reconciliations) != 0 || len(residual.Guidance) != 0 || len(residual.Analysis) != 0 || len(residual.Unsupported) != 0 || len(residual.Ignored) != 0 || len(residual.Compatibility) != 0 || len(residual.Preserved) != 0 {
			return report, fmt.Errorf("nonempty semantic residual on pass %d: status=%s statements=%d batches=%d questions=%d operations=%d reconciliations=%d guidance=%d analysis=%d unsupported=%v ignored=%v compatibility=%v preserved=%v", pass, residual.Status, len(residual.Statements), len(residual.Batches), len(residual.Questions), len(residual.Operations), len(residual.Reconciliations), len(residual.Guidance), len(residual.Analysis), residual.Unsupported, residual.Ignored, residual.Compatibility, residual.Preserved)
		}
		equivalent, err := graphplan.Equivalent(actual, desired, options)
		if err != nil {
			return report, fmt.Errorf("compare fingerprints on pass %d: %w", pass, err)
		}
		actualFingerprint, err := graphplan.Fingerprint(actual, options)
		if err != nil {
			return report, err
		}
		desiredFingerprint, err := graphplan.Fingerprint(desired, options)
		if err != nil {
			return report, err
		}
		if !equivalent || actualFingerprint != desiredFingerprint {
			return report, fmt.Errorf("fingerprints differ on pass %d: actual=%s desired=%s", pass, actualFingerprint, desiredFingerprint)
		}
		if pass > 1 && (report.ActualFingerprint != actualFingerprint || report.DesiredFingerprint != desiredFingerprint) {
			return report, fmt.Errorf("zero-drift fingerprints changed between passes")
		}
		report = ResidualReport{Converged: true, DesiredFingerprint: desiredFingerprint, ActualFingerprint: actualFingerprint, Passes: pass}
	}
	return report, nil
}

func AssertNoCompatibilityArtifacts(ctx context.Context, connection *pgx.Conn, namePatterns ...string) error {
	for _, pattern := range namePatterns {
		var count int
		err := connection.QueryRow(ctx, `
SELECT count(*)
FROM (
  SELECT n.nspname, c.relname AS name
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  UNION ALL
  SELECT n.nspname, p.proname AS name
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  UNION ALL
  SELECT n.nspname, t.tgname AS name
  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
) objects
WHERE nspname NOT IN ('pg_catalog', 'information_schema') AND name LIKE $1`, pattern).Scan(&count)
		if err != nil {
			return fmt.Errorf("inventory compatibility artifacts matching %q: %w", pattern, err)
		}
		if count != 0 {
			return fmt.Errorf("%d compatibility artifacts remain matching %q", count, pattern)
		}
	}
	return nil
}
