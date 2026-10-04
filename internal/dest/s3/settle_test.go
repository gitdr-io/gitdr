package s3_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// A write settled as ours, because the object at the key holds its bytes, carries the retention
// the store holds that object to, as HeadObject shows it, and never the one the write asked for.
//
// On AWS a write to a key that already holds the same bytes meets If-None-Match, 412, and the
// settle takes the object there as this write's. That object is an earlier write's, held to that
// write's date and mode, and the result carried the date asked for: a manifest could say an
// artifact is retained longer than the store holds it. Where HeadObject shows no lock, because
// there is none or because the key may not read it, the result claims none.
func TestAWriteSettledAsOursCarriesTheRetentionStored(t *testing.T) {
	const key = "github.com/octo/hello/2026-10-04/hello.bundle"
	asked := dest.Retention{Mode: dest.RetentionCompliance, Until: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	earlier := time.Date(2029, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		// inParts writes 23 MiB in parts; otherwise a small object in one PUT.
		inParts bool
		fault   func(f *fakeStore, data []byte)
		until   time.Time
		mode    dest.RetentionMode
	}{
		{
			name: "the same bytes an earlier write left, held to its own date",
			fault: func(f *fakeStore, data []byte) {
				f.seedHeld(key, data, "GOVERNANCE", "2029-06-01T00:00:00Z")
			},
			until: earlier, mode: dest.RetentionGovernance,
		},
		{
			name: "the same bytes an earlier write left, on a key that may not read the lock",
			fault: func(f *fakeStore, data []byte) {
				f.seedHeld(key, data, "GOVERNANCE", "2029-06-01T00:00:00Z")
				f.hideLockOnHead = true
			},
		},
		{
			name:  "a put that landed and lost its answer",
			fault: func(f *fakeStore, _ []byte) { f.dropAfterCommit = 1 },
			until: asked.Until, mode: dest.RetentionCompliance,
		},
		{
			name: "a put that landed and lost its answer, on a key that may not read the lock",
			fault: func(f *fakeStore, _ []byte) {
				f.dropAfterCommit = 1
				f.hideLockOnHead = true
			},
		},
		{
			name:    "a Complete that landed and lost its answer",
			inParts: true,
			fault:   func(f *fakeStore, _ []byte) { f.dropCompleteAfterCommit = true },
			until:   asked.Until, mode: dest.RetentionCompliance,
		},
		{
			name:    "a Complete that landed without its lock and lost its answer",
			inParts: true,
			fault: func(f *fakeStore, _ []byte) {
				f.dropCompleteAfterCommit = true
				f.dropLockOnComplete = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = true
			b := newPartsBackend(t, srv, true)

			data := []byte("the bundle of one repository")
			var body io.Reader = bytes.NewReader(data)
			if tc.inParts {
				body, data = payloadFile(t, 23*mib)
			}
			tc.fault(fake, data)

			res, err := b.PutImmutable(context.Background(), key, body, int64(len(data)), asked)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if !res.RetainUntil.Equal(tc.until) || res.RetainMode != tc.mode {
				t.Errorf("the result says retained until %v under %q; HeadObject shows the object held to %v under %q",
					res.RetainUntil, res.RetainMode, tc.until, tc.mode)
			}
			if o := fake.object(key); o == nil || o.versions != 1 || !bytes.Equal(o.data, data) {
				t.Errorf("the store holds %+v, want the one object", o)
			}
			if tc.inParts {
				noAbort(t, fake)
			}
		})
	}
}

// The ordinary path is unchanged: a write the store answered carries the retention it asked for,
// under the mode it asked for.
func TestAWriteTheStoreAnsweredCarriesTheRetentionAskedFor(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		asked dest.Retention
		mode  dest.RetentionMode
	}{
		{dest.Retention{Mode: dest.RetentionCompliance, Until: until}, dest.RetentionCompliance},
		{dest.Retention{Mode: dest.RetentionGovernance, Until: until}, dest.RetentionGovernance},
		{dest.Retention{}, ""},
	} {
		fake, srv := newFakeStore(t)
		fake.conditional = true
		b := newBackend(t, srv)
		s3backend.ConditionalWrites(b)
		data := []byte("the bundle of one repository")
		res, err := b.PutImmutable(context.Background(), "k", bytes.NewReader(data), int64(len(data)), tc.asked)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if !res.RetainUntil.Equal(tc.asked.Until) || res.RetainMode != tc.mode {
			t.Errorf("asked %+v; the result says %v under %q", tc.asked, res.RetainUntil, res.RetainMode)
		}
	}
}
