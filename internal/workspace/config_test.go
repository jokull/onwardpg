package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStrictConfigAndResolveBundlePath(t *testing.T) {
	directory := t.TempDir()
	name := filepath.Join(directory, ".onwardpg.toml")
	data := `version = 1
bundle_root = "onward-bundles"

[targets.primary-postgres]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
migration_path = "packages/db/migrations"
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
postgres_major = 16
`
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Targets["primary-postgres"].SchemaCommand) == 0 {
		t.Fatalf("config = %#v", config)
	}
	path, err := config.BundlePath("/repo", "primary-postgres", "customer-profile")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join("/repo", "onward-bundles", "primary-postgres", "customer-profile") {
		t.Fatalf("bundle path = %q", path)
	}
}

func TestLoadRejectsUnknownFieldsAndUnsafeValues(t *testing.T) {
	tests := map[string]string{
		"adapter-surface": `version = 1
bundle_root = "onward-bundles"
[targets.db]
adapter = "ddl"
schema_file = "schema.sql"
migration_path = "migrations"
dev_database_env = "DEV_DATABASE_URL"
postgres_major = 16
`,
		"unknown": `version = 1
bundle_root = "onward-bundles"
surprise = true
[targets.db]
schema_file = "schema.sql"
migration_path = "migrations"
dev_database_env = "DEV_DATABASE_URL"
postgres_major = 16
`,
		"escape": `version = 1
bundle_root = "../outside"
[targets.db]
schema_file = "schema.sql"
migration_path = "migrations"
dev_database_env = "DEV_DATABASE_URL"
postgres_major = 16
`,
		"secret": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_file = "schema.sql"
migration_path = "migrations"
dev_database_env = "postgres://secret@localhost/db"
postgres_major = 16
`,
		"ambiguous-source": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_file = "schema.sql"
schema_command = ["pnpm", "schema"]
migration_path = "migrations"
dev_database_env = "DEV_DATABASE_URL"
postgres_major = 16
`,
		"command-secret": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_command = ["tool", "--database", "postgres://secret@localhost/db"]
migration_path = "migrations"
dev_database_env = "DEV_DATABASE_URL"
postgres_major = 16
`,
	}
	for label, data := range tests {
		t.Run(label, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), ".onwardpg.toml")
			if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(name); err == nil {
				t.Fatal("expected invalid config to fail")
			}
		})
	}
}

func TestConfigRejectsOverlappingBundleAndMigrationPaths(t *testing.T) {
	config := Config{
		Version: ConfigVersion, BundleRoot: "migrations/onward",
		Targets: map[string]Target{"db": {SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}},
	}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("expected path overlap rejection, got %v", err)
	}
}

func TestConfigRejectsAmbiguousTargetPaths(t *testing.T) {
	base := Target{SchemaFile: "schema.sql", MigrationPath: "migrations", DevDatabaseEnv: "DEV_DATABASE_URL", PostgresMajor: 16}
	config := Config{Version: ConfigVersion, BundleRoot: "onward-bundles", Targets: map[string]Target{"first": base, "second": base}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "share migration_path") {
		t.Fatalf("expected shared migration path rejection, got %v", err)
	}
	base.SchemaFile = "MIGRATIONS/schema.sql"
	config.Targets = map[string]Target{"db": base}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "schema_file") {
		t.Fatalf("expected schema-in-history rejection, got %v", err)
	}
	base.SchemaFile = "schema.sql"
	config.Targets = map[string]Target{"db": base}
	config.BundleRoot = ".."
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "remain within") {
		t.Fatalf("expected parent bundle root rejection, got %v", err)
	}
}
