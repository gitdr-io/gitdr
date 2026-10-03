package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gcsbackend "gitdr.io/gitdr/internal/dest/gcs"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// A copy of an unchanged repository is relied on only while the bucket's own lock still holds it.
//
// On GCS and Azure gitdr locks nothing itself: the bucket's retention policy holds every copy for
// the policy's period, whatever retention.days says. Up to v0.1.20 the skip window was a third of
// retention.days anyway, ten days at the default thirty, so in a bucket locked for one day a copy two
// days old was skipped, relied on a day after anything stopped it being deleted.
//
// The bucket here is the GCS emulator with a locked one-day policy, so the window is a third of a
// day. A run six hours after the copy relies on it; the run two days after copies again.
func TestAnUnchangedCopyIsNotTrustedPastTheBucketsOwnLock(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	const bucket = "gitdr-one-day"
	endpoint := gcsEmulator(t, bucket, &gcsPolicy{period: 24 * time.Hour, locked: true})
	dst, err := gcsbackend.New(ctx, gcsbackend.Options{Bucket: bucket, Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := gcsConfig(bucket, endpoint)
	if cfg.Destination.Retention.Days != 30 {
		t.Fatalf("retention.days = %d; this test is about the default 30 against a one-day lock", cfg.Destination.Retention.Days)
	}
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: initFixtureRepo(t), DefaultBranch: "main",
	}}}
	signer := testSigner(t)
	start := time.Date(2026, 6, 13, 20, 0, 0, 0, time.UTC)
	run := func(after time.Duration) pipeline.RepoEntry {
		t.Helper()
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: cfg, Source: src, Dest: dst, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test",
			Now: func() time.Time { return start.Add(after) },
		})
		if err != nil {
			t.Fatalf("backup %s after the first: %v", after, err)
		}
		if d := res.Manifest.Destination.WormDetails; d != "bucket retention policy Locked, 1 day" {
			t.Fatalf("the emulated bucket was not read as locked for a day: %s", d)
		}
		return res.Manifest.Repos[0]
	}

	if got := run(0); got.Status != pipeline.StatusSuccess {
		t.Fatalf("first run: %s %s", got.Status, got.Error)
	}
	// The next UTC date, so this is the unchanged check and not a same-day rerun. Six hours into a
	// one-day lock, two thirds of it is still ahead.
	if got := run(6 * time.Hour); got.Status != pipeline.StatusSkipped || !strings.HasPrefix(got.Reason, pipeline.ReasonUnchanged) {
		t.Errorf("six hours after the copy: %s %q, want a skip that relies on it", got.Status, got.Reason)
	}
	// Two days after the copy its lock ended a day ago.
	if got := run(48 * time.Hour); got.Status != pipeline.StatusSuccess {
		t.Errorf("two days after the copy, in a bucket that locks for one: %s %q; the run relied on a copy nothing holds any more",
			got.Status, got.Reason)
	}
}
