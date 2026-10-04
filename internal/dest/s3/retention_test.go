package s3_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"gitdr.io/gitdr/internal/dest"
)

// What each store answers GetObjectRetention about an object that holds no lock, as each answered
// it: AWS as documented, MinIO RELEASE.2025-04-22 (the CI image) and RELEASE.2026-09-22, and
// Backblaze B2 on gitdr-e2e-b2-worm on 2026-10-04.
var (
	awsNoRetention   = errorAnswer{http.StatusNotFound, "NoSuchObjectLockConfiguration", "The specified object does not have a ObjectLock configuration"}
	minioNoRetention = errorAnswer{http.StatusBadRequest, "NoSuchObjectLockConfiguration", "The specified object does not have a ObjectLock configuration"}
	b2NoRetention    = errorAnswer{http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "The specified object does not have a ObjectLock configuration"}
)

// An object that holds no lock reads absent, in whichever words its store says so.
//
// Backblaze says it with ObjectLockConfigurationNotFoundError, the code AWS gives a bucket with no
// lock configuration, and that read not-checked: a B2 store that dropped the lock on a write
// passed --require-worm, and its manifest said immutable. Here the code is about the object, the
// only thing this read asks about. A refusal is still not a no.
func TestAnObjectWithoutALockReadsAbsentInItsStoresWords(t *testing.T) {
	for _, tc := range []struct {
		store  string
		answer errorAnswer
		want   dest.RetentionObservation
	}{
		{"AWS", awsNoRetention, dest.RetentionAbsent},
		{"MinIO", minioNoRetention, dest.RetentionAbsent},
		{"Backblaze B2", b2NoRetention, dest.RetentionAbsent},
		{"a key that may not read retention", errorAnswer{http.StatusForbidden, "AccessDenied", "Access Denied"}, dest.RetentionNotChecked},
		{"a store without the call", errorAnswer{http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented"}, dest.RetentionNotChecked},
	} {
		t.Run(tc.store, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.noRetention = tc.answer
			b := newBackend(t, srv)
			ctx := context.Background()
			report := []byte(`{"schema":"gitdr.drill/v1"}`)
			if _, err := b.PutImmutable(ctx, "drills/report.json", bytes.NewReader(report), int64(len(report)), dest.Retention{}); err != nil {
				t.Fatalf("put: %v", err)
			}
			got, until, err := b.ObserveRetention(ctx, "drills/report.json")
			if got != tc.want || !until.IsZero() {
				t.Errorf("%s %s %q reads %s until %v (%v), want %s", tc.store, http.StatusText(tc.answer.status), tc.answer.code, got, until, err, tc.want)
			}
			if fake.count("GetObjectRetention") != 1 {
				t.Errorf("%d GetObjectRetention requests, want 1", fake.count("GetObjectRetention"))
			}
		})
	}
}
