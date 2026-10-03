package pipeline_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// A second run on the same day finds the copy the first one made and leaves the repository alone,
// whatever that copy is: a bundle for a repository with commits, the metadata alone for one
// without.
//
// The check used to look for the bundle only. A repository nobody has pushed to has no bundle, so
// the rerun backed it up again and wrote its metadata to a key the first run had already written.
// The destination is create-only, which is the point of it, so the repository failed, the run
// failed with it, and so did every rerun until the date changed.
func TestASameDayRerunSkipsWhatTheFirstRunCopied(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo func(*testing.T) string
		// What the first run records for the repository, and the artifacts it writes.
		status, reason string
		kinds          []string
	}{
		{"a repository with commits", initFixtureRepo, pipeline.StatusSuccess, "", []string{"bundle", "meta", "sha256"}},
		{"a repository with no commits", initEmptyRepo, pipeline.StatusSkipped, pipeline.ReasonEmpty, []string{"meta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			ctx := context.Background()
			src := &fixtureSource{repos: []source.Repo{{
				Host: "github.com", Owner: "octo", Name: "hello", CloneURL: tc.repo(t),
			}}}
			md := newMemDest(true)
			signer := testSigner(t)
			cfg := testConfig()
			if !cfg.Backup.Resume {
				t.Fatal("resume is off by default, and this test is about the resume path")
			}
			run := func(at time.Time) (*pipeline.BackupResult, error) {
				md.storeAt(at)
				return pipeline.Backup(ctx, pipeline.BackupDeps{
					Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
					SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
				})
			}
			morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
			const repoPrefix = "github.com/octo/hello/"

			first, err := run(morning)
			if err != nil {
				t.Fatalf("first run: %v", err)
			}
			entry := first.Manifest.Repos[0]
			if entry.Status != tc.status || entry.Reason != tc.reason {
				t.Fatalf("first run = %s %q, want %s %q", entry.Status, entry.Reason, tc.status, tc.reason)
			}
			var kinds []string
			for _, a := range entry.Artifacts {
				kinds = append(kinds, a.Kind)
			}
			if strings.Join(kinds, ",") != strings.Join(tc.kinds, ",") {
				t.Fatalf("first run wrote %v, want %v", kinds, tc.kinds)
			}
			copied := objectsUnder(md, repoPrefix)

			second, err := run(morning.Add(8 * time.Hour))
			if err != nil {
				t.Fatalf("second run the same day: %v", err)
			}
			entry = second.Manifest.Repos[0]
			if entry.Status != pipeline.StatusSkipped || entry.Reason != pipeline.ReasonResume {
				t.Errorf("second run = %s %q %s, want skipped as %q", entry.Status, entry.Reason, entry.Error, pipeline.ReasonResume)
			}
			if got := objectsUnder(md, repoPrefix); got != copied {
				t.Errorf("the repository has %d objects after the second run, want the first run's %d", got, copied)
			}
		})
	}
}

