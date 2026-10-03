package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// stoppingSource stops the run, the way SIGTERM does, when it is asked for the metadata of the
// repository named at, and then takes as long as the stopped context allows.
type stoppingSource struct {
	*fixtureSource
	at   string
	stop context.CancelCauseFunc
}

func (s *stoppingSource) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	if r.Name == s.at {
		s.stop(errors.New("terminated signal received"))
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.fixtureSource.FetchMetadata(ctx, r)
}

// A backup stopped halfway files a manifest of what it did, and the copies it records count.
//
// SIGTERM cancelled the context the manifest was written on, so a stopped run filed nothing: the
// copies it had finished were in the bucket, and nothing could verify, restore or drill them, or
// skip them the next time. Now the run stops starting repositories, records the one in flight and
// the ones never started as stopped before they finished, and files its manifest anyway. That
// manifest verifies, and the same day's rerun skips the copies it records and makes the rest.
func TestAStoppedBackupStillFilesItsManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	pub, signer := drillKeys(t)
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/r1", "octo/r2", "octo/r3", "octo/r4")
	// A store that refuses a write on a context that is done, as every real store does. The
	// manifest being there is then the proof it was written on a context the stop did not cancel.
	md := contextDest{newMemDest(true)}
	cfg := testConfig()
	cfg.Source.Repo = ""
	cfg.Backup.Concurrency = 1
	morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	md.storeAt(morning)

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &stoppingSource{fixtureSource: &fixtureSource{repos: repos}, at: "r3", stop: stop},
		Dest: md, Git: gitexec.New(nil), SigningKey: signer, ToolVersion: "test",
		Now: func() time.Time { return morning },
	})
	if err == nil {
		t.Error("a stopped run reported success")
	}
	if res == nil || res.ManifestKey == "" || objectsUnder(md.memDest, res.ManifestKey) == 0 {
		t.Fatalf("the stopped run filed no manifest: %v", err)
	}
	if res.Manifest.Status != pipeline.StatusFailed {
		t.Errorf("the stopped run's manifest says %s", res.Manifest.Status)
	}
	for i, want := range []string{"", "", "stopped before it finished: metadata: context canceled", "stopped before it finished: terminated signal received"} {
		e := res.Manifest.Repos[i]
		switch {
		case want == "" && e.Status != pipeline.StatusSuccess:
			t.Errorf("%s = %s %q, want copied before the stop", e.Slug, e.Status, e.Error)
		case want != "" && (e.Status != pipeline.StatusFailed || e.Error != want):
			t.Errorf("%s = %s %q, want failed with %q", e.Slug, e.Status, e.Error, want)
		}
	}

	v, err := pipeline.Verify(context.Background(), pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey)
	if err != nil || v.ArtifactsOK != 6 {
		t.Errorf("verify of the stopped run's manifest: %v, %d artifacts ok, want the 6 of the two copies", err, v.ArtifactsOK)
	}

	rerun := backupAt(t, md.memDest, signer, morning.Add(time.Hour), repos)
	for i, want := range []string{pipeline.StatusSkipped, pipeline.StatusSkipped, pipeline.StatusSuccess, pipeline.StatusSuccess} {
		if e := rerun.Manifest.Repos[i]; e.Status != want {
			t.Errorf("rerun: %s = %s %q %q, want %s", e.Slug, e.Status, e.Reason, e.Error, want)
		}
	}
}
