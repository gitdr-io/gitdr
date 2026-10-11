package pipeline_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

// A repository whose HEAD names a branch that does not exist: a default branch renamed or
// deleted, or an empty default beside other branches. Its copy carries every ref and no HEAD, a
// restore of it holds every branch and checks none out, and its drill is clean.

// defaultBranchNamed keeps the git this test runs, and the git the restore runs, off the machine's
// own configuration, and names the default branch they use. A clone of a bundle that declares no
// HEAD checks out the branch so named when the bundle has one, in the git versions that do, so a
// test that says what a restore checks out has to say what that name is.
func defaultBranchNamed(t *testing.T, name string) {
	t.Helper()
	isolateGit(t)
	f, err := os.OpenFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := fmt.Fprintf(f, "[init]\n\tdefaultBranch = %s\n", name)
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Fatalf("write the default branch: %v %v", werr, cerr)
	}
}

// declaredBy reads the refs the bundle stored for entry declares.
func declaredBy(t *testing.T, md *memDest, entry pipeline.RepoEntry) map[string]string {
	t.Helper()
	i := slices.IndexFunc(entry.Artifacts, func(a pipeline.ArtifactInfo) bool { return a.Kind == "bundle" })
	if i < 0 {
		t.Fatalf("%s records no bundle: %v", entry.Slug, entry.Artifacts)
	}
	md.mu.Lock()
	raw, ok := md.objs[entry.Artifacts[i].Key]
	md.mu.Unlock()
	if !ok {
		t.Fatalf("no object at %s", entry.Artifacts[i].Key)
	}
	p := filepath.Join(t.TempDir(), "stored.bundle")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	heads, err := gitexec.New(nil).BundleHeads(context.Background(), p)
	if err != nil {
		t.Fatalf("read the stored bundle: %v", err)
	}
	declared := make(map[string]string, len(heads))
	for _, h := range heads {
		declared[h.Name] = h.OID
	}
	return declared
}

// restoredWhole checks that a restore holds every branch and tag the source lists, at the same
// object: a branch under the remote-tracking name a clone gives it, a tag under its own.
func restoredWhole(t *testing.T, out string, source map[string]string) {
	t.Helper()
	got, err := gitexec.New(nil).ListRefs(context.Background(), out)
	if err != nil {
		t.Fatalf("read the restore's refs: %v", err)
	}
	for name, oid := range source {
		where := name
		switch branch, isBranch := strings.CutPrefix(name, "refs/heads/"); {
		case name == "HEAD":
			continue
		case isBranch:
			where = "refs/remotes/origin/" + branch
		}
		if got[where] != oid {
			t.Errorf("the restore has %s at %q, and the source has %s at %s", where, got[where], name, oid)
		}
	}
}

// checkedOut names what a restore put beside its .git.
func checkedOut(t *testing.T, out string) []string {
	t.Helper()
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".git" {
			names = append(names, e.Name())
		}
	}
	return names
}

