package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/logging"
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

// stopWhen stops the run, the way SIGTERM does, once at is closed, and says when.
func stopWhen(at <-chan struct{}, stop context.CancelCauseFunc) <-chan time.Time {
	when := make(chan time.Time, 1)
	go func() {
		<-at
		when <- time.Now()
		stop(errors.New("terminated signal received"))
	}()
	return when
}

// releaseLater closes release after d, or when the test ends if that is sooner: a step that ignores
// the stop and takes d. A run that waits for it returns d after the stop, and one that does not
// returns without it.
func releaseLater(t *testing.T, release chan struct{}, d time.Duration) {
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	time.AfterFunc(d, let)
	t.Cleanup(let)
}

// stoppedAt is when stopWhen stopped the run, which has returned. A run that was never stopped
// fails the test: the step it was to be stopped in never began.
func stoppedAt(t *testing.T, when <-chan time.Time, err error) time.Time {
	t.Helper()
	select {
	case at := <-when:
		return at
	default:
		t.Fatalf("the run was never stopped, so the step it was to be stopped in never began: %v", err)
		return time.Time{}
	}
}

// syncLog is a log that a repository the run stopped waiting for can still write to.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A stop during a step that does not look at the context still files the manifest, within the
// grace.
//
// The grace counted from the stop, and the run waited for every repository in flight without a
// bound. An archive of many GiB of LFS objects, its encryption or its checksum ran to the end of
// the file whatever the stop said, and one that outlasted the grace took the manifest's own upload
// with it: "manifest: upload manifest: context canceled". Those steps now stop at their next read,
// and a repository still running inFlightGrace after the stop is recorded as stopped before it
// finished while the run files its manifest. The archive here takes 20 s, the grace 1 s.
func TestAStopDuringAStepThatIgnoresItStillFilesTheManifest(t *testing.T) {
	requireLFS(t)
	t.Chdir(t.TempDir())
	repoDir, _ := initLFSFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	restoreArchive := pipeline.HoldLFSArchiveForTest(started, release)
	defer restoreArchive()
	releaseLater(t, release, 20*time.Second)
	restoreGrace := pipeline.ShortenInFlightGraceForTest(time.Second)
	defer restoreGrace()
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	when := stopWhen(started, stop)

	md := contextDest{newMemDest(true)}
	cfg := testConfig()
	cfg.Source.Repo = ""
	var logged syncLog
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/lfsrepo")},
		Dest: md, Git: gitexec.New(nil), SigningKey: testSigner(t), ToolVersion: "test",
		Now: fixedClock(), Logger: logging.New("info", "json", &logged),
	})
	took := time.Since(stoppedAt(t, when, err))

	if err == nil {
		t.Error("a stopped run reported success")
	}
	if res == nil || res.ManifestKey == "" || objectsUnder(md.memDest, res.ManifestKey) == 0 {
		t.Fatalf("the stopped run filed no manifest: %v", err)
	}
	if took < time.Second || took > 10*time.Second {
		t.Errorf("the manifest was filed %s after the stop, want the second the repository in flight was given and little more", took)
	}
	const want = "stopped before it finished: terminated signal received; still running 1s after the stop"
	if e := res.Manifest.Repos[0]; e.Status != pipeline.StatusFailed || e.Error != want || len(e.Artifacts) != 0 {
		t.Errorf("octo/lfsrepo = %s %q with %d artifacts, want failed with %q and none: nothing is written before the archive", e.Status, e.Error, len(e.Artifacts), want)
	}
	if lines := eventLines(t, logged.String(), "repo finished"); len(lines) != 1 {
		t.Errorf("%d repo finished lines for one repository:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// holdingDest is a store whose write of a key ending in hold takes until release, whatever its
// context says, the way a store that checksums a whole artifact before it sends a byte holds a
// write. held is closed as that write begins. A write whose context is done is refused.
type holdingDest struct {
	contextDest
	hold          string
	held, release chan struct{}
	once          sync.Once
}

func (d *holdingDest) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	if strings.HasSuffix(key, d.hold) {
		d.once.Do(func() { close(d.held) })
		<-d.release
	}
	return d.contextDest.PutImmutable(ctx, key, r, size, ret)
}

// A repository the run stops waiting for is recorded with what it wrote, and the manifest that
// records it verifies. Here the bundle is written, and the metadata, the next upload, takes 20 s
// to start.
func TestARepositoryTheRunStopsWaitingForKeepsWhatItWrote(t *testing.T) {
	t.Chdir(t.TempDir())
	pub, signer := drillKeys(t)
	md := &holdingDest{contextDest: contextDest{newMemDest(true)}, hold: ".meta.json", held: make(chan struct{}), release: make(chan struct{})}
	releaseLater(t, md.release, 20*time.Second)
	restoreGrace := pipeline.ShortenInFlightGraceForTest(time.Second)
	defer restoreGrace()
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	when := stopWhen(md.held, stop)

	cfg := testConfig()
	cfg.Source.Repo = ""
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/r1")},
		Dest: md, Git: gitexec.New(nil), SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	stoppedAt(t, when, err)
	if res == nil || res.ManifestKey == "" {
		t.Fatalf("the stopped run filed no manifest: %v", err)
	}
	e := res.Manifest.Repos[0]
	var kinds []string
	for _, a := range e.Artifacts {
		kinds = append(kinds, a.Kind)
	}
	const want = "stopped before it finished: terminated signal received; still running 1s after the stop"
	if e.Status != pipeline.StatusFailed || e.Error != want || !slices.Equal(kinds, []string{"bundle"}) {
		t.Errorf("octo/r1 = %s %q with %v, want failed with %q and the bundle it wrote", e.Status, e.Error, kinds, want)
	}
	v, err := pipeline.Verify(context.Background(), pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey)
	if err != nil || v.ArtifactsOK != 1 {
		t.Errorf("verify of the stopped run's manifest: %v, %d artifacts ok, want the bundle", err, v.ArtifactsOK)
	}
}
