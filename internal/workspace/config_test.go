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
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
`
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	target := config.Targets["primary-postgres"]
	if len(target.SchemaCommand) == 0 || target.ScratchEnv() != "ONWARDPG_SCRATCH_DATABASE_URL" {
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

func TestRequireUnchangedReloadsCompleteConfiguration(t *testing.T) {
	directory := t.TempDir()
	name := filepath.Join(directory, ".onwardpg.toml")
	first := `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "DEV_DATABASE_URL"
`
	if err := os.WriteFile(name, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireUnchanged(name, expected); err != nil {
		t.Fatal(err)
	}
	second := strings.Replace(first, `schema_file = "schema.sql"`, `schema_file = "next.sql"`, 1)
	if err := os.WriteFile(name, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireUnchanged(name, expected); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("changed configuration was accepted: %v", err)
	}
}

func TestLoadRejectsUnknownFieldsAndUnsafeValues(t *testing.T) {
	tests := map[string]string{
		"adapter-surface": `version = 1
bundle_root = "onward-bundles"
[targets.db]
adapter = "ddl"
schema_file = "schema.sql"
dev_database_env = "DEV_DATABASE_URL"
`,
		"unknown": `version = 1
bundle_root = "onward-bundles"
surprise = true
[targets.db]
schema_file = "schema.sql"
dev_database_env = "DEV_DATABASE_URL"
`,
		"escape": `version = 1
bundle_root = "../outside"
[targets.db]
schema_file = "schema.sql"
dev_database_env = "DEV_DATABASE_URL"
`,
		"secret": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_file = "schema.sql"
dev_database_env = "postgres://secret@localhost/db"
`,
		"scratch-secret": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_file = "schema.sql"
dev_database_env = "DEV_DATABASE_URL"
scratch_database_env = "postgres://secret@localhost/db"
`,
		"ambiguous-source": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_file = "schema.sql"
schema_command = ["pnpm", "schema"]
dev_database_env = "DEV_DATABASE_URL"
`,
		"command-secret": `version = 1
bundle_root = "onward-bundles"
[targets.db]
schema_command = ["tool", "--database", "postgres://secret@localhost/db"]
dev_database_env = "DEV_DATABASE_URL"
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

func TestTargetScratchEnvFallsBackToDevelopmentEnvironment(t *testing.T) {
	target := Target{DevDatabaseEnv: "DEV_DATABASE_URL"}
	if got := target.ScratchEnv(); got != "DEV_DATABASE_URL" {
		t.Fatalf("ScratchEnv = %q", got)
	}
	target.ScratchDatabaseEnv = "SCRATCH_DATABASE_URL"
	if got := target.ScratchEnv(); got != "SCRATCH_DATABASE_URL" {
		t.Fatalf("ScratchEnv = %q", got)
	}
}

func TestConfigRejectsSchemaInsideBundleHistory(t *testing.T) {
	config := Config{
		Version: ConfigVersion, BundleRoot: "migrations/onward",
		Targets: map[string]Target{"db": {SchemaFile: "migrations/onward/schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL"}},
	}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("expected path overlap rejection, got %v", err)
	}
}

func TestConfigRejectsUnsafePaths(t *testing.T) {
	base := Target{SchemaFile: "onward-bundles/schema.sql", DevDatabaseEnv: "DEV_DATABASE_URL"}
	config := Config{Version: ConfigVersion, BundleRoot: "onward-bundles", Targets: map[string]Target{"db": base}}
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
