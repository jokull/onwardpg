package bundle

import (
	"encoding/json"
	"fmt"
)

// WithExpandCheckpoint returns a new immutable artifact whose hash chain
// receipts the graph observed after expand in disposable PostgreSQL.
func WithExpandCheckpoint(artifact Artifact, expandFingerprint string) (Artifact, error) {
	if artifact.Manifest.State != "planned" {
		return Artifact{}, fmt.Errorf("expand checkpoint requires a planned bundle")
	}
	if !fingerprintPattern.MatchString(expandFingerprint) {
		return Artifact{}, fmt.Errorf("expand fingerprint %q is invalid", expandFingerprint)
	}
	checkpoint := CatalogCheckpoint{
		BundleID: artifact.Manifest.BundleID, PlanID: artifact.Manifest.PlanID, Generation: artifact.Manifest.Generation,
		BaselineFingerprint: artifact.Manifest.BaselineSource.Fingerprint,
		ExpandFingerprint:   expandFingerprint,
		DesiredFingerprint:  artifact.Manifest.DesiredSource.Fingerprint,
	}
	body, err := jsonDocument(checkpoint)
	if err != nil {
		return Artifact{}, err
	}
	files := make(map[string][]byte, len(artifact.Files)+1)
	for name, value := range artifact.Files {
		files[name] = append([]byte(nil), value...)
	}
	files["expand-checkpoint.json"] = body
	manifest := artifact.Manifest
	manifest.ExpandCheckpointDigest = Digest(body)
	if manifest.History != nil {
		digest, err := HistoryEntryDigest(manifest)
		if err != nil {
			return Artifact{}, err
		}
		manifest.History.EntryDigest = digest
	}
	manifestBody, err := jsonDocument(manifest)
	if err != nil {
		return Artifact{}, err
	}
	files["manifest.json"] = manifestBody
	result := Artifact{Manifest: manifest, Files: files}
	if err := result.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate expand-checkpoint receipt: %w", err)
	}
	return result, nil
}

func ReadCatalogCheckpoint(artifact Artifact) (CatalogCheckpoint, error) {
	var checkpoint CatalogCheckpoint
	body, exists := artifact.Files["expand-checkpoint.json"]
	if !exists || artifact.Manifest.ExpandCheckpointDigest == "" {
		return checkpoint, fmt.Errorf("bundle has no receipted expand checkpoint")
	}
	if Digest(body) != artifact.Manifest.ExpandCheckpointDigest {
		return checkpoint, fmt.Errorf("expand checkpoint digest does not match manifest")
	}
	if err := json.Unmarshal(body, &checkpoint); err != nil {
		return checkpoint, fmt.Errorf("decode expand checkpoint: %w", err)
	}
	if !fingerprintPattern.MatchString(checkpoint.ExpandFingerprint) {
		return checkpoint, fmt.Errorf("expand checkpoint fingerprint is invalid")
	}
	return checkpoint, nil
}
