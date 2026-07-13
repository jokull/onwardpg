// Package history discovers and validates the content-addressed onwardpg
// migration chain for one database target. Filesystem names locate bundles;
// parent and entry digests are the sole source of execution order.
package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
)

const Version = "onwardpg.history/v1"

var phaseOrder = []string{"expand", "migrate", "manual", "contract"}

type Entry struct {
	Directory string
	Artifact  bundle.Artifact
}

type Chain struct {
	Target     string
	RootDigest string
	HeadDigest string
	Entries    []Entry
}

type Replay struct {
	DDL        []byte
	Files      []string
	Digest     string
	Provenance string
}

// Through returns the chain prefix ending at bundleID. It is used by
// verification so a historical bundle can be checked without applying later
// entries that happen to exist in the working tree.
func (c Chain) Through(bundleID string) (Chain, error) {
	if !safeName(bundleID) {
		return Chain{}, fmt.Errorf("bundle id %q is invalid", bundleID)
	}
	prefix := Chain{Target: c.Target, RootDigest: c.RootDigest, HeadDigest: c.RootDigest}
	for _, entry := range c.Entries {
		prefix.Entries = append(prefix.Entries, entry)
		prefix.HeadDigest = entry.Artifact.Manifest.History.EntryDigest
		if entry.Artifact.Manifest.BundleID == bundleID {
			return prefix, nil
		}
	}
	return Chain{}, fmt.Errorf("bundle %s is not in target history", bundleID)
}

// Load validates all bundle directories for target and orders them by their
// hash links. It rejects forks and disconnected entries instead of choosing a
// filename, timestamp, or directory order.
func Load(root, bundleRoot, target string) (Chain, error) {
	chain := Chain{Target: target, RootDigest: bundle.HistoryRootDigest(), HeadDigest: bundle.HistoryRootDigest()}
	if root == "" || !filepath.IsAbs(root) {
		return Chain{}, fmt.Errorf("history root must be absolute")
	}
	if err := validateRelativePath(bundleRoot); err != nil {
		return Chain{}, fmt.Errorf("bundle root: %w", err)
	}
	if !safeName(target) {
		return Chain{}, fmt.Errorf("history target %q is invalid", target)
	}
	base := filepath.Join(root, filepath.FromSlash(bundleRoot), target)
	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		return chain, nil
	}
	if err != nil {
		return Chain{}, fmt.Errorf("inspect target history: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Chain{}, fmt.Errorf("target history must be a real directory")
	}
	directories, err := os.ReadDir(base)
	if err != nil {
		return Chain{}, fmt.Errorf("read target history: %w", err)
	}

	byDigest := make(map[string]Entry, len(directories))
	children := make(map[string][]string, len(directories))
	for _, directory := range directories {
		if directory.Type()&os.ModeSymlink != 0 || !directory.IsDir() {
			return Chain{}, fmt.Errorf("history contains unexpected entry %s", directory.Name())
		}
		name := directory.Name()
		artifact, err := bundle.Read(filepath.Join(base, name))
		if err != nil {
			return Chain{}, fmt.Errorf("read history bundle %s: %w", name, err)
		}
		manifest := artifact.Manifest
		if manifest.BundleID != name {
			return Chain{}, fmt.Errorf("history directory %s contains bundle_id %s", name, manifest.BundleID)
		}
		if manifest.Target != target {
			return Chain{}, fmt.Errorf("history bundle %s targets %s, want %s", name, manifest.Target, target)
		}
		if manifest.State != "planned" {
			return Chain{}, fmt.Errorf("history bundle %s is %s, want planned", name, manifest.State)
		}
		if manifest.History == nil {
			return Chain{}, fmt.Errorf("history bundle %s has no hash-chain receipt", name)
		}
		entryDigest := manifest.History.EntryDigest
		if _, exists := byDigest[entryDigest]; exists {
			return Chain{}, fmt.Errorf("history entry digest %s is duplicated", entryDigest)
		}
		entry := Entry{Directory: name, Artifact: artifact}
		byDigest[entryDigest] = entry
		parent := manifest.History.ParentDigest
		children[parent] = append(children[parent], entryDigest)
	}

	for _, entry := range byDigest {
		parent := entry.Artifact.Manifest.History.ParentDigest
		if parent != bundle.HistoryRootDigest() {
			if _, exists := byDigest[parent]; !exists {
				return Chain{}, fmt.Errorf("history bundle %s has missing parent %s", entry.Directory, parent)
			}
		}
		if len(children[parent]) > 1 {
			return Chain{}, fmt.Errorf("history fork at parent %s", parent)
		}
	}

	current := bundle.HistoryRootDigest()
	seen := make(map[string]bool, len(byDigest))
	for {
		next := children[current]
		if len(next) == 0 {
			break
		}
		digest := next[0]
		if seen[digest] {
			return Chain{}, fmt.Errorf("history cycle at entry %s", digest)
		}
		seen[digest] = true
		chain.Entries = append(chain.Entries, byDigest[digest])
		current = digest
	}
	if len(seen) != len(byDigest) {
		return Chain{}, fmt.Errorf("history contains entries disconnected from root")
	}
	chain.HeadDigest = current
	return chain, nil
}

// Replay concatenates the already-validated phase artifacts in chain and
// lifecycle order. It does not execute them.
func (c Chain) Replay() (Replay, error) {
	var ddl strings.Builder
	var files []string
	for _, entry := range c.Entries {
		for _, phase := range phaseOrder {
			artifact, exists := entry.Artifact.Manifest.Phases[phase]
			if !exists {
				continue
			}
			body, exists := entry.Artifact.Files[artifact.Path]
			if !exists {
				return Replay{}, fmt.Errorf("history bundle %s is missing %s", entry.Directory, artifact.Path)
			}
			name := filepath.ToSlash(filepath.Join(entry.Directory, artifact.Path))
			files = append(files, name)
			ddl.WriteString("\n-- onwardpg history: " + name + "\n")
			ddl.Write(body)
			if len(body) == 0 || body[len(body)-1] != '\n' {
				ddl.WriteByte('\n')
			}
		}
	}
	return Replay{
		DDL: []byte(ddl.String()), Files: files, Digest: c.HeadDigest,
		Provenance: "onwardpg-history:" + c.Target + ":" + c.HeadDigest,
	}, nil
}

func validateRelativePath(name string) error {
	if name == "" || filepath.IsAbs(name) || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("path must be repository-relative")
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path must remain within the repository")
	}
	return nil
}

func safeName(value string) bool {
	if value == "" || strings.Trim(value, ".") == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
