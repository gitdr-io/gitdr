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
// WormVerdict is what gitdr was able to determine about a destination's immutability.
//
// Three answers and not two, because the store can refuse the question. Before this existed
// there were two booleans, so a store that could not answer was recorded as one that had
// answered no — a definite negative claim gitdr had not earned. Google's S3 surface is the
// case that made it visible: it implements the lock call but reports Object Retention Lock,
// so a bucket protected by a locked Bucket Lock policy answers exactly like an open one.
//
// The distinction is not about a provider. It is about what the protocol said: a store that
// answers has earned its negative, and a store that refuses has told us nothing.
type WormVerdict string

const (
	// VerdictUnknown is the zero value, on purpose. A backend that returns a WormStatus
	// without setting a verdict claims nothing, fails closed under --require-worm, and is
	// sent no retention. The other choice — zero meaning confirmed-absent — makes a
	// forgetful backend state exactly the unearned negative this type exists to prevent.
	VerdictUnknown WormVerdict = ""
	// VerdictImmutable: the store answered, and what it described is enforced.
	VerdictImmutable WormVerdict = "immutable"
	// VerdictNotImmutable: the store answered, and said it locks nothing. Earned, and worth
	// saying loudly — it is one of the most useful warnings this tool prints.
	VerdictNotImmutable WormVerdict = "not-immutable"
)

// Wire is the value written to the manifest. Never empty: an omitted field on a v4 manifest
// would tell a reader "an engine too old to say", which is a different answer from "the engine
// said it cannot tell" and a lie about the producer.
func (v WormVerdict) Wire() string {
	if v == VerdictUnknown {
		return "unknown"
	}
	return string(v)
}

// Immutable reports whether the destination was confirmed immutable. It is the only condition
// under which retention headers are sent and the only one --require-worm accepts.
func (v WormVerdict) Immutable() bool { return v == VerdictImmutable }

type WormStatus struct {
	// Verdict replaces the Enabled/Locked pair. Two booleans that had to agree could express
	// a state neither of them meant, and nothing outside a test ever read Enabled.
	Verdict WormVerdict
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

// RetentionObservation is what a store said about one object's retention, after it was written.
//
// Three answers and not two, for the same reason WormVerdict has three: a store that refuses the
// question has told us nothing, and recording that as "no retention" is a definite negative gitdr
// has not earned. On S3 the refusal is the common case rather than the exotic one - the lock
// headers on a HEAD, and `?retention` itself, both need `s3:GetObjectRetention`, and this
// product's own advice is to scope destination credentials create/put-only.
type RetentionObservation string

const (
	// RetentionPresent means the store returned a retention for the object gitdr wrote.
	RetentionPresent RetentionObservation = "present"
	// RetentionAbsent means the store implements the question and said this object holds
	// nothing. An earned negative: the write was accepted and the retention was not applied.
	RetentionAbsent RetentionObservation = "absent"
	// RetentionNotChecked means nobody asked, or the store would not answer. The safe zero
	// value, and never a downgrade.
	RetentionNotChecked RetentionObservation = "not-checked"
)

// RetentionObserver is an optional interface for Destinations that can be asked what retention
// actually landed on an object they wrote.
//
// Optional, so Destination stays at four methods and a backend that cannot answer declines
// honestly rather than inventing one. The pipeline asserts for it and records
// RetentionNotChecked when it is absent.
//
// This exists because `artifacts[].retainUntil` was not the same fact on every backend. GCS
// records what the write returned; S3 records what gitdr asked for, because PutObject returns no
// object-lock headers at all. Both went into a signed manifest, so on S3 the document named a
// retain-until date that nothing had confirmed - and if a store accepted the write and ignored
// the lock headers, that date was signed and wrong.
type RetentionObserver interface {
	// ObserveRetention asks the store what retention is on key. It is read-only, and it must
	// distinguish "the store said none" from "the store would not say": an error the caller
	// cannot classify is RetentionNotChecked, never RetentionAbsent.
	ObserveRetention(ctx context.Context, key string) (RetentionObservation, time.Time, error)
}
