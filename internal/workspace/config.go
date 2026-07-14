// Package workspace owns repository-level onwardpg configuration. It keeps
// schema-export commands, migration paths, and policy separate from the typed
// PostgreSQL planner.
package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const ConfigVersion = 1

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

type Config struct {
	Version    int               `toml:"version" json:"version"`
	BundleRoot string            `toml:"bundle_root" json:"bundle_root"`
	Targets    map[string]Target `toml:"targets" json:"targets"`
}

type Target struct {
	SchemaFile         string   `toml:"schema_file" json:"schema_file,omitempty"`
	SchemaCommand      []string `toml:"schema_command" json:"schema_command,omitempty"`
	DevDatabaseEnv     string   `toml:"dev_database_env" json:"dev_database_env"`
	ScratchDatabaseEnv string   `toml:"scratch_database_env" json:"scratch_database_env,omitempty"`
	DevMode            string   `toml:"dev_mode" json:"dev_mode,omitempty"`
	Ignore             []string `toml:"ignore" json:"ignore,omitempty"`
}

func Load(name string) (Config, error) {
	file, err := os.Open(name)
	if err != nil {
		return Config{}, fmt.Errorf("open onwardpg config: %w", err)
	}
	defer file.Close()
	limited := io.LimitReader(file, 1<<20)
	var config Config
	if err := toml.NewDecoder(limited).DisallowUnknownFields().Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode onwardpg config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// RequireUnchanged reloads the complete strict repository configuration at a
// lifecycle commit point. A caller must not write using paths or schema source
// settings captured before a concurrent config edit.
func RequireUnchanged(name string, expected Config) error {
	current, err := Load(name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("repository configuration changed since the command started")
	}
	return nil
}

func (c Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("config version is %d, want %d", c.Version, ConfigVersion)
	}
	if c.BundleRoot == "" {
		return fmt.Errorf("bundle_root is required")
	}
	if err := validateRepositoryPath(c.BundleRoot); err != nil {
		return fmt.Errorf("bundle_root: %w", err)
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	for name, target := range c.Targets {
		if !safeName(name) {
			return fmt.Errorf("target name %q is invalid", name)
		}
		if err := target.Validate(); err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		if target.SchemaFile != "" && pathsOverlap(target.SchemaFile, c.BundleRoot) {
			return fmt.Errorf("target %s: schema_file and bundle_root must not overlap", name)
		}
	}
	return nil
}

func pathsOverlap(first, second string) bool {
	first = strings.ToLower(filepath.ToSlash(filepath.Clean(first)))
	second = strings.ToLower(filepath.ToSlash(filepath.Clean(second)))
	return first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/")
}

func (t Target) Validate() error {
	hasFile, hasCommand := t.SchemaFile != "", len(t.SchemaCommand) > 0
	if hasFile == hasCommand {
		return fmt.Errorf("exactly one of schema_file or schema_command is required")
	}
	if hasFile {
		if err := validateRepositoryPath(t.SchemaFile); err != nil {
			return fmt.Errorf("schema_file: %w", err)
		}
	}
	if hasCommand {
		for _, argument := range t.SchemaCommand {
			if strings.TrimSpace(argument) == "" || strings.ContainsRune(argument, '\x00') || strings.Contains(argument, "://") {
				return fmt.Errorf("schema_command contains an empty or invalid argument")
			}
		}
	}
	if !envNamePattern.MatchString(t.DevDatabaseEnv) {
		return fmt.Errorf("dev_database_env must name an environment variable, not contain a URL")
	}
	if t.ScratchDatabaseEnv != "" && !envNamePattern.MatchString(t.ScratchDatabaseEnv) {
		return fmt.Errorf("scratch_database_env must name an environment variable, not contain a URL")
	}
	if t.DevMode != "" && t.DevMode != "workspace" && t.DevMode != "strict" {
		return fmt.Errorf("dev_mode must be workspace or strict")
	}
	for _, selector := range t.Ignore {
		kind, name, found := strings.Cut(selector, ":")
		if !found || kind == "" || name == "" || strings.Contains(name, "*") && name != "*" || strings.TrimSpace(selector) != selector {
			return fmt.Errorf("invalid ignore selector %q; expected kind:name or kind:*", selector)
		}
	}
	return nil
}

// WorkspaceMode is deliberately the default: long-lived developer databases
// may retain objects from other branches, so absence from exported DDL is not
// enough authority to drop them.
func (t Target) WorkspaceMode() bool { return t.DevMode == "" || t.DevMode == "workspace" }

// ScratchEnv returns the environment variable containing the disposable
// PostgreSQL administrative URL. Falling back to dev_database_env preserves
// existing preview configuration while new repositories can keep the read-only
// development catalog separate from CREATE DATABASE authority.
func (t Target) ScratchEnv() string {
	if t.ScratchDatabaseEnv != "" {
		return t.ScratchDatabaseEnv
	}
	return t.DevDatabaseEnv
}

func (c Config) Target(name string) (Target, error) {
	target, ok := c.Targets[name]
	if !ok {
		return Target{}, fmt.Errorf("target %q is not configured", name)
	}
	return target, nil
}

func (c Config) BundlePath(repositoryRoot, target, bundleID string) (string, error) {
	if _, err := c.Target(target); err != nil {
		return "", err
	}
	if !safeName(bundleID) {
		return "", fmt.Errorf("bundle id %q is invalid", bundleID)
	}
	return filepath.Join(repositoryRoot, filepath.FromSlash(c.BundleRoot), target, bundleID), nil
}

func validateRepositoryPath(value string) error {
	if value == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') || strings.Contains(value, `\`) {
		return fmt.Errorf("path must be a slash-separated repository-relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path must be normalized and remain within the repository")
	}
	return nil
}

func safeName(value string) bool {
	if value == "" || strings.Trim(value, ".") == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