// In a mirror whose HEAD names a branch that does not exist, `git bundle create --all HEAD` finds
// no revision called HEAD and takes it for the path of the mirror's own HEAD file, which no commit
// touches, so every branch is left out. Without an annotated tag git refuses to write the empty
// bundle, and the repository failed every night. With one, the bundle held the annotated tags
// alone, which up to v0.1.22 was stored as a good copy, and which the check on the bundle
// (copiedrefs.go) then failed every night too.
func TestARepositoryWhoseHEADNamesNoBranchIsCopiedWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed shapes the fixture repository, which starts with one commit on main, checked out.
		seed func(t *testing.T, repoDir string)
		// Whether HEAD resolves in the source, and so whether the copy records it, the bundle
		// declares it, and a restore checks a branch out.
		head bool
	}{
		{
			name: "two branches",
			seed: func(t *testing.T, repoDir string) {
				gitIn(t, repoDir, "branch", "feature")
				gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/gone")
			},
		},
		{
			name: "two branches and an annotated tag",
			seed: func(t *testing.T, repoDir string) {
				gitIn(t, repoDir, "branch", "feature")
				gitIn(t, repoDir, "tag", "-a", "v1", "-m", "v1")
				gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/gone")
			},
		},
		{
			name: "an annotated tag and no branch",
			seed: func(t *testing.T, repoDir string) {
				gitIn(t, repoDir, "tag", "-a", "v1", "-m", "v1")
				gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/gone")
				gitIn(t, repoDir, "update-ref", "-d", "refs/heads/main")
			},
		},
		{
			// Unchanged: HEAD is recorded, declared and compared, and the default branch is
			// checked out.
			name: "a repository whose HEAD resolves",
			seed: func(t *testing.T, repoDir string) {
				gitIn(t, repoDir, "branch", "feature")
				gitIn(t, repoDir, "tag", "-a", "v1", "-m", "v1")
			},
			head: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			defaultBranchNamed(t, "trunk") // a branch none of these repositories has
			ctx := context.Background()
			repoDir := initFixtureRepo(t)
			tc.seed(t, repoDir)
			source := sourceNow(t, repoDir)
			if _, ok := source["HEAD"]; ok != tc.head {
				t.Fatalf("the source lists HEAD %v, want %v: %v", ok, tc.head, source)
			}

			md := newMemDest(true)
			pub, signer := drillKeys(t)
			g := gitexec.New(nil)
			cfg := testConfig()
			cfg.Backup.LFS = false
			day := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
			backup := func(at time.Time) (*pipeline.BackupResult, pipeline.RepoEntry) {
				t.Helper()
				md.storeAt(at)
				res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
					Config: cfg, Source: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/hello")},
					Dest: md, Git: g, SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
				})
				if res == nil || res.Manifest == nil {
					t.Fatalf("backup at %s: no manifest: %v", at.Format(time.RFC3339), err)
				}
				entry := res.Manifest.Repos[0]
				if err != nil {
					t.Fatalf("backup at %s: %v: the copy = %s %q", at.Format(time.RFC3339), err, entry.Status, entry.Error)
				}
				return res, entry
			}

			res, entry := backup(day)
			if entry.Status != pipeline.StatusSuccess {
				t.Fatalf("the copy = %s %q, want success", entry.Status, entry.Error)
			}
			// The record and the bundle both hold every ref the source lists, and HEAD exactly
			// when the source lists it.
			if got := refMap(entry.Refs); !maps.Equal(got, source) {
				t.Errorf("the copy records\n  %v\nand the source lists\n  %v", got, source)
			}
			if got := declaredBy(t, md, entry); !maps.Equal(got, source) {
				t.Errorf("the bundle declares\n  %v\nand the source lists\n  %v", got, source)
			}

			out := filepath.Join(t.TempDir(), "restored")
			restored, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: g, PublicKey: pub},
				pipeline.RestoreRequest{ManifestKey: res.ManifestKey, Owner: "octo", Name: "hello", OutDir: out})
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			restoredWhole(t, out, source)
			files := checkedOut(t, out)
			said := strings.Contains(restored.Verification, "nothing is checked out")
			if tc.head {
				if !slices.Equal(files, []string{"README.md"}) || said {
					t.Errorf("the restore checked out %v and says %q; want the default branch checked out", files, restored.Verification)
				}
			} else if len(files) > 0 || !said {
				t.Errorf("the restore checked out %v and says %q; want nothing checked out, and said", files, restored.Verification)
			}

			drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
				Dest: md, Git: g, PublicKey: pub, ToolVersion: "test",
				Now: func() time.Time { return day.Add(time.Hour) },
			}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
			if drilled == nil {
				t.Fatalf("drill: %v", err)
			}
			r := drilled.Report.Repos[0]
			if err != nil || r.Status != pipeline.StatusSuccess || r.SourceMatch == nil || !*r.SourceMatch || len(r.Mismatches) > 0 {
				t.Errorf("the drill = %s %q (%v), sourceMatch %v, mismatches %v", r.Status, r.Error, err, r.SourceMatch, r.Mismatches)
			}
			if n := len(source); r.SourceRefs != n || r.BundleRefs != n || r.RestoredRefs != n {
				t.Errorf("the drill compared %d source, %d bundle and %d restored refs, want %d of each", r.SourceRefs, r.BundleRefs, r.RestoredRefs, n)
			}

			// And it is not copied again the next day: the record is what the next run lists.
			bundles := bundleCount(md)
			_, next := backup(day.AddDate(0, 0, 1))
			if want := pipeline.ReasonUnchanged + " 2026-06-13"; next.Status != pipeline.StatusSkipped || next.Reason != want {
				t.Errorf("the next day = %s %q, want skipped as %q", next.Status, next.Reason, want)
			}
			if got := bundleCount(md); got != bundles {
				t.Errorf("the next day wrote %d bundles of an unchanged repository", got-bundles)
			}
		})
	}
}

