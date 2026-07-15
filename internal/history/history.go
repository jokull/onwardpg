// Package history discovers and validates the content-addressed onwardpg
// migration chain for one database target. Filesystem names locate bundles;
// parent and entry digests are the sole source of execution order.
package history

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
)

const Version = "onwardpg.history/v1"

const StatusVersion = "onwardpg.history-status/v2"

var phaseOrder = []string{"expand", "contract"}

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

type StatusEntry struct {
	BundleID     string `json:"bundle_id"`
	Generation   int    `json:"generation"`
	ParentDigest string `json:"parent_digest"`
	EntryDigest  string `json:"entry_digest"`
}

type SelectedStatus struct {
	BundleID     string `json:"bundle_id"`
	State        string `json:"state"`
	Generation   int    `json:"generation"`
	ParentDigest string `json:"parent_digest"`
	EntryDigest  string `json:"entry_digest"`
	Relationship string `json:"relationship"`
}

type StatusFinding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type StatusReport struct {
	ProtocolVersion string          `json:"protocol_version"`
	Status          string          `json:"status"`
	Target          string          `json:"target"`
	HistoryHead     string          `json:"history_head"`
	HeadBundle      string          `json:"head_bundle,omitempty"`
	HeadRef         string          `json:"head_ref,omitempty"`
	Entries         []StatusEntry   `json:"entries,omitempty"`
	Selected        *SelectedStatus `json:"selected,omitempty"`
	Findings        []StatusFinding `json:"findings,omitempty"`
}

// Inspect returns the repository-local hash-chain state without consulting
// Git or any database. When selectedBundle is the valid chain head, that one
// mutable bundle is excluded so the report describes the base it is actually
// stacked on and whether its receipted parent is current. Selecting an earlier
// entry in a valid chain is blocked explicitly: it is immutable history, not a
// mutable bundle whose successor may be ignored. Exclusion remains the
// recovery path when the complete history is invalid because the selected
// mutable bundle forms a stale fork.
func Inspect(root, bundleRoot, target, selectedBundle string) (StatusReport, error) {
	report := StatusReport{ProtocolVersion: StatusVersion, Status: "valid", Target: target}
	var (
		chain           Chain
		selected        *Entry
		selectedNotHead bool
		err             error
	)
	if selectedBundle == "" {
		chain, err = Load(root, bundleRoot, target)
	} else {
		fullChain, fullErr := Load(root, bundleRoot, target)
		if fullErr == nil {
			selectedIndex := -1
			for i := range fullChain.Entries {
				if fullChain.Entries[i].Directory == selectedBundle {
					selectedIndex = i
					break
				}
			}
			switch {
			case selectedIndex == -1:
				chain = fullChain
			case selectedIndex < len(fullChain.Entries)-1:
				chain = fullChain
				selected = &chain.Entries[selectedIndex]
				selectedNotHead = true
			default:
				chain, selected, err = LoadExcluding(root, bundleRoot, target, selectedBundle)
			}
		} else {
			chain, selected, err = LoadExcluding(root, bundleRoot, target, selectedBundle)
			if err != nil {
				chain, selected, err = LoadEditedDraft(root, bundleRoot, target, selectedBundle)
			}
		}
	}
	if err != nil {
		return report, err
	}
	report.HistoryHead = chain.HeadDigest
	for _, entry := range chain.Entries {
		receipt := entry.Artifact.Manifest.History
		report.Entries = append(report.Entries, StatusEntry{
			BundleID: entry.Directory, Generation: entry.Artifact.Manifest.Generation,
			ParentDigest: receipt.ParentDigest, EntryDigest: receipt.EntryDigest,
		})
	}
	if len(chain.Entries) > 0 {
		report.HeadBundle = chain.Entries[len(chain.Entries)-1].Directory
		report.HeadRef = HeadRef(chain)
	}
	if selectedBundle == "" {
		return report, nil
	}
	if selected == nil {
		report.Status = "missing"
		report.Findings = []StatusFinding{{
			Code: "selected_bundle_missing", Message: fmt.Sprintf("selected bundle %s does not exist", selectedBundle),
			Remediation: "if this is a new feature, pass this report's head_ref to draft --after and use --create once; otherwise restore the missing bundle from Git",
		}}
		return report, nil
	}
	receipt := selected.Artifact.Manifest.History
	if selectedNotHead {
		report.Status = "blocked"
		report.Findings = []StatusFinding{{
			Code:        "selected_bundle_not_head",
			Message:     fmt.Sprintf("selected bundle %s is not the target history head; current head is %s", selectedBundle, report.HeadBundle),
			Remediation: fmt.Sprintf("do not rewrite accepted history; choose a new feature bundle ID and create it after this report's head_ref for %s", report.HeadBundle),
		}}
		report.Selected = &SelectedStatus{
			BundleID: selectedBundle, State: selected.Artifact.Manifest.State,
			Generation: selected.Artifact.Manifest.Generation, ParentDigest: receipt.ParentDigest,
			EntryDigest: receipt.EntryDigest, Relationship: "not_head",
		}
		return report, nil
	}
	relationship := "current"
	if receipt.ParentDigest != chain.HeadDigest {
		relationship = "stale"
		report.Status = "stale"
		report.Findings = []StatusFinding{{
			Code:        "stale_history_parent",
			Message:     fmt.Sprintf("selected bundle parent %s is stale; current base head is %s", receipt.ParentDigest, chain.HeadDigest),
			Remediation: "rerun draft with the same bundle id and pass this report's exact head_ref to --after",
		}}
	}
	report.Selected = &SelectedStatus{
		BundleID: selectedBundle, State: selected.Artifact.Manifest.State,
		Generation: selected.Artifact.Manifest.Generation, ParentDigest: receipt.ParentDigest,
		EntryDigest: receipt.EntryDigest, Relationship: relationship,
	}
	return report, nil
}