// initEmptyRepo makes a repository that was created and never pushed to: no commits and no refs.
func initEmptyRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed; in CI the empty repository must be built, not skipped")
		}
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// objectsUnder counts what md holds under prefix.
func objectsUnder(md *memDest, prefix string) int {
	md.mu.Lock()
	defer md.mu.Unlock()
	n := 0
	for key := range md.objs {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

// hideLFSObjects moves a fixture repository's LFS objects away, so `git lfs fetch --all` from a
// clone of it fails on the missing object, and returns the function that puts them back.
func hideLFSObjects(t *testing.T, repoDir string) (restore func()) {
	t.Helper()
	objects := filepath.Join(repoDir, ".git", "lfs", "objects")
	hidden := filepath.Join(t.TempDir(), "hidden-lfs-objects")
	if err := os.Rename(objects, hidden); err != nil {
		t.Fatalf("hide the fixture's LFS objects: %v", err)
	}
	return func() {
		t.Helper()
		// git-lfs makes the remote's object directory again, empty, when a fetch looks in it.
		if err := os.RemoveAll(objects); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(hidden, objects); err != nil {
			t.Fatalf("put the fixture's LFS objects back: %v", err)
		}
	}
}

// A same-day rerun fails by name a repository whose first copy did not finish, and never skips it.
//
// The rerun took any bundle under the date as a finished copy. So when the first run stored the
// bundle and then failed, on its LFS objects (QA AC1), on its metadata (AC2) or on its checksum,
// the rerun reported the repository as already backed up, exited 0, and the repository counted
// as protected with half a copy. A rerun cannot finish that copy either, because its keys are
// create-only, so the next copy is the next day's.
//
// Since every read of the source comes before the first write, an LFS fetch that fails writes
// nothing at all (TestEverySourceReadHappensBeforeTheFirstWrite). An LFS repository can still be
// left half copied by an upload that fails, which is the case here.
func TestASameDayRerunFailsAnIncompleteCopyByName(t *testing.T) {
	for _, tc := range []struct {
		name string
		lfs  bool
		// fault makes the first run fail part way, and returns what lifts the fault again.
		fault func(t *testing.T, md *memDest, src *failingMetadata, repoDir string) (lift func())
		left  []string // what the first run leaves under the date
	}{
		{
			// The archive is the largest artifact, so it is written first, and the bundle after it.
			name: "its bundle was refused after its lfs archive landed",
			lfs:  true,
			fault: func(_ *testing.T, md *memDest, _ *failingMetadata, _ string) func() {
				md.refuse = "hello.bundle"
				return func() { md.refuse = "" }
			},
			left: []string{"hello.lfs.tar"},
		},
		{
			name: "its metadata could not be fetched",
			fault: func(_ *testing.T, _ *memDest, src *failingMetadata, _ string) func() {
				src.fails = "hello"
				return func() { src.fails = "" }
			},
			left: []string{"hello.bundle", "hello.sha256"},
		},
		{
			name: "its checksum was refused",
			fault: func(_ *testing.T, md *memDest, _ *failingMetadata, _ string) func() {
				md.refuse = "hello.sha256"
				return func() { md.refuse = "" }
			},
			left: []string{"hello.bundle", "hello.meta.json"},
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
			src := &failingMetadata{
				fixtureSource: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/hello")},
				err:           errors.New("github: issues: GET .../issues: 403 Resource not accessible by integration"),
			}
			md := newMemDest(true)
			signer := testSigner(t)
			cfg := testConfig()
			run := func(at time.Time) (*pipeline.BackupResult, error) {
				md.storeAt(at)
				return pipeline.Backup(context.Background(), pipeline.BackupDeps{
					Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
					SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
				})
			}
			morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
			const dated = "github.com/octo/hello/2026-06-13/"

			lift := tc.fault(t, md, src, repoDir)
			first, err := run(morning)
			if err == nil || first == nil || first.Manifest.Repos[0].Status != pipeline.StatusFailed {
				t.Fatalf("the first run did not fail: %v", err)
			}
			if got := storedNames(md, dated); !slices.Equal(got, tc.left) {
				t.Fatalf("the first run left %v under the date, want %v", got, tc.left)
			}
			lift()

			second, err := run(morning.Add(6 * time.Hour))
			if err == nil {
				t.Error("the rerun exited 0 over a copy that never finished")
			}
			if second == nil || second.Manifest == nil {
				t.Fatalf("the rerun wrote no manifest: %v", err)
			}
			entry := second.Manifest.Repos[0]
			const want = "an incomplete copy for 2026-06-13 exists"
			if entry.Status != pipeline.StatusFailed || !strings.Contains(entry.Error, want) || !strings.Contains(entry.Error, "the next copy is on 2026-06-14") {
				t.Errorf("rerun = %s %q %q, want failed with %q", entry.Status, entry.Reason, entry.Error, want)
			}
			if got := storedNames(md, dated); !slices.Equal(got, tc.left) {
				t.Errorf("the rerun changed what is under the date: %v, want %v", got, tc.left)
			}

			// And the next day copies it in full, as the error says.
			next, err := run(morning.AddDate(0, 0, 1))
			if err != nil || next.Manifest.Repos[0].Status != pipeline.StatusSuccess {
				t.Fatalf("the next day's run did not copy the repository: %v", err)
			}
		})
	}
}

// A restore by date restores only a copy a run finished.
//
// A repository whose metadata could not be fetched has its bundle and checksum stored and fails.
// Its entry lists those two artifacts. `restore -manifest` refused it, as a repository the run did
// not copy, while a restore by date took the same entry and restored the bundle as verified.
func TestARestoreByDateTakesOnlyACopy(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	src := &failingMetadata{
		fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/broken")},
		fails:         "broken",
		err:           errors.New("github: issues: GET .../issues: 403 Resource not accessible by integration"),
	}
	cfg := testConfig()
	cfg.Source.Repo = "octo/broken"
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err == nil || res == nil || res.Manifest.Repos[0].Status != pipeline.StatusFailed {
		t.Fatalf("the run did not fail on the repository's metadata: %v", err)
	}
	if got := storedNames(md, "github.com/octo/broken/"); !slices.Equal(got, []string{"broken.bundle", "broken.sha256"}) {
		t.Fatalf("stored %v, want the bundle and its checksum", got)
	}

	for _, tc := range []struct {
		name string
		req  pipeline.RestoreRequest
	}{
		{"by date", pipeline.RestoreRequest{Host: "github.com", Owner: "octo", Name: "broken", Date: "2026-06-13"}},
		{"by manifest", pipeline.RestoreRequest{ManifestKey: res.ManifestKey, Owner: "octo", Name: "broken"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.OutDir = filepath.Join(t.TempDir(), "restored")
			_, err := pipeline.Restore(ctx, restoreDeps(md, pub), tc.req)
			if err == nil || !strings.Contains(err.Error(), `records "octo/broken" as failed`) {
				t.Errorf("err = %v, want a refusal of the entry of a repository that failed", err)
			}
		})
	}
}

// Every same-day rerun skips the copy the first run recorded, and carries that copy's refs and
// copiedAt, so the next day still skips an unchanged repository.
//
// The skip carried nothing. A third attempt found the second's manifest newest, which records a
// skip and no copy (RT 4), and the next day found that skip without refs and copied the whole
// repository again.
func TestEverySameDayRerunCarriesTheCopyItReliesOn(t *testing.T) {
	t.Chdir(t.TempDir())
	md := newMemDest(true)
	_, signer := drillKeys(t)
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/hello")
	morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)

	first := backupAt(t, md, signer, morning, repos)
	copied := first.Manifest.Repos[0]
	if copied.Status != pipeline.StatusSuccess || copied.CopiedAt == nil || len(copied.Refs) == 0 {
		t.Fatalf("the first run = %s, copiedAt %v, %d refs; want a copy with both", copied.Status, copied.CopiedAt, len(copied.Refs))
	}

	for _, at := range []time.Time{morning.Add(3 * time.Hour), morning.Add(6 * time.Hour)} {
		res := backupAt(t, md, signer, at, repos)
		e := res.Manifest.Repos[0]
		if e.Status != pipeline.StatusSkipped || e.Reason != pipeline.ReasonResume {
			t.Fatalf("the rerun at %s = %s %q %s, want skipped as %q", at.Format("15:04"), e.Status, e.Reason, e.Error, pipeline.ReasonResume)
		}
		if e.CopiedAt == nil || !e.CopiedAt.Equal(*copied.CopiedAt) {
			t.Errorf("the rerun at %s carries copiedAt %v, want the copy's %s", at.Format("15:04"), e.CopiedAt, copied.CopiedAt)
		}
		if !slices.Equal(e.Refs, copied.Refs) {
			t.Errorf("the rerun at %s carries refs %v, want the copy's %v", at.Format("15:04"), e.Refs, copied.Refs)
		}
	}

	next := backupAt(t, md, signer, morning.AddDate(0, 0, 1), repos).Manifest.Repos[0]
	if next.Status != pipeline.StatusSkipped || next.Reason != pipeline.ReasonUnchanged+" 2026-06-13" {
		t.Errorf("the next day = %s %q, want skipped as %q", next.Status, next.Reason, pipeline.ReasonUnchanged+" 2026-06-13")
	}
}

