package pipeline_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

// The refs a copy records are the refs of the mirror its bundle was made from, the clone is held to
// what the source advertised on either side of it, and the bundle to the clone.

// racingSource is a fixture source whose repository changes while a backup copies it. A run asks
// its source for a git credential immediately before each git command that talks to the source, so
// that is where the changes go in: before is called with the number of the command about to start.
// In a run of one repository with LFS off, 1 is the run's preflight, 2 the ls-remote, 3 the clone,
// and 4 the second listing a run makes when the clone lacks a ref the first one had. An error from
// before is the credential failing.
type racingSource struct {
	*fixtureSource
	before func(call int) error

	mu    sync.Mutex
	calls int
}

func (s *racingSource) GitAuthHeader(context.Context) (string, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if s.before != nil {
		if err := s.before(call); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (s *racingSource) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// gitIn runs git in dir to change a fixture, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if gitDuring(t, dir, args...) != nil {
		t.FailNow()
	}
}

// gitDuring is gitIn for a change made while a backup runs. The hook that makes it runs on one of
// the backup's goroutines, where a test can say it failed but cannot stop, so the error also goes
// back to the hook, to hand to the run.
func gitDuring(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("git %v: %v: %s", args, err, out)
		return err
	}
	return nil
}

// refMap is a manifest entry's refs as the map a listing gives.
func refMap(entries []pipeline.RefEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Name] = e.Commit
	}
	return out
}

// sourceNow is what the source advertises now, read the way the next run reads it.
func sourceNow(t *testing.T, repoDir string) map[string]string {
	t.Helper()
	refs, err := gitexec.New(nil).LsRemote(context.Background(), repoDir, gitexec.Options{})
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	return refs
}

// A push that lands while a repository is being copied is recorded with the copy that holds it.
//
// The refs recorded were the ls-remote taken before the clone, and on a clone that takes hours a
// busy repository is pushed to in between most nights. The copy then held what the record did not
// say: a branch moved in between was recorded at the commit before, a branch deleted in between was
// recorded as present. A drill failed the good copy on the source join, and the next day copied the
// repository again, although nothing had moved since the copy was made.
func TestAPushDuringTheCloneIsRecordedWithTheCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	gitIn(t, repoDir, "branch", "gone")
	// An annotated tag and a pull request ref, so the record is held to the shape a listing has
	// beyond branches: the tag as its tag object, and a namespace a clone of the bundle drops.
	gitIn(t, repoDir, "tag", "-a", "v1", "-m", "v1")
	gitIn(t, repoDir, "update-ref", "refs/pull/1/head", "HEAD")

	pushed := false
	src := &racingSource{fixtureSource: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/hello")}}
	src.before = func(call int) error {
		if call != 3 { // 3: the ls-remote is done, and the clone starts next
			return nil
		}
		pushed = true
		if err := os.WriteFile(filepath.Join(repoDir, "pushed.txt"), []byte("pushed"), 0o600); err != nil {
			t.Error(err)
			return err
		}
		for _, args := range [][]string{
			{"add", "pushed.txt"}, {"commit", "-q", "-m", "pushed"},
			{"branch", "-D", "gone"}, {"branch", "new"},
		} {
			if err := gitDuring(t, repoDir, args...); err != nil {
				return err
			}
		}
		return nil
	}
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	day1 := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	run := func(at time.Time) *pipeline.BackupResult {
		t.Helper()
		md.storeAt(at)
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
		})
		if err != nil {
			t.Fatalf("backup at %s: %v", at.Format(time.RFC3339), err)
		}
		return res
	}

	first := run(day1)
	if !pushed {
		t.Fatal("the clone never asked for its credential, so nothing was pushed while it ran")
	}
	entry := first.Manifest.Repos[0]
	if entry.Status != pipeline.StatusSuccess {
		t.Fatalf("the copy = %s %q, want success", entry.Status, entry.Error)
	}
	// What the source advertises now is what the clone received: the push is in the copy, and the
	// record says so, HEAD included, in the shape the next run's listing has.
	now := sourceNow(t, repoDir)
	if now["HEAD"] == "" || now["refs/heads/new"] == "" || now["refs/heads/gone"] != "" {
		t.Fatalf("the source does not show the push: %v", now)
	}
	if got := refMap(entry.Refs); !maps.Equal(got, now) {
		t.Errorf("the copy records\n  %v\nand the clone received\n  %v", got, now)
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return day1.Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: first.ManifestKey, WorkDir: t.TempDir()})
	if drilled == nil {
		t.Fatalf("drill: %v", err)
	}
	r := drilled.Report.Repos[0]
	if err != nil || r.Status != pipeline.StatusSuccess || r.SourceMatch == nil || !*r.SourceMatch {
		t.Errorf("the drill failed a good copy: %v\n  %s %q, sourceMatch %v, mismatches %v", err, r.Status, r.Error, r.SourceMatch, r.Mismatches)
	}

	// Nothing has moved since the copy was made, so the next day copies nothing.
	bundles := bundleCount(md)
	next := run(day1.AddDate(0, 0, 1)).Manifest.Repos[0]
	if next.Status != pipeline.StatusSkipped || next.Reason != pipeline.ReasonUnchanged+" 2026-06-13" {
		t.Errorf("the next day = %s %q, want skipped as %q", next.Status, next.Reason, pipeline.ReasonUnchanged+" 2026-06-13")
	}
	if got := bundleCount(md); got != bundles {
		t.Errorf("the next day wrote %d bundles of an unchanged repository", got-bundles)
	}
}

