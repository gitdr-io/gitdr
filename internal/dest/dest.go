// Package dest defines the create-only Destination interface implemented by every
// storage backend (S3 and S3-compatible; GCS and Azure later).
//
// Invariant: no Delete/Remove/Overwrite method, anywhere, backups are append-only by
// construction. Runtime immutability is enforced by the WORM gate (VerifyWorm) and by
// object-lock retention on every write.
package dest

import (
	"context"
	"io"
	"time"
)

// RetentionMode is the object-lock mode requested for an immutable write.
type RetentionMode string

const (
	// RetentionCompliance is true WORM: not even the root account can shorten
	// retention or delete the object before it expires. This is the gitdr default.
	RetentionCompliance RetentionMode = "COMPLIANCE"
	// RetentionGovernance allows sufficiently privileged identities to bypass
	// retention. Weaker; offered only because some buckets are provisioned this way.
	RetentionGovernance RetentionMode = "GOVERNANCE"
)

// Retention describes how long a written object must remain immutable.
type Retention struct {
	Mode  RetentionMode // COMPLIANCE (default) or GOVERNANCE
	Until time.Time     // retain-until timestamp (UTC)
}

// WormStatus reports a destination's immutability configuration as observed by the
// preflight gate.
type WormStatus struct {
	Enabled bool   // object lock / immutability is enabled on the bucket/container
	Locked  bool   // immutability is enforced (a retention mode/policy is in effect)
	Mode    string // observed default mode, if any (e.g. "COMPLIANCE")
	Details string // human-readable detail for logs and `gitdr doctor`
}

// PutResult describes the outcome of a successful immutable write.
type PutResult struct {
	Key         string    `json:"key"`
	ETag        string    `json:"etag,omitempty"`
	VersionID   string    `json:"versionId,omitempty"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256,omitempty"`
	RetainUntil time.Time `json:"retainUntil"`
}

// Object is a stored object as seen when listing a prefix (read-only).
type Object struct {
	Key  string
	Size int64
}

// Destination is the create-only storage interface. Its entire write surface is one
// method (PutImmutable); every other method is read-only. There is intentionally no
// delete/overwrite operation, see the package-level invariant.
type Destination interface {
	// VerifyWorm probes the destination's immutability configuration. The pipeline
	// calls this before writing: if immutability isn't confirmed (WormStatus.Locked)
	// it warns and proceeds, unless worm.require is set, in which case it fails closed.
	VerifyWorm(ctx context.Context) (WormStatus, error)

	// PutImmutable creates an object at key with object-lock retention applied. It is
	// create-only: implementations MUST refuse to overwrite an existing key
	// (fail-closed) and MUST never delete. size is the exact content length; r is
	// streamed to storage.
	PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret Retention) (PutResult, error)

	// List returns objects under prefix. Read-only; used by restore/verify.
	List(ctx context.Context, prefix string) ([]Object, error)

	// Get opens an object for reading. Read-only; used by restore/verify. The caller
	// closes the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}
