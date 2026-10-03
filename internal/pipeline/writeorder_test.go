package pipeline_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// The order a copy is made in: every read of the source first, then the uploads.

// faultySource is a fixture source whose clone URL can be made to point nowhere.
type faultySource struct {
	*failingMetadata
	cloneFails bool
}

func (s *faultySource) CloneURL(ctx context.Context, r source.Repo) (string, error) {
	if s.cloneFails {
		return filepath.Join(r.CloneURL, "no-such-repository"), nil
	}
	return s.failingMetadata.CloneURL(ctx, r)
}

// A copy that fails before its first upload leaves nothing under the date, whatever it failed on,
// and the same day's rerun copies the repository cleanly.
//
// The bundle, the metadata and the checksum used to be stored before the LFS objects were fetched
// and archived. A failure there, an expired token or a missing object, left three objects under
// the date that no rerun that day could finish, because their keys are create-only.
func TestEverySourceReadHappensBeforeTheFirstWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		lfs  bool
		// fault makes the first run fail before it writes, and returns what lifts it again.
		fault func(t *testing.T, src *faultySource, repoDir string) (lift func())
	}{
		{
			name: "at the clone",
			fault: func(_ *testing.T, src *faultySource, _ string) func() {
				src.cloneFails = true
				return func() { src.cloneFails = false }
			},
		},
		{
			name: "at the lfs fetch",
			lfs:  true,
			fault: func(t *testing.T, _ *faultySource, repoDir string) func() {
				return hideLFSObjects(t, repoDir)
			},
		},
		{
			name: "at the metadata, on a rate limit that could not be waited out",
			fault: func(_ *testing.T, src *faultySource, _ string) func() {
				src.fails = "repo"
				src.err = source.Transient(errors.New("github: issues: 403 API rate limit exceeded, past the deadline"))
				return func() { src.fails = "" }
			},
		},
		{
			// No fixture makes the archive fail on its own, so the archiver is replaced: a disk
			// that fills while the LFS objects are written into it.
			name: "at the lfs archive",
			lfs:  true,
			fault: func(*testing.T, *faultySource, string) func() {
				return pipeline.FailLFSArchiveForTest(errors.New("write lfs.tar: no space left on device"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			repoDir := ""
			if tc.lfs {
				requireLFS(t)
				repoDir, _ = initLFSFixture(t)
			} else {
				repoDir = initFixtureRepo(t)
			}
			src := &faultySource{failingMetadata: &failingMetadata{fixtureSource: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/repo")}}}
			md := newMemDest(true)
			cfg := testConfig()
			cfg.Source.Repo = ""
			deps := pipeline.BackupDeps{Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil), SigningKey: testSigner(t), ToolVersion: "test"}
			morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
			const dated = "github.com/octo/repo/2026-06-13/"

			// Lifted before the rerun, and in any case when the test ends, so a fault cannot outlive
			// a failure here.
			lift := sync.OnceFunc(tc.fault(t, src, repoDir))
			t.Cleanup(lift)
			deps.Now = func() time.Time { return morning }
			md.storeAt(morning)
			first, err := pipeline.Backup(context.Background(), deps)
			if err == nil || first == nil || first.Manifest.Repos[0].Status != pipeline.StatusFailed {
				t.Fatalf("the first run did not fail: %v", err)
			}
			if got := storedNames(md, dated); len(got) != 0 {
				t.Fatalf("the failed copy left %v under the date, want nothing: it failed %s", got, tc.name)
			}
			lift()

			deps.Now = func() time.Time { return morning.Add(time.Hour) }
			md.storeAt(morning.Add(time.Hour))
			second, err := pipeline.Backup(context.Background(), deps)
			if err != nil || second.Manifest.Repos[0].Status != pipeline.StatusSuccess {
				t.Fatalf("the same day's rerun did not copy the repository: %v %+v", err, second.Manifest.Repos)
			}
		})
	}
}

// orderedDest is a memDest that records the order of its writes.
type orderedDest struct {
	*memDest
	mu   sync.Mutex
	puts []dest.Object
}

func (o *orderedDest) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	res, err := o.memDest.PutImmutable(ctx, key, r, size, ret)
	if err == nil {
		o.mu.Lock()
		o.puts = append(o.puts, dest.Object{Key: key, Size: size})
		o.mu.Unlock()
	}
	return res, err
}

// A copy is uploaded largest first, and the checksum sidecar last.
//
// Largest first, because the longest upload is the likeliest to fail and then fails with the least
// written beside it. The sidecar last, because a restore without the public key goes by the
// sidecar, and a sidecar beside a partial copy would vouch for it.
func TestACopyIsUploadedLargestFirstAndItsSidecarLast(t *testing.T) {
	requireLFS(t)
	t.Chdir(t.TempDir())
	repoDir, _ := initLFSFixture(t)
	md := &orderedDest{memDest: newMemDest(true)}
	cfg := testConfig()
	cfg.Source.Repo = ""
	res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/lfsrepo")}, Dest: md,
		Git: gitexec.New(nil), SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	var copied []dest.Object
	for _, p := range md.puts {
		if strings.HasPrefix(p.Key, "github.com/octo/lfsrepo/") {
			copied = append(copied, p)
		}
	}
	if len(copied) != 4 {
		t.Fatalf("wrote %v, want the bundle, the metadata, the lfs archive and the sidecar", copied)
	}
	if last := copied[len(copied)-1]; !strings.HasSuffix(last.Key, ".sha256") {
		t.Errorf("the last write was %s, want the sha256 sidecar: %v", last.Key, copied)
	}
	for i := 1; i < len(copied)-1; i++ {
		if copied[i].Size > copied[i-1].Size {
			t.Errorf("%s (%d bytes) was written after %s (%d bytes), want the largest first", copied[i].Key, copied[i].Size, copied[i-1].Key, copied[i-1].Size)
		}
	}
	// The manifest lists the artifacts the way it always has, whatever order they were written in.
	var kinds []string
	for _, a := range res.Manifest.Repos[0].Artifacts {
		kinds = append(kinds, a.Kind)
	}
	if got := strings.Join(kinds, ","); got != "bundle,meta,sha256,lfs" {
		t.Errorf("the manifest lists %s, want bundle,meta,sha256,lfs", got)
	}
}