// A clone that lacks a branch or a tag the source advertised before it started, and still
// advertises once it is done, fails the repository, and nothing is written for it.
//
// The record now comes from the clone, so it cannot show a clone that received less than the
// source has: a drill compares the restore with the record, and the two lack the same branch. The
// listing taken before the clone can, and the clone is held to it. A ref the clone lacks passes
// only when the source no longer advertises it either, which is a ref deleted while the clone ran,
// and the source is asked that again only when the clone lacks one.
func TestACloneThatLacksARefTheSourceStillHasFails(t *testing.T) {
	hide := func(t *testing.T, repoDir string) error {
		return gitDuring(t, repoDir, "config", "uploadpack.hideRefs", "refs/heads/dev")
	}
	for _, tc := range []struct {
		name string
		// before runs as the run asks for the credential of its call'th git command; see racingSource.
		before func(t *testing.T, repoDir string, call int) error
		// What the copy fails with, every string of it; none when the copy succeeds.
		wantErr []string
		// How many git commands asked for a credential.
		wantAsked int
	}{
		{
			name:      "nothing changes while the clone runs",
			before:    func(*testing.T, string, int) error { return nil },
			wantAsked: 3,
		},
		{
			// Nothing to hold the clone to, and the copy still records what it holds. Up to
			// v0.1.22 it recorded no refs and no copiedAt, and the next day copied it in full.
			name: "the listing before the clone fails",
			before: func(_ *testing.T, _ string, call int) error {
				if call == 2 {
					return errors.New("source.github.tokenPath: the token file is being replaced")
				}
				return nil
			},
			wantAsked: 3,
		},
		{
			name: "a branch deleted while the clone runs",
			before: func(t *testing.T, repoDir string, call int) error {
				if call == 3 {
					return gitDuring(t, repoDir, "branch", "-D", "dev")
				}
				return nil
			},
			wantAsked: 4,
		},
		{
			name: "a branch the clone did not receive, advertised before and after it",
			before: func(t *testing.T, repoDir string, call int) error {
				switch call {
				case 3:
					return hide(t, repoDir)
				case 4:
					return gitDuring(t, repoDir, "config", "--unset", "uploadpack.hideRefs")
				}
				return nil
			},
			wantErr:   []string{"the clone lacks refs/heads/dev, which the source advertised before the clone and still advertises"},
			wantAsked: 4,
		},
		{
			name: "a branch the clone did not receive, and a source that cannot be asked again",
			before: func(t *testing.T, repoDir string, call int) error {
				switch call {
				case 3:
					return hide(t, repoDir)
				case 4:
					return errors.New("source.github.tokenPath: the token file is gone")
				}
				return nil
			},
			wantErr: []string{
				"the clone lacks refs/heads/dev, which the source advertised before the clone",
				"could not be asked whether it still does",
				"the token file is gone",
			},
			wantAsked: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			repoDir := initFixtureRepo(t)
			gitIn(t, repoDir, "branch", "dev")
			md := newMemDest(true)
			signer := testSigner(t)
			cfg := testConfig()
			cfg.Backup.LFS = false // so the source is asked for nothing else
			run := func(src *racingSource, at time.Time) (*pipeline.BackupResult, error) {
				md.storeAt(at)
				return pipeline.Backup(context.Background(), pipeline.BackupDeps{
					Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
					SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
				})
			}
			repos := slugRepos("github.com", repoDir, "octo/hello")
			morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)

			src := &racingSource{fixtureSource: &fixtureSource{repos: repos}}
			src.before = func(call int) error { return tc.before(t, repoDir, call) }
			res, err := run(src, morning)
			if got := src.asked(); got != tc.wantAsked {
				t.Errorf("the source was asked for a credential %d times, want %d", got, tc.wantAsked)
			}
			if res == nil || res.Manifest == nil {
				t.Fatalf("no manifest: %v", err)
			}
			entry := res.Manifest.Repos[0]

			if len(tc.wantErr) == 0 {
				if err != nil || entry.Status != pipeline.StatusSuccess {
					t.Fatalf("the copy = %s %q (%v), want success", entry.Status, entry.Error, err)
				}
				if got, now := refMap(entry.Refs), sourceNow(t, repoDir); !maps.Equal(got, now) {
					t.Errorf("the copy records\n  %v\nand the source advertises\n  %v", got, now)
				}
				if entry.CopiedAt == nil {
					t.Error("the copy records no copiedAt")
				}
				return
			}

			if err == nil || entry.Status != pipeline.StatusFailed {
				t.Fatalf("the copy = %s (%v), want it failed: the clone lacks a branch the source has", entry.Status, err)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(entry.Error, want) {
					t.Errorf("the error does not say %q:\n  %s", want, entry.Error)
				}
			}
			if len(entry.Refs) != 0 || entry.CopiedAt != nil {
				t.Errorf("a failed copy records %d refs and copiedAt %v", len(entry.Refs), entry.CopiedAt)
			}
			if got := storedNames(md, "github.com/octo/hello/"); len(got) != 0 {
				t.Fatalf("the failed copy wrote %v", got)
			}

			// Nothing was written, so the same day's rerun copies the repository cleanly, and the
			// branch with it.
			_ = exec.Command("git", "-C", repoDir, "config", "--unset", "uploadpack.hideRefs").Run()
			rerun, err := run(&racingSource{fixtureSource: &fixtureSource{repos: repos}}, morning.Add(time.Hour))
			if err != nil || rerun.Manifest.Repos[0].Status != pipeline.StatusSuccess {
				t.Fatalf("the same day's rerun did not copy the repository: %v", err)
			}
			if refMap(rerun.Manifest.Repos[0].Refs)["refs/heads/dev"] == "" {
				t.Errorf("the rerun's copy records no refs/heads/dev: %v", rerun.Manifest.Repos[0].Refs)
			}
		})
	}
}
