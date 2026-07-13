package workspace

import (
	"crypto/sha256"
	"encoding/hex"
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
	return loadSortedSQLHistory(root, target.MigrationPath)
}

func loadSortedSQLHistory(root, directory string) (MigrationHistory, error) {
	files, err := sqlFiles(root, directory)
	if err != nil {
		return MigrationHistory{}, err
	}
	if err := validateNumericPrefixWidths(files); err != nil {
		return MigrationHistory{}, err
	}
	sort.Strings(files)
	return concatenateHistory(root, files, "sql:"+directory)
}

func validateNumericPrefixWidths(files []string) error {
	width := 0
	for _, name := range files {
		base := filepath.Base(name)
		prefixWidth := 0
		for prefixWidth < len(base) && base[prefixWidth] >= '0' && base[prefixWidth] <= '9' {
			prefixWidth++
		}
		if prefixWidth == 0 {
			continue
		}
		if width == 0 {
			width = prefixWidth
			continue
		}
		if prefixWidth != width {
			return fmt.Errorf("SQL migration numeric prefixes must have a consistent zero-padded width: %s has width %d, want %d", name, prefixWidth, width)
		}
	}
	return nil
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
		writeDigestFrame(hash, []byte(name))
		writeDigestFrame(hash, data)
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