// HeadRef binds the human-readable head bundle ID to its exact content
// digest. Agents pass this value back through draft --after so a same-named
// but rewritten base cannot be mistaken for the accepted history tip.
func HeadRef(chain Chain) string {
	if len(chain.Entries) == 0 || chain.HeadDigest == "" {
		return ""
	}
	return chain.Entries[len(chain.Entries)-1].Directory + "@" + chain.HeadDigest
}

func ParseHeadRef(value string) (bundleID, digest string, err error) {
	bundleID, digest, found := strings.Cut(value, "@")
	if !found || !safeName(bundleID) || !strings.HasPrefix(digest, "sha256:") {
		return "", "", fmt.Errorf("history head reference %q must be <bundle>@sha256:<64 lowercase hex characters>", value)
	}
	encoded := strings.TrimPrefix(digest, "sha256:")
	decoded, decodeErr := hex.DecodeString(encoded)
	if decodeErr != nil || len(decoded) != 32 || encoded != strings.ToLower(encoded) {
		return "", "", fmt.Errorf("history head reference %q must be <bundle>@sha256:<64 lowercase hex characters>", value)
	}
	return bundleID, digest, nil
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
	chain, _, err := load(root, bundleRoot, target, "", false)
	return chain, err
}

// LoadExcluding validates target history while omitting one explicitly selected
// bundle from the returned chain. This is the Git-free draft boundary: the
// selected bundle is the only mutable entry, while every other directory must
// still form one complete hash chain. The excluded artifact is returned so a
// caller can preserve valid decisions or diagnose a stale parent.
func LoadExcluding(root, bundleRoot, target, bundleID string) (Chain, *Entry, error) {
	if !safeName(bundleID) {
		return Chain{}, nil, fmt.Errorf("bundle id %q is invalid", bundleID)
	}
	return load(root, bundleRoot, target, bundleID, false)
}

// LoadEditedDraft is the receipt-refresh path for one selected bundle. Base
// history stays strict, while the selected artifact is prepared from
// developer-owned phase SQL without writing its refreshed manifest.
func LoadEditedDraft(root, bundleRoot, target, bundleID string) (Chain, *Entry, error) {
	if !safeName(bundleID) {
		return Chain{}, nil, fmt.Errorf("bundle id %q is invalid", bundleID)
	}
	return load(root, bundleRoot, target, bundleID, true)
}

