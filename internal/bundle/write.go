package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type WriteOptions struct {
	ReplaceDraft bool
}

// NextCoordinates returns deterministic generation and decision-attempt
// numbers for a bundle path. A missing path starts at generation/attempt 1.
func NextCoordinates(destination string) (generation, attempt int, err error) {
	artifact, readErr := Read(destination)
	if os.IsNotExist(readErr) {
		return 1, 1, nil
	}
	if readErr != nil {
		return 0, 0, fmt.Errorf("read existing bundle: %w", readErr)
	}
	manifest := artifact.Manifest
	maxAttempt := 0
	for _, decision := range manifest.Decisions {
		var value int
		if _, err := fmt.Sscanf(filepath.Base(decision.Path), "attempt-%03d.json", &value); err == nil && value > maxAttempt {
			maxAttempt = value
		}
	}
	return manifest.Generation + 1, maxAttempt + 1, nil
}

// Read loads and verifies every file in an existing bundle. It is deliberately
// strict: extra files, missing receipts, digest drift, and symlinks all make a
// directory ineligible for lifecycle operations such as draft replacement.
func Read(directory string) (Artifact, error) {
	manifestData, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return Artifact{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return Artifact{}, fmt.Errorf("decode bundle manifest: %w", err)
	}
	files := make(map[string][]byte)
	err = filepath.WalkDir(directory, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == directory {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle contains symlink %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if err := validateArtifactPath(relative); err != nil {
			return err
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[relative] = body
		return nil
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("read bundle files: %w", err)
	}
	artifact := Artifact{Manifest: manifest, Files: files}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate bundle: %w", err)
	}
	return artifact, nil
}

// Write stores a complete artifact directory using a sibling temporary
// directory and rename. Existing directories are replaced only when they are
// valid, unexecuted bundle drafts and the caller explicitly opts in.
func Write(destination string, artifact Artifact, options WriteOptions) error {
	if destination == "" {
		return fmt.Errorf("bundle destination is required")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create bundle parent: %w", err)
	}
	if _, err := os.Stat(destination); err == nil {
		if !options.ReplaceDraft {
			return fmt.Errorf("bundle destination %s already exists; use explicit draft replacement", destination)
		}
		if err := validateReplaceableBundle(destination); err != nil {
			return err
		}
		artifact, err = preserveDecisionHistory(destination, artifact)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".onwardpg-bundle-")
	if err != nil {
		return fmt.Errorf("create temporary bundle directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	for _, name := range SortedFiles(artifact.Files) {
		if err := validateArtifactPath(name); err != nil {
			return err
		}
		full := filepath.Join(temporary, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create artifact directory for %s: %w", name, err)
		}
		if err := os.WriteFile(full, artifact.Files[name], 0o644); err != nil {
			return fmt.Errorf("write artifact %s: %w", name, err)
		}
	}
	if _, err := os.Stat(destination); err == nil {
		backup := destination + ".onwardpg-replaced"
		if _, err := os.Stat(backup); err == nil {
			return fmt.Errorf("bundle replacement backup already exists: %s", backup)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(destination, backup); err != nil {
			return fmt.Errorf("preserve existing bundle for replacement: %w", err)
		}
		if err := os.Rename(temporary, destination); err != nil {
			_ = os.Rename(backup, destination)
			return fmt.Errorf("install replacement bundle: %w", err)
		}
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove replaced bundle backup: %w", err)
		}
		return nil
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}
	return nil
}

func preserveDecisionHistory(destination string, next Artifact) (Artifact, error) {
	previousArtifact, err := Read(destination)
	if err != nil {
		return Artifact{}, err
	}
	previous := previousArtifact.Manifest
	byPath := make(map[string]FileReceipt, len(previous.Decisions)+len(next.Manifest.Decisions))
	for _, decision := range previous.Decisions {
		body := previousArtifact.Files[decision.Path]
		next.Files[decision.Path] = body
		byPath[decision.Path] = decision
	}
	for _, decision := range next.Manifest.Decisions {
		if existing, exists := byPath[decision.Path]; exists && existing.Digest != decision.Digest {
			return Artifact{}, fmt.Errorf("decision attempt path %s already records a different result", decision.Path)
		}
		byPath[decision.Path] = decision
	}
	next.Manifest.Decisions = next.Manifest.Decisions[:0]
	for _, name := range sortedReceiptPaths(byPath) {
		next.Manifest.Decisions = append(next.Manifest.Decisions, byPath[name])
	}
	manifestBytes, err := jsonDocument(next.Manifest)
	if err != nil {
		return Artifact{}, err
	}
	next.Files["manifest.json"] = manifestBytes
	return next, nil
}

func sortedReceiptPaths(receipts map[string]FileReceipt) []string {
	names := make([]string, 0, len(receipts))
	for name := range receipts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateArtifactPath(name string) error {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("invalid bundle artifact path %q", name)
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean != name || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("invalid bundle artifact path %q", name)
	}
	return nil
}

func validateReplaceableBundle(destination string) error {
	if _, err := os.Stat(filepath.Join(destination, "execution.json")); err == nil {
		return fmt.Errorf("bundle has an execution receipt and is immutable")
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(filepath.Join(destination, "verification", "execution.json")); err == nil {
		return fmt.Errorf("bundle has an execution receipt and is immutable")
	} else if !os.IsNotExist(err) {
		return err
	}
	artifact, err := Read(destination)
	if err != nil {
		return fmt.Errorf("refuse to replace invalid bundle: %w", err)
	}
	manifest := artifact.Manifest
	if manifest.State != "planned" && manifest.State != "needs_input" && manifest.State != "unsupported" {
		return fmt.Errorf("bundle state %q is immutable", manifest.State)
	}
	return nil
}
