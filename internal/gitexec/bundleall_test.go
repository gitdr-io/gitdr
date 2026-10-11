package gitexec

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A bundle declares every ref of the mirror it is written from, each at the same object, and HEAD
// exactly when HEAD resolves there: what the copy records of the mirror (MirrorRefs). A mirror
// clone of the bundle gives every ref back.
//
// In a mirror whose HEAD names a branch that does not exist, `git bundle create --all HEAD` found no
// revision called HEAD and, run in the mirror, took it for the path of the mirror's own HEAD file.
// No commit touches that path, so every branch was left out: the bundle held the annotated tags
// alone, or nothing, and git refused to write nothing.
func TestABundleCarriesEveryRefAndHEADWhenItResolves(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Chdir(t.TempDir()) // the clone of the bundle below runs here, outside any repository
	ctx := context.Background()
	g := New(nil)

	// source is a bare repository with main checked out and feature one commit behind it.
	source := func(t *testing.T) string {
		t.Helper()
		work := t.TempDir()
		git(t, work, "init", "--quiet", "-b", "main", work)
		for i, name := range []string{"a.txt", "b.txt"} {
			if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, work, "add", name)
			git(t, work, "commit", "--quiet", "-m", name)
			if i == 0 {
				git(t, work, "branch", "feature")
			}
		}
		src := filepath.Join(t.TempDir(), "src.git")
		git(t, work, "clone", "--quiet", "--mirror", work, src)
		return src
	}

	for _, tc := range []struct {
		name string
		// seed changes the source's refs; each args is one git command run in it.
		seed [][]string
		// Whether HEAD resolves in the source, and so in its mirror.
		head bool
	}{
		{name: "HEAD at a branch", head: true},
		{name: "HEAD detached at a commit", seed: [][]string{{"update-ref", "--no-deref", "HEAD", "refs/heads/feature"}}, head: true},
		{name: "HEAD naming a branch that does not exist", seed: [][]string{{"symbolic-ref", "HEAD", "refs/heads/gone"}}},
		{name: "the same, with an annotated tag", seed: [][]string{
			{"tag", "-a", "v1", "-m", "v1", "main"},
			{"symbolic-ref", "HEAD", "refs/heads/gone"},
		}},
		{name: "the same, with an annotated tag and no branch", seed: [][]string{
			{"tag", "-a", "v1", "-m", "v1", "main"},
			{"symbolic-ref", "HEAD", "refs/heads/gone"},
			{"update-ref", "-d", "refs/heads/main"},
			{"update-ref", "-d", "refs/heads/feature"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := source(t)
			for _, args := range tc.seed {
				git(t, src, args...)
			}
			mirror := filepath.Join(t.TempDir(), "m.git")
			if err := g.CloneMirror(ctx, src, mirror, Options{}); err != nil {
				t.Fatalf("clone: %v", err)
			}
			copied, err := g.MirrorRefs(ctx, mirror)
			if err != nil {
				t.Fatalf("MirrorRefs: %v", err)
			}
			if _, ok := copied["HEAD"]; ok != tc.head {
				t.Fatalf("fixture: HEAD resolves in the mirror %v, want %v: %v", ok, tc.head, copied)
			}

			bundle := filepath.Join(t.TempDir(), "r.bundle")
			if err := g.BundleAll(ctx, mirror, bundle); err != nil {
				t.Fatalf("BundleAll: %v", err)
			}
			heads, err := g.BundleHeads(ctx, bundle)
			if err != nil {
				t.Fatalf("BundleHeads: %v", err)
			}
			declared := map[string]string{}
			for _, h := range heads {
				declared[h.Name] = h.OID
			}
			if !maps.Equal(declared, copied) {
				t.Errorf("the bundle declares\n  %v\nand the mirror holds\n  %v", declared, copied)
			}

			// What the bundle declares, it carries: a mirror clone of it holds every ref.
			restored := filepath.Join(t.TempDir(), "restored.git")
			git(t, "", "clone", "--quiet", "--mirror", "--", bundle, restored)
			got, err := g.ListRefs(ctx, restored)
			if err != nil {
				t.Fatalf("ListRefs: %v", err)
			}
			want, err := g.ListRefs(ctx, mirror)
			if err != nil {
				t.Fatalf("ListRefs: %v", err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("a mirror clone of the bundle holds\n  %v\nand the mirror holds\n  %v", got, want)
			}
		})
	}

	t.Run("a repository with no refs has nothing to bundle", func(t *testing.T) {
		// What the pipeline never asks (HasRefs), asserted so an empty bundle cannot slip in.
		empty := t.TempDir()
		git(t, empty, "init", "--bare", "--quiet", empty)
		if err := g.BundleAll(ctx, empty, filepath.Join(t.TempDir(), "empty.bundle")); err == nil {
			t.Error("git wrote a bundle of a repository with no refs")
		}
	})
}

// HeadResolves is git's word on whether HEAD resolves, and a failure is never read as a HEAD that
// does not: that would bundle a repository git could not read as one without HEAD, and take a
// restore git could not read for one with nothing checked out.
func TestHeadResolves(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)

	// committed is a working repository with one commit on main, checked out.
	committed := func(t *testing.T) string {
		t.Helper()
		work := t.TempDir()
		git(t, work, "init", "--quiet", "-b", "main", work)
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "a.txt")
		git(t, work, "commit", "--quiet", "-m", "a")
		return work
	}
	for _, tc := range []struct {
		name string
		repo func(t *testing.T) string
		want bool
	}{
		{name: "a branch checked out", repo: committed, want: true},
		{name: "a detached HEAD", repo: func(t *testing.T) string {
			work := committed(t)
			git(t, work, "checkout", "--quiet", "--detach")
			return work
		}, want: true},
		{name: "a HEAD naming a branch that does not exist", repo: func(t *testing.T) string {
			work := committed(t)
			git(t, work, "symbolic-ref", "HEAD", "refs/heads/gone")
			return work
		}},
		{name: "a repository with no commits", repo: func(t *testing.T) string {
			dir := t.TempDir()
			git(t, dir, "init", "--bare", "--quiet", dir)
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := g.HeadResolves(ctx, tc.repo(t))
			if err != nil {
				t.Fatalf("HeadResolves: %v", err)
			}
			if got != tc.want {
				t.Errorf("HeadResolves = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("errors on a directory that is not a repository", func(t *testing.T) {
		if _, err := g.HeadResolves(ctx, t.TempDir()); err == nil {
			t.Error("HeadResolves read a directory that is not a repository as a HEAD that does not resolve")
		}
	})
}