func load(root, bundleRoot, target, excludedBundleID string, editedCandidate bool) (Chain, *Entry, error) {
	chain := Chain{Target: target, RootDigest: bundle.HistoryRootDigest(), HeadDigest: bundle.HistoryRootDigest()}
	if root == "" || !filepath.IsAbs(root) {
		return Chain{}, nil, fmt.Errorf("history root must be absolute")
	}
	if err := validateRelativePath(bundleRoot); err != nil {
		return Chain{}, nil, fmt.Errorf("bundle root: %w", err)
	}
	if !safeName(target) {
		return Chain{}, nil, fmt.Errorf("history target %q is invalid", target)
	}
	base := filepath.Join(root, filepath.FromSlash(bundleRoot), target)
	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		return chain, nil, nil
	}
	if err != nil {
		return Chain{}, nil, fmt.Errorf("inspect target history: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Chain{}, nil, fmt.Errorf("target history must be a real directory")
	}
	directories, err := os.ReadDir(base)
	if err != nil {
		return Chain{}, nil, fmt.Errorf("read target history: %w", err)
	}

	byDigest := make(map[string]Entry, len(directories))
	children := make(map[string][]string, len(directories))
	var excluded *Entry
	for _, directory := range directories {
		if directory.Type()&os.ModeSymlink != 0 || !directory.IsDir() {
			return Chain{}, nil, fmt.Errorf("history contains unexpected entry %s", directory.Name())
		}
		name := directory.Name()
		var artifact bundle.Artifact
		var err error
		if name == excludedBundleID && editedCandidate {
			artifact, err = bundle.PrepareEdited(filepath.Join(base, name))
		} else {
			artifact, err = bundle.Read(filepath.Join(base, name))
		}
		if err != nil {
			return Chain{}, nil, fmt.Errorf("read history bundle %s: %w", name, err)
		}
		manifest := artifact.Manifest
		if manifest.BundleID != name {
			return Chain{}, nil, fmt.Errorf("history directory %s contains bundle_id %s", name, manifest.BundleID)
		}
		if manifest.Target != target {
			return Chain{}, nil, fmt.Errorf("history bundle %s targets %s, want %s", name, manifest.Target, target)
		}
		entry := Entry{Directory: name, Artifact: artifact}
		if name == excludedBundleID {
			if manifest.History == nil {
				return Chain{}, nil, fmt.Errorf("selected bundle %s has no hash-chain receipt", name)
			}
			excluded = &entry
			continue
		}
		if manifest.State != "planned" {
			return Chain{}, nil, fmt.Errorf("history bundle %s is %s, want planned", name, manifest.State)
		}
		if manifest.History == nil {
			return Chain{}, nil, fmt.Errorf("history bundle %s has no hash-chain receipt", name)
		}
		entryDigest := manifest.History.EntryDigest
		if _, exists := byDigest[entryDigest]; exists {
			return Chain{}, nil, fmt.Errorf("history entry digest %s is duplicated", entryDigest)
		}
		byDigest[entryDigest] = entry
		parent := manifest.History.ParentDigest
		children[parent] = append(children[parent], entryDigest)
	}

	for _, entry := range byDigest {
		parent := entry.Artifact.Manifest.History.ParentDigest
		if parent != bundle.HistoryRootDigest() {
			if _, exists := byDigest[parent]; !exists {
				return Chain{}, nil, fmt.Errorf("history bundle %s has missing parent %s", entry.Directory, parent)
			}
		}
		if len(children[parent]) > 1 {
			return Chain{}, nil, fmt.Errorf("history fork at parent %s", parent)
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
			return Chain{}, nil, fmt.Errorf("history cycle at entry %s", digest)
		}
		seen[digest] = true
		chain.Entries = append(chain.Entries, byDigest[digest])
		current = digest
	}
	if len(seen) != len(byDigest) {
		return Chain{}, nil, fmt.Errorf("history contains entries disconnected from root")
	}
	chain.HeadDigest = current
	return chain, excluded, nil
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
