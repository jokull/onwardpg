package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type MigrationHistory struct {
	DDL        []byte
	Files      []string
	Digest     string
	Provenance string
}

func LoadMigrationHistory(root string, target Target) (MigrationHistory, error) {
	if root == "" || !filepath.IsAbs(root) {
		return MigrationHistory{}, fmt.Errorf("migration root must be absolute")
	}
	if err := target.Validate(); err != nil {
		return MigrationHistory{}, err
	}
	if target.Adapter == "drizzle" {
		return loadDrizzleHistory(root, target.MigrationPath)
	}
	return loadSortedSQLHistory(root, target.MigrationPath)
}

type drizzleJournal struct {
	Dialect string `json:"dialect"`
	Entries []struct {
		Index int    `json:"idx"`
		Tag   string `json:"tag"`
	} `json:"entries"`
}

func loadDrizzleHistory(root, directory string) (MigrationHistory, error) {
	journalName := filepath.Join(root, filepath.FromSlash(directory), "meta", "_journal.json")
	data, err := os.ReadFile(journalName)
	if err != nil {
		return MigrationHistory{}, fmt.Errorf("read Drizzle migration journal: %w", err)
	}
	var journal drizzleJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return MigrationHistory{}, fmt.Errorf("decode Drizzle migration journal: %w", err)
	}
	if journal.Dialect != "postgresql" {
		return MigrationHistory{}, fmt.Errorf("Drizzle journal dialect is %q, want postgresql", journal.Dialect)
	}
	seen := make(map[string]bool, len(journal.Entries))
	files := make([]string, 0, len(journal.Entries))
	for position, entry := range journal.Entries {
		if entry.Index != position {
			return MigrationHistory{}, fmt.Errorf("Drizzle journal entry %q has idx %d, want %d", entry.Tag, entry.Index, position)
		}
		if !safeMigrationTag(entry.Tag) || seen[entry.Tag] {
			return MigrationHistory{}, fmt.Errorf("Drizzle journal contains invalid or duplicate tag %q", entry.Tag)
		}
		seen[entry.Tag] = true
		files = append(files, filepath.ToSlash(filepath.Join(directory, entry.Tag+".sql")))
	}
	actual, err := sqlFiles(root, directory)
	if err != nil {
		return MigrationHistory{}, err
	}
	expected := make(map[string]bool, len(files))
	for _, name := range files {
		expected[name] = true
	}
	for _, name := range actual {
		if !expected[name] {
			return MigrationHistory{}, fmt.Errorf("Drizzle SQL migration %s is not recorded in meta/_journal.json", name)
		}
	}
	if len(actual) != len(files) {
		return MigrationHistory{}, fmt.Errorf("Drizzle journal references %d SQL files but found %d", len(files), len(actual))
	}
	return concatenateHistory(root, files, "drizzle:"+directory)
}

func loadSortedSQLHistory(root, directory string) (MigrationHistory, error) {
	files, err := sqlFiles(root, directory)
	if err != nil {
		return MigrationHistory{}, err
	}
	sort.Strings(files)
	return concatenateHistory(root, files, "sql:"+directory)
}

func sqlFiles(root, directory string) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(directory))
	var files []string
	err := filepath.WalkDir(base, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("migration history contains symlink %s", name)
		}
		if strings.EqualFold(filepath.Ext(name), ".sql") {
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return files, err
}

func concatenateHistory(root string, files []string, provenance string) (MigrationHistory, error) {
	var ddl strings.Builder
	hash := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return MigrationHistory{}, fmt.Errorf("read migration %s: %w", name, err)
		}
		_, _ = hash.Write([]byte(name + "\x00"))
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
		ddl.WriteString("\n-- onwardpg migration: " + name + "\n")
		ddl.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			ddl.WriteByte('\n')
		}
	}
	return MigrationHistory{
		DDL: []byte(ddl.String()), Files: append([]string(nil), files...),
		Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Provenance: provenance,
	}, nil
}

func safeMigrationTag(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
