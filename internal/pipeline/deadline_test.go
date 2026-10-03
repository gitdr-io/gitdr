package pipeline_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// stalledMetadata is a fixture source whose metadata takes as long as its context allows, up to
// stall: a repository caught in a long wait for a rate limit.
type stalledMetadata struct {
	*fixtureSource
	stall time.Duration
}

func (s *stalledMetadata) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.stall):
		return s.fixtureSource.FetchMetadata(ctx, r)
	}
}

// contextDest is a memDest that refuses a write whose context is done, as every real store does.
// Without it a manifest written on an expired context would pass here and fail in production.
type contextDest struct{ *memDest }

func (d contextDest) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	if err := ctx.Err(); err != nil {
		return dest.PutResult{}, err
	}
	return d.memDest.PutImmutable(ctx, key, r, size, ret)
}

// A run's work stops at its deadline, and the run still writes its manifest.
//
// The engine had no deadline of its own, so a wait for a rate limit was bounded only by a stop
// signal, and a run stopped that way writes no manifest: everything it had done went unrecorded.
// With a deadline the work stops, the repositories it did not finish fail with the deadline as
// their error, and the manifest records them.
func TestARunStopsAtItsDeadlineAndStillWritesItsManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	src := &stalledMetadata{
		fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/stalled")},
		stall:         10 * time.Second,
	}
	md := newMemDest(true)
	cfg := testConfig()
	cfg.Source.Repo = ""
	start := time.Now()

	res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: contextDest{md}, Git: gitexec.New(nil),
		SigningKey: testSigner(t), ToolVersion: "test",
		Deadline: start.Add(3 * time.Second),
	})
	if err == nil {
		t.Fatal("a run that ran out of time reported success")
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("the run took %s, well past its three-second deadline", took)
	}
	if res == nil || res.Manifest == nil || res.ManifestKey == "" {
		t.Fatalf("the run that ran out of time wrote no manifest: %v", err)
	}
	if objectsUnder(md, res.ManifestKey) == 0 {
		t.Errorf("the manifest %s is not in the destination", res.ManifestKey)
	}
	entry := res.Manifest.Repos[0]
	if entry.Status != pipeline.StatusFailed || !strings.Contains(entry.Error, "deadline") {
		t.Errorf("octo/stalled = %s %q, want failed on the deadline", entry.Status, entry.Error)
	}
}
