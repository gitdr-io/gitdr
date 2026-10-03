package gcs

import (
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"gitdr.io/gitdr/internal/dest"
)

// The verdict and the words for each retention policy a bucket can report. The emulator holds no
// policy, so these are the SDK's own type, the way the bucket's metadata parses into it; the
// pipeline's GCS test reads one through the SDK end to end.
func TestVerifyWormSaysWhatTheBucketLocks(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name    string
		policy  *storage.RetentionPolicy
		verdict dest.WormVerdict
		details string
	}{
		{"no policy", nil, dest.VerdictNotImmutable, "no bucket retention policy"},
		{"locked for 30 days", &storage.RetentionPolicy{RetentionPeriod: 30 * day, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 30 days"},
		{"locked for a day", &storage.RetentionPolicy{RetentionPeriod: day, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 1 day"},
		// gcloud's 1y is 365.25 days, 31,557,600 seconds.
		{"locked for a year", &storage.RetentionPolicy{RetentionPeriod: 31557600 * time.Second, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 365.25 days"},
		{"locked for 36 hours", &storage.RetentionPolicy{RetentionPeriod: 36 * time.Hour, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 1.5 days"},
		{"locked for 25 hours", &storage.RetentionPolicy{RetentionPeriod: 25 * time.Hour, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 25 hours"},
		{"locked for 90 seconds", &storage.RetentionPolicy{RetentionPeriod: 90 * time.Second, IsLocked: true},
			dest.VerdictImmutable, "bucket retention policy Locked, 90 seconds"},
		{"unlocked", &storage.RetentionPolicy{RetentionPeriod: 30 * day},
			dest.VerdictNotImmutable, "bucket retention policy Unlocked, 30 days; an unlocked policy can be shortened or removed"},
		{"locked, and no period", &storage.RetentionPolicy{IsLocked: true},
			dest.VerdictUnknown, "bucket retention policy Locked, with no retention period reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := verdictFromPolicy(tc.policy)
			if st.Verdict != tc.verdict || st.Details != tc.details {
				t.Errorf("got %q, %q; want %q, %q", st.Verdict.Wire(), st.Details, tc.verdict.Wire(), tc.details)
			}
		})
	}
}