// The same with LFS content. With no branch checked out there is no tree for the LFS files: the
// objects are restored and the filters installed, and a branch checked out from the restore has its
// files. `git lfs checkout` fails on a HEAD that resolves to nothing, "Git can't resolve ref", and
// with it the restore and the drill.
func TestAnLFSRepositoryWhoseHEADNamesNoBranchRestores(t *testing.T) {
	requireLFS(t)
	t.Chdir(t.TempDir())
	defaultBranchNamed(t, "trunk")
	ctx := context.Background()
	repoDir, want := initLFSFixture(t)
	gitIn(t, repoDir, "branch", "feature")
	gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/gone")

	md := newMemDest(true)
	pub, signer := drillKeys(t)
	g := gitexec.New(nil)
	cfg := testConfig()
	cfg.Source.Repo = "octo/lfsrepo"
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: slugRepos("github.com", repoDir, "octo/lfsrepo")},
		Dest: md, Git: g, SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if res == nil || res.Manifest == nil {
		t.Fatalf("backup: no manifest: %v", err)
	}
	entry := res.Manifest.Repos[0]
	if err != nil || entry.Status != pipeline.StatusSuccess {
		t.Fatalf("the copy = %s %q (%v), want success", entry.Status, entry.Error, err)
	}
	if !slices.ContainsFunc(entry.Artifacts, func(a pipeline.ArtifactInfo) bool { return a.Kind == "lfs" }) {
		t.Fatalf("the copy stored no LFS archive: %v", entry.Artifacts)
	}

	out := filepath.Join(t.TempDir(), "restored")
	restored, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: g, PublicKey: pub},
		pipeline.RestoreRequest{ManifestKey: res.ManifestKey, Owner: "octo", Name: "lfsrepo", OutDir: out})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, say := range []string{"bundle and lfs tar verified against the signed manifest", "nothing is checked out"} {
		if !strings.Contains(restored.Verification, say) {
			t.Errorf("the restore says %q, and not %q", restored.Verification, say)
		}
	}
	if files := checkedOut(t, out); len(files) > 0 {
		t.Errorf("the restore checked out %v, want nothing", files)
	}
	// A branch checked out from the restore has its LFS file, from the objects restored beside it.
	gitIn(t, out, "checkout", "--quiet", "-b", "feature", "origin/feature")
	got, err := os.ReadFile(filepath.Join(out, "big.bin"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("big.bin, checked out from the restore, is %d bytes (%v), want the %d of its content", len(got), err, len(want))
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: g, PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return fixedClock()().Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if drilled == nil {
		t.Fatalf("drill: %v", err)
	}
	if r := drilled.Report.Repos[0]; err != nil || r.Status != pipeline.StatusSuccess || r.SourceMatch == nil || !*r.SourceMatch {
		t.Errorf("the drill = %s %q (%v), sourceMatch %v, mismatches %v", r.Status, r.Error, err, r.SourceMatch, r.Mismatches)
	}
}
