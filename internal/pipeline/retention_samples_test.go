package pipeline

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
)

// The run asks about the smallest object it wrote and the largest: the first went in one PUT, the
// second in parts if anything did. Once when they are one object, and not at all when nothing was
// written.
func TestRetentionIsSampledOnceForEachWritePath(t *testing.T) {
	art := func(key string, size int64) ArtifactInfo { return ArtifactInfo{Key: key, Size: size} }
	for _, tc := range []struct {
		name    string
		entries []RepoEntry
		want    []string
	}{
		{"nothing written", []RepoEntry{{Slug: "octo/a", Status: StatusSkipped}}, nil},
		{"one object", []RepoEntry{{Slug: "octo/a", Artifacts: []ArtifactInfo{art("a.meta.json", 41)}}}, []string{"a.meta.json"}},
		{
			"the smallest and the largest, across repositories",
			[]RepoEntry{
				{Slug: "octo/a", Artifacts: []ArtifactInfo{art("a.bundle", 330), art("a.meta.json", 43), art("a.sha256", 75)}},
				{Slug: "octo/skipped", Status: StatusSkipped},
				{Slug: "octo/b", Artifacts: []ArtifactInfo{art("b.bundle", 6<<30), art("b.meta.json", 41), art("b.sha256", 75), art("b.lfs.tar", 5<<30)}},
			},
			[]string{"b.meta.json", "b.bundle"},
		},
		{
			"a failed copy's objects count, it wrote them",
			[]RepoEntry{
				{Slug: "octo/a", Status: StatusSuccess, Artifacts: []ArtifactInfo{art("a.bundle", 330), art("a.sha256", 75)}},
				{Slug: "octo/b", Status: StatusFailed, Artifacts: []ArtifactInfo{art("b.lfs.tar", 9<<30)}},
			},
			[]string{"a.sha256", "b.lfs.tar"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retentionSamples(tc.entries); !slices.Equal(got, tc.want) {
				t.Errorf("sampled %q, want %q", got, tc.want)
			}
		})
	}
}

// keyedObserver answers ObserveRetention per key, and records what it was asked.
type keyedObserver struct {
	stubDest
	says  map[string]dest.RetentionObservation
	asked []string
}

func (k *keyedObserver) ObserveRetention(_ context.Context, key string) (dest.RetentionObservation, time.Time, error) {
	k.asked = append(k.asked, key)
	if k.says[key] == dest.RetentionPresent {
		return dest.RetentionPresent, time.Now().Add(24 * time.Hour), nil
	}
	return k.says[key], time.Time{}, nil
}

// What the run records is what every write path said: absent on either lowers the verdict, present
// takes both, and one path that would not answer leaves nothing confirmed for the run.
func TestTheRetentionObservedIsWhatEveryWritePathSaid(t *testing.T) {
	const (
		present    = dest.RetentionPresent
		absent     = dest.RetentionAbsent
		notChecked = dest.RetentionNotChecked
	)
	entries := []RepoEntry{
		{Slug: "octo/small", Artifacts: []ArtifactInfo{{Key: "one-put", Size: 41}}},
		{Slug: "octo/big", Artifacts: []ArtifactInfo{{Key: "in-parts", Size: 6 << 30}}},
	}
	for _, tc := range []struct {
		onePut, inParts dest.RetentionObservation
		want            dest.RetentionObservation
		wantVerdict     dest.WormVerdict
	}{
		{present, present, present, dest.VerdictImmutable},
		{present, absent, absent, dest.VerdictNotImmutable},
		{absent, present, absent, dest.VerdictNotImmutable},
		{notChecked, absent, absent, dest.VerdictNotImmutable},
		{present, notChecked, notChecked, dest.VerdictImmutable},
		{notChecked, notChecked, notChecked, dest.VerdictImmutable},
	} {
		obs := &keyedObserver{says: map[string]dest.RetentionObservation{"one-put": tc.onePut, "in-parts": tc.inParts}}
		r := &backupRun{dst: obs, log: slog.New(slog.DiscardHandler), wormStatus: dest.WormStatus{Verdict: dest.VerdictImmutable}}
		got, verdict := r.observeRetention(context.Background(), entries)
		if got != tc.want || verdict != tc.wantVerdict {
			t.Errorf("one PUT %s, in parts %s: recorded %s and %s, want %s and %s",
				tc.onePut, tc.inParts, got, verdict, tc.want, tc.wantVerdict)
		}
		if !slices.Equal(obs.asked, []string{"one-put", "in-parts"}) {
			t.Errorf("one PUT %s, in parts %s: asked about %q, want both paths", tc.onePut, tc.inParts, obs.asked)
		}
	}
}
