package gitexec

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What a clone received, read back from the mirror it made, is what ls-remote says of the source:
// the same refs at the same objects, and HEAD among them when it resolves. A backup records the
// first and the next run compares it with the second, so a difference in shape alone would copy
// every repository again every day.
func TestMirrorRefsIsWhatLsRemoteSaysOfTheSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)

	// seeded is a bare source with two branches, a lightweight and an annotated tag, a symbolic
	// ref, and a pull request ref at a commit no branch has.
	seeded := func(t *testing.T) string {
		t.Helper()
		work := t.TempDir()
		git(t, work, "init", "--quiet", "-b", "main", work)
		commitFile := func(name string) {
			if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, work, "add", name)
			git(t, work, "commit", "--quiet", "-m", name)
		}
		commitFile("a.txt")
		git(t, work, "tag", "v1")
		git(t, work, "tag", "-a", "annotated", "-m", "an annotated tag")
		git(t, work, "branch", "feature")
		commitFile("b.txt")
		git(t, work, "checkout", "--quiet", "-b", "pr")
		commitFile("c.txt")
		git(t, work, "update-ref", "refs/pull/1/head", "HEAD")
		git(t, work, "checkout", "--quiet", "main")
		git(t, work, "branch", "-D", "pr")

		src := filepath.Join(t.TempDir(), "src.git")
		git(t, work, "clone", "--quiet", "--mirror", work, src)
		git(t, src, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
		return src
	}

	for _, tc := range []struct {
		name   string
		source func(t *testing.T) string
		// Whether the source's HEAD resolves, and so whether ls-remote lists it.
		head bool
	}{
		{name: "branches, tags, a symbolic ref and a pull request ref", source: seeded, head: true},
		{
			// The mirror's HEAD is then detached too, and read as the object, as ls-remote reads it.
			name: "a HEAD detached at a commit only a pull request ref has",
			source: func(t *testing.T) string {
				src := seeded(t)
				git(t, src, "update-ref", "--no-deref", "HEAD", "refs/pull/1/head")
				return src
			},
			head: true,
		},
		{
			// ls-remote leaves out a HEAD that resolves to nothing, and so does the read of the mirror,
			// rather than failing.
			name: "a HEAD naming a branch that does not exist",
			source: func(t *testing.T) string {
				src := seeded(t)
				git(t, src, "symbolic-ref", "HEAD", "refs/heads/gone")
				return src
			},
		},
		{
			name: "a repository with no commits",
			source: func(t *testing.T) string {
				src := t.TempDir()
				git(t, src, "init", "--bare", "--quiet", src)
				return src
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.source(t)
			want, err := g.LsRemote(ctx, src, Options{})
			if err != nil {
				t.Fatalf("ls-remote: %v", err)
			}
			mirror := filepath.Join(t.TempDir(), "m.git")
			if err := g.CloneMirror(ctx, src, mirror, Options{}); err != nil {
				t.Fatalf("clone: %v", err)
			}

			got, err := g.MirrorRefs(ctx, mirror)
			if err != nil {
				t.Fatalf("MirrorRefs: %v", err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("MirrorRefs = %v\nls-remote  = %v", got, want)
			}
			if _, ok := got["HEAD"]; ok != tc.head {
				t.Errorf("HEAD listed = %v, want %v: %v", ok, tc.head, got)
			}
			for name := range got {
				if strings.HasSuffix(name, "^{}") {
					t.Errorf("a peeled entry %s, which ls-remote's reading drops", name)
				}
			}
		})
	}

	t.Run("errors on a directory that is not a repository", func(t *testing.T) {
		// Never read as a repository with no refs, which would pass a clone that made nothing.
		if _, err := g.MirrorRefs(ctx, t.TempDir()); err == nil {
			t.Error("MirrorRefs accepted a directory that is not a repository")
		}
	})
}
