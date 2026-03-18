package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ManifestSchema is the versioned identifier of the run-manifest contract. The
// manifest schema and the --output json shape are a STABLE PUBLIC CONTRACT: changing
// them requires a version bump and a note in SPEC.md.
const ManifestSchema = "gitdr.manifest/v2"

// Status values used in the manifest.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusSkipped = "skipped" // already backed up for the run date (resume)
)

// Manifest is the signed record of one backup run.
type Manifest struct {
	Schema      string      `json:"schema"`
	RunID       string      `json:"runId"`
	Tool        ToolInfo    `json:"tool"`
	Source      SourceInfo  `json:"source"`
	Destination DestInfo    `json:"destination"`
	StartedAt   time.Time   `json:"startedAt"`
	FinishedAt  time.Time   `json:"finishedAt"`
	Status      string      `json:"status"` // success | failed
	Repos       []RepoEntry `json:"repos"`
}

// ToolInfo identifies the producer.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SourceInfo identifies where the data came from.
type SourceInfo struct {
	Type string `json:"type"`
	Host string `json:"host"`
}

// DestInfo identifies where the data went and the immutability observed at write time.
// wormImmutable records whether the WORM check confirmed the destination immutable,
// the signed, tamper-evident answer to "was this backup on WORM storage?" (v2).
type DestInfo struct {
	Type          string `json:"type"`
	Bucket        string `json:"bucket"`
	WormMode      string `json:"wormMode,omitempty"`    // configured retention mode
	WormImmutable bool   `json:"wormImmutable"`         // WORM check confirmed immutable
	WormDetails   string `json:"wormDetails,omitempty"` // observed immutability detail
}

// RepoEntry is the per-repository outcome.
type RepoEntry struct {
	Slug      string         `json:"slug"`
	Status    string         `json:"status"` // success | failed
	Error     string         `json:"error,omitempty"`
	Artifacts []ArtifactInfo `json:"artifacts,omitempty"`
}

// ArtifactInfo is one stored object with its integrity data.
type ArtifactInfo struct {
	Kind        string    `json:"kind"` // bundle | meta | sha256
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	RetainUntil time.Time `json:"retainUntil"`
}

// Canonical returns the deterministic bytes that get signed and stored. Struct field
// order makes encoding/json output stable, so the stored bytes are the signed bytes.
// Keep the schema map-free, map key order would break verification.
func (m *Manifest) Canonical() ([]byte, error) { return json.Marshal(m) }

func statusString(ok bool) string {
	if ok {
		return StatusSuccess
	}
	return StatusFailed
}

func newRunID(t time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}