// A same-day rerun skips only when what is under the date is exactly the copy a manifest records.
// An object the manifest does not list, or a listed one that is gone, fails the repository by
// name: the copy a restore would find is not the one that was recorded.
func TestASameDayRerunFailsWhatTheRecordDoesNotCover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(md *memDest)
		want   string
	}{
		{
			name: "an object the manifest does not record",
			change: func(md *memDest) {
				const stray = "github.com/octo/hello/2026-06-13/hello.lfs.tar"
				md.objs[stray] = []byte("not from this run")
				md.modified[stray] = time.Date(2026, 6, 13, 9, 30, 0, 0, time.UTC)
			},
			want: "github.com/octo/hello/2026-06-13/hello.lfs.tar is under the date, and github.com/octo/manifests/20260613T090000Z.manifest.json does not record it",
		},
		{
			name: "an object the manifest records is gone",
			change: func(md *memDest) {
				delete(md.objs, "github.com/octo/hello/2026-06-13/hello.sha256")
			},
			want: "github.com/octo/manifests/20260613T090000Z.manifest.json records github.com/octo/hello/2026-06-13/hello.sha256, and the destination does not have it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			md := newMemDest(true)
			signer := testSigner(t)
			repos := slugRepos("github.com", initFixtureRepo(t), "octo/hello")
			morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
			backupAt(t, md, signer, morning, repos)

			md.mu.Lock()
			tc.change(md)
			md.mu.Unlock()

			md.storeAt(morning.Add(time.Hour))
			res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
				Config: testConfig(), Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
				SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return morning.Add(time.Hour) },
			})
			if err == nil {
				t.Error("the rerun exited 0")
			}
			e := res.Manifest.Repos[0]
			if e.Status != pipeline.StatusFailed || !strings.Contains(e.Error, tc.want) {
				t.Errorf("rerun = %s %q %q\nwant failed saying %q", e.Status, e.Reason, e.Error, tc.want)
			}
		})
	}
}
