package pipeline_test

import (
	"context"
	"os"
	"os/exec"
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
