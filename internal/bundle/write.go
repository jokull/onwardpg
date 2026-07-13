package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
)

type WriteOptions struct {
	ReplaceDraft   bool
	PreserveEdited []string
}

// NextCoordinates returns deterministic generation and decision-attempt
// numbers for a bundle path. Replanning the same source contract stays in the
// same generation, and an identical decision result reuses its attempt path.
// A changed source/planner contract advances the generation.
func NextCoordinates(destination string, metadata Metadata, result protocol.Result) (generation, attempt int, err error) {
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
	if !samePlanningContract(manifest, metadata) {
		return manifest.Generation + 1, 1, nil
	}
	if result.Status == protocol.NeedsInput || result.Status == protocol.Unsupported {
		data, encodeErr := jsonDocument(result)
		if encodeErr != nil {
			return 0, 0, encodeErr
		}
		digest := Digest(data)
		for _, decision := range manifest.Decisions {
			if decision.Digest != digest {
				continue
			}
			var value int
			if _, scanErr := fmt.Sscanf(filepath.Base(decision.Path), "attempt-%03d.json", &value); scanErr == nil {
				return manifest.Generation, value, nil
			}
		}
	}
	return manifest.Generation, maxAttempt + 1, nil
}

func samePlanningContract(manifest Manifest, metadata Metadata) bool {
	return manifest.BundleID == metadata.BundleID &&
		manifest.Target == metadata.Target &&
		manifest.Purpose == metadata.Purpose &&
		sameSourcePlanningContract(manifest.BaselineSource, metadata.BaselineSource) &&
		sameSourcePlanningContract(manifest.DesiredSource, metadata.DesiredSource) &&
		reflect.DeepEqual(manifest.Planner, metadata.Planner) &&
		manifestHistoryParent(manifest) == metadata.HistoryParentDigest
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
	if len(options.PreserveEdited) > 0 && !options.ReplaceDraft {
		return fmt.Errorf("preserving edited files requires draft replacement")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create bundle parent: %w", err)
	}
	backup := destination + ".onwardpg-replaced"
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("bundle replacement backup exists at %s; recover or remove it before continuing", backup)
	} else if !os.IsNotExist(err) {
		return err
	}
	lock := destination + ".onwardpg-lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("bundle lifecycle operation is already in progress: %s", lock)
		}
		return fmt.Errorf("acquire bundle lifecycle lock: %w", err)
	}
	defer os.Remove(lock)
	var previous Artifact
	var replacing bool
	if _, err := os.Stat(destination); err == nil {
		replacing = true
		if !options.ReplaceDraft {
			return fmt.Errorf("bundle destination %s already exists and replacement was not authorized by the owning workflow", destination)
		}
		if err := validateReplaceableBundle(destination); err != nil {
			return err
		}
		previous, err = Read(destination)
		if err != nil {
			return err
		}
		if previous.Manifest.BundleID != artifact.Manifest.BundleID || previous.Manifest.Target != artifact.Manifest.Target {
			return fmt.Errorf("replacement bundle identity does not match the existing bundle")
		}
		switch artifact.Manifest.Generation {
		case previous.Manifest.Generation:
			if !sameManifestPlanningContract(previous.Manifest, artifact.Manifest) {
				return fmt.Errorf("same-generation replacement changed its source or planner contract")
			}
			artifact, err = preserveDecisionHistory(destination, artifact)
			if err != nil {
				return err
			}
		case previous.Manifest.Generation + 1:
		default:
			return fmt.Errorf("replacement generation must stay at %d or advance to %d", previous.Manifest.Generation, previous.Manifest.Generation+1)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	filesToWrite := artifact.Files
	if len(options.PreserveEdited) > 0 {
		filesToWrite = make(map[string][]byte, len(artifact.Files))
		for name, body := range artifact.Files {
			filesToWrite[name] = append([]byte(nil), body...)
		}
		seen := make(map[string]bool, len(options.PreserveEdited))
		for _, name := range options.PreserveEdited {
			if seen[name] {
				continue
			}
			seen[name] = true
			if name != "verify.sql" && !isEditablePhasePath(name) {
				return fmt.Errorf("conflict handoff path %q is not editable", name)
			}
			body, exists := previous.Files[name]
			if !exists {
				delete(filesToWrite, name)
				continue
			}
			filesToWrite[name] = append([]byte(nil), body...)
		}
	}
	temporary, err := os.MkdirTemp(parent, ".onwardpg-bundle-")
	if err != nil {
		return fmt.Errorf("create temporary bundle directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	for _, name := range SortedFiles(filesToWrite) {
		if err := validateArtifactPath(name); err != nil {
			return err
		}
		full := filepath.Join(temporary, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create artifact directory for %s: %w", name, err)
		}
		if err := writeFileDurable(full, filesToWrite[name], 0o644); err != nil {
			return fmt.Errorf("write artifact %s: %w", name, err)
		}
	}
	if err := syncDirectoryTree(temporary); err != nil {
		return fmt.Errorf("sync temporary bundle: %w", err)
	}
	if replacing {
		current, err := Read(destination)
		if err != nil {
			return fmt.Errorf("bundle changed before replacement: %w", err)
		}
		if Digest(current.Files["manifest.json"]) != Digest(previous.Files["manifest.json"]) {
			return fmt.Errorf("bundle changed after replacement was prepared")
		}
		if err := validateReplaceableBundle(destination); err != nil {
			return fmt.Errorf("bundle became immutable before replacement: %w", err)
		}
		if err := os.Rename(destination, backup); err != nil {
			return fmt.Errorf("preserve existing bundle for replacement: %w", err)
		}
		if err := os.Rename(temporary, destination); err != nil {
			_ = os.Rename(backup, destination)
			return fmt.Errorf("install replacement bundle: %w", err)
		}
		if err := syncDirectory(parent); err != nil {
			return fmt.Errorf("sync installed replacement: %w", err)
		}
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove replaced bundle backup: %w", err)
		}
		return nil
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync installed bundle: %w", err)
	}
	return nil
}

func writeFileDurable(name string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectoryTree(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func sameManifestPlanningContract(previous, next Manifest) bool {
	return previous.BundleID == next.BundleID && previous.Target == next.Target &&
		previous.Purpose == next.Purpose &&
		sameSourcePlanningContract(previous.BaselineSource, next.BaselineSource) &&
		sameSourcePlanningContract(previous.DesiredSource, next.DesiredSource) &&
		reflect.DeepEqual(previous.Planner, next.Planner) &&
		manifestHistoryParent(previous) == manifestHistoryParent(next)
}

func sameSourcePlanningContract(previous, next SourceReceipt) bool {
	return reflect.DeepEqual(previous, next)
}

func manifestHistoryParent(manifest Manifest) string {
	if manifest.History == nil {
		return ""
	}
	return manifest.History.ParentDigest
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
	if next.Manifest.History != nil {
		digest, err := HistoryEntryDigest(next.Manifest)
		if err != nil {
			return Artifact{}, err
		}
		next.Manifest.History.EntryDigest = digest
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
	artifact, err := Read(destination)
	if err != nil {
		return fmt.Errorf("refuse to replace invalid bundle: %w", err)
	}
	manifest := artifact.Manifest
	if manifest.State != "planned" && manifest.State != "needs_sql_edits" && manifest.State != "needs_input" && manifest.State != "unsupported" {
		return fmt.Errorf("bundle state %q is immutable", manifest.State)
	}
	return nil
}
