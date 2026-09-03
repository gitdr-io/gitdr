package pipeline_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"

	"gitdr.io/gitdr/internal/pipeline"
)

// Pins the gitdr.manifest/v5 field set. A change here means the public output contract
// changed, bump the schema version and update SPEC.md §14 deliberately.
//
// v4 added `destination.wormVerdict`. The three-state verdict exists because the engine could
// observe three things and had only two bits to say them with, so a store that refused the
// lock question was recorded as one that had answered no.
//
// v5 adds `destination.retentionObserved`, and it is the same failure one level down. The
// verdict is about the bucket's configuration; this is about an object gitdr actually wrote, and
// a store can report Object Lock enabled, accept the write and apply nothing. Nothing in v4
// could tell those apart, because `artifacts[].retainUntil` on S3 is the retention that was
// *asked for* - PutObject returns no object-lock headers, so there was never anything to observe.
func TestManifestV5Shape(t *testing.T) {
	if pipeline.ManifestSchema != "gitdr.manifest/v5" {
		t.Fatalf("manifest schema is now %q, that is a breaking contract change", pipeline.ManifestSchema)
	}

	ts := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	m := &pipeline.Manifest{
		Schema: pipeline.ManifestSchema,
		RunID:  "20260613T120000Z-a1b2c3d4e5f6",
		Tool:   pipeline.ToolInfo{Name: "gitdr", Version: "test"},
		Source: pipeline.SourceInfo{Type: "github", Host: "github.com"},
		Destination: pipeline.DestInfo{
			Type: "s3", Bucket: "b", WormMode: "COMPLIANCE",
			WormImmutable: true, WormVerdict: "immutable", WormDetails: "Object Lock enabled",
			RetentionObserved: "present",
		},
		StartedAt:  ts,
		FinishedAt: ts,
		Status:     pipeline.StatusSuccess,
		Repos: []pipeline.RepoEntry{{
			Slug:      "octo/hello",
			Status:    pipeline.StatusSuccess,
			Artifacts: []pipeline.ArtifactInfo{{Kind: "bundle", Key: "k", Size: 1, SHA256: "h", RetainUntil: ts}},
			Refs:      []pipeline.RefEntry{{Name: "refs/heads/main", Commit: "abc123"}},
			CopiedAt:  &ts,
		}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	top := decode(t, b)
	checkKeys(t, "manifest", top, "schema", "runId", "tool", "source", "destination", "startedAt", "finishedAt", "status", "repos")
	checkKeys(t, "tool", decode(t, top["tool"]), "name", "version")
	checkKeys(t, "source", decode(t, top["source"]), "type", "host")
	checkKeys(t, "destination", decode(t, top["destination"]),
		"type", "bucket", "wormMode", "wormImmutable", "wormVerdict", "wormDetails", "retentionObserved")

	var repos []json.RawMessage
	if err := json.Unmarshal(top["repos"], &repos); err != nil {
		t.Fatal(err)
	}
	repo := decode(t, repos[0])
	checkKeys(t, "repos[]", repo, "slug", "status", "artifacts", "refs", "copiedAt") // error and reason are omitempty
	var refs []json.RawMessage
	if err := json.Unmarshal(repo["refs"], &refs); err != nil {
		t.Fatal(err)
	}
	checkKeys(t, "refs[]", decode(t, refs[0]), "name", "commit")
	var arts []json.RawMessage
	if err := json.Unmarshal(repo["artifacts"], &arts); err != nil {
		t.Fatal(err)
	}
	checkKeys(t, "artifacts[]", decode(t, arts[0]), "kind", "key", "size", "sha256", "retainUntil")
}

func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func checkKeys(t *testing.T, where string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Fatalf("%s: field set changed (versioned contract, bump schema + SPEC §14): got %v, want %v", where, keys, want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s: missing contract field %q", where, k)
		}
	}
}

// A v2 manifest still verifies, which SPEC.md claims and nothing checked.
//
// v3 is additive: `refs` is omitempty and every other field is untouched. So a manifest
// written by an older gitdr, signed with the same key, must still canonicalise to the same
// bytes it was signed over — otherwise every artifact anyone has already stored becomes
// unverifiable the day they upgrade, which for a backup product is the worst possible
// regression and the one nobody would notice until they needed a restore.
func TestV2ManifestStillCanonicalisesToItsSignedBytes(t *testing.T) {
	// Exactly what a v2 gitdr wrote: no refs field anywhere.
	const stored = `{"schema":"gitdr.manifest/v2","runId":"20260101T000000Z-aaaaaaaaaaaa",` +
		`"tool":{"name":"gitdr","version":"0.3.0"},"source":{"type":"github","host":"github.com"},` +
		`"destination":{"type":"s3","bucket":"b","wormMode":"COMPLIANCE","wormImmutable":true,` +
		`"wormDetails":"Object Lock enabled"},"startedAt":"2026-01-01T00:00:00Z",` +
		`"finishedAt":"2026-01-01T00:00:00Z","status":"success","repos":[{"slug":"octo/hello",` +
		`"status":"success","artifacts":[{"kind":"bundle","key":"k","size":1,"sha256":"h",` +
		`"retainUntil":"2027-01-01T00:00:00Z"}]}]}`

	var m pipeline.Manifest
	if err := json.Unmarshal([]byte(stored), &m); err != nil {
		t.Fatalf("a v2 manifest no longer parses: %v", err)
	}
	if m.Schema != "gitdr.manifest/v2" {
		t.Fatalf("schema mangled on read: %q", m.Schema)
	}

	got, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stored {
		t.Errorf("v2 no longer round-trips to its signed bytes.\n stored: %s\n  got:   %s", stored, got)
	}
}

// copiedAt must vanish when it is unset, or v2 stops verifying.
//
// `omitempty` does nothing on a `time.Time`, because a struct is never empty to encoding/json.
// A value field there emitted "copiedAt":"0001-01-01T00:00:00Z" into every manifest, including
// one read back from v2, which changed its canonical bytes and broke the signature over every
// artifact anyone had already stored. This pins the pointer.
func TestCopiedAtIsAbsentWhenUnset(t *testing.T) {
	m := &pipeline.Manifest{
		Schema: pipeline.ManifestSchema,
		Repos:  []pipeline.RepoEntry{{Slug: "octo/hello", Status: pipeline.StatusSuccess}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("copiedAt")) {
		t.Errorf("copiedAt is present on an entry that has none: %s", b)
	}
	// Scoped to the entry. startedAt and finishedAt are required top-level fields and are
	// legitimately zero in this minimal fixture; asserting on the whole document failed on
	// them and would have pushed the fix into the wrong place.
	entry := b[bytes.Index(b, []byte(`"repos"`)):]
	if bytes.Contains(entry, []byte("0001-01-01")) {
		t.Errorf("a zero time leaked into a repo entry: %s", entry)
	}
}

// A v4 manifest still parses, and its silence is not mistaken for an answer.
//
// `retentionObserved` is omitempty, so a manifest written before v5 carries no such key. That
// absence means "the engine was too old to say" and is a different fact from "not-checked",
// which means the engine asked and the store would not answer. A reader that folds them together
// is making exactly the mistake the field was added to end - the same trap `wormVerdict` already
// documents, where null is an engine too old to speak and "unknown" is one that looked and could
// not tell.
func TestAV4ManifestStillParsesAndSaysNothingAboutRetention(t *testing.T) {
	// Exactly what a v4 gitdr wrote: a verdict, and no retentionObserved anywhere.
	const stored = `{"schema":"gitdr.manifest/v4","runId":"20260101T000000Z-aaaaaaaaaaaa",` +
		`"tool":{"name":"gitdr","version":"0.1.14"},"source":{"type":"github","host":"github.com"},` +
		`"destination":{"type":"s3","bucket":"b","wormMode":"COMPLIANCE","wormImmutable":true,` +
		`"wormVerdict":"immutable","wormDetails":"Object Lock enabled"},` +
		`"startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:00:00Z","status":"success",` +
		`"repos":[{"slug":"octo/hello","status":"success","artifacts":[]}]}`

	var m pipeline.Manifest
	if err := json.Unmarshal([]byte(stored), &m); err != nil {
		t.Fatalf("a v4 manifest no longer parses: %v", err)
	}
	if m.Destination.WormVerdict != "immutable" {
		t.Errorf("wormVerdict = %q, want immutable", m.Destination.WormVerdict)
	}
	if m.Destination.RetentionObserved != "" {
		t.Errorf("retentionObserved = %q on a v4 manifest; it must stay empty, because an engine "+
			"too old to look is not an engine that looked and got no answer",
			m.Destination.RetentionObserved)
	}

	// And a v5 manifest that genuinely could not check says so, distinguishably.
	m.Destination.RetentionObserved = string(dest.RetentionNotChecked)
	b, err := json.Marshal(m.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"retentionObserved":"not-checked"`) {
		t.Errorf("not-checked did not survive the round trip: %s", b)
	}
}
