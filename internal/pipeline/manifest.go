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
//
// v3 adds RepoEntry.Refs, the ref-to-commit map the source advertised when the copy was
// made. It is what lets the next run tell whether a repository has changed without cloning
// it, and it is additive: every v2 field is unchanged and a v2 manifest still verifies.
const ManifestSchema = "gitdr.manifest/v5"

// Status values used in the manifest.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusSkipped = "skipped" // nothing to write; see RepoEntry.Reason for which case

	// The reasons a repository is skipped. Both mean "nothing was written and nothing was
	// lost", which is why neither fails the run, but they are different facts and an operator
	// reading a manifest is entitled to know which one applies.
	ReasonResume = "already backed up for this date"
	ReasonEmpty  = "repository has no commits"
	// ReasonUnchanged is a PREFIX, not a whole string: the date of the copy being relied on
	// follows it, because an operator looking at a skipped repository wants to know which copy
	// they still have.
	//
	// The prefix is the stable half and it is part of the contract. A consumer matches on it
	// and renders the rest; the two reasons above are matched whole, and a dynamic string
	// where those are constant would have left every consumer falling through to "skipped for
	// some reason" on what is now the most common outcome of a run.
	ReasonUnchanged = "unchanged since"
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
	WormMode      string `json:"wormMode,omitempty"` // configured retention mode
	WormImmutable bool   `json:"wormImmutable"`      // WORM check confirmed immutable
	/*
	 * What gitdr was able to determine: "immutable", "not-immutable" or "unknown".
	 *
	 * `wormImmutable` is not a liar and keeps its exact v3 meaning — false covers both of the
	 * other two verdicts. The defect was that the engine observed three distinguishable things
	 * and shipped two bits plus a sentence, so the only way a consumer could recover the third
	 * was to match on prose. Added rather than repurposed, because a nullable boolean would
	 * change the meaning of a field every pinned consumer already reads, in the direction where
	 * absent reads as falsy.
	 *
	 * `omitempty` on a string is required, not stylistic. Without it, re-reading a v2 or v3
	 * manifest and marshalling it again adds a key, which changes the canonical bytes and
	 * breaks every signature already written. The round-trip test is the tripwire.
	 */
	WormVerdict string `json:"wormVerdict,omitempty"`
	WormDetails string `json:"wormDetails,omitempty"` // observed immutability detail
	/*
	 * What was actually on an object gitdr wrote, from `gitdr.manifest/v5` on: "present",
	 * "absent" or "not-checked".
	 *
	 * WormVerdict is a statement about the bucket's configuration. This is a statement about one
	 * object, and the two can disagree: a store can report Object Lock enabled, accept a write,
	 * and apply nothing. Nothing else in the manifest could tell those apart, because
	 * `artifacts[].retainUntil` on the S3 path is the retention gitdr *asked for* - PutObject
	 * returns no object-lock headers, so there was never anything to observe there.
	 *
	 * Absent, like WormVerdict's, means the engine was too old to say. It is **not**
	 * "not-checked": an engine that did not have the field and an engine that asked and got no
	 * answer are different facts, and a reader that folds them together is making the same
	 * mistake this field exists to end.
	 *
	 * Only ever lowers a claim. A "present" here does not raise WormVerdict, because one object
	 * carrying retention proves the store implements the headers and nothing about the rest.
	 */
	RetentionObserved string `json:"retentionObserved,omitempty"`
}

// RepoEntry is the per-repository outcome.
type RepoEntry struct {
	Slug   string `json:"slug"`
	Status string `json:"status"` // success | failed | skipped
	Error  string `json:"error,omitempty"`
	// Why a repository was skipped, in plain words. Additive to the manifest schema rather
	// than a new status value, because a consumer switching on `status` would break on an
	// unknown one and there is more than one reason to skip.
	Reason    string         `json:"reason,omitempty"`
	Artifacts []ArtifactInfo `json:"artifacts,omitempty"`
	// What the source advertised when this copy was made, from `git ls-remote`.
	//
	// It is recorded so the next run can ask the same question and compare, and skip a
	// repository whose refs have not moved instead of writing a byte-identical copy of its
	// entire history. Before this, every run rewrote everything, and on WORM storage the
	// customer could not delete any of it.
	//
	// A slice and not a map: Canonical() relies on struct field order for stable bytes, and
	// Go map iteration order is random, so a map here would produce a different signature
	// for the same run. Sorted by name for the same reason.
	//
	// Recorded only on a successful copy. A run that failed halfway has refs that describe a
	// repository nothing was written for, and trusting them would skip the retry.
	Refs []RefEntry `json:"refs,omitempty"`
	// When the artifacts this entry relies on were actually written.
	//
	// For a copy that was made this run it is this run's finish time. For a repository that
	// was skipped as unchanged it is carried forward from the run that made the copy, which
	// is the whole point: without it, each skip would reset the age of the copy and the
	// refresh bound in unchanged.go would never fire, so a repository that never changes
	// would be skipped past its object lock's expiry and end up with nothing.
	//
	// Found by running the backup three times in a row and watching the third copy in full.
	//
	// A pointer, because `omitempty` does nothing on a `time.Time`: a struct is never empty to
	// encoding/json, so a value field emitted `"copiedAt":"0001-01-01T00:00:00Z"` into every
	// manifest including ones re-read from v2 — which changed their canonical bytes and made
	// every already-signed manifest fail verification. Caught by the v2 round-trip test.
	CopiedAt *time.Time `json:"copiedAt,omitempty"`
}

// RefEntry is one ref and the object it pointed at, as the source advertised it.
type RefEntry struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
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
