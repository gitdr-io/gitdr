package gitexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// git runs a git command in dir, failing the test on error. Used to build fixtures rather than
// to exercise the code under test.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestHasRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)

	t.Run("a repository with no commits has no refs", func(t *testing.T) {
		// The case that made every backup of an organisation fail: a project created and never
		// pushed to. `git bundle create` refuses to write an empty bundle, so this has to be
		// detected before bundling rather than caught afterwards as a failure.
		dir := t.TempDir()
		git(t, dir, "init", "--bare", "--quiet", dir)

		has, err := g.HasRefs(ctx, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if has {
			t.Error("HasRefs = true, want false for a repository with no commits")
		}
	})

	t.Run("a repository with one commit has refs", func(t *testing.T) {
		work := t.TempDir()
		git(t, work, "init", "--quiet", work)
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "a.txt")
		git(t, work, "commit", "--quiet", "-m", "first")

		has, err := g.HasRefs(ctx, work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !has {
			t.Error("HasRefs = false, want true for a repository with a commit")
		}
	})

	t.Run("a mirror clone of an empty repository has no refs", func(t *testing.T) {
		// Closest to what the pipeline actually sees: it asks this of a `clone --mirror`, not
		// of a freshly initialised directory.
		src := t.TempDir()
		git(t, src, "init", "--bare", "--quiet", src)

		mirror := filepath.Join(t.TempDir(), "m.git")
		if err := g.CloneMirror(ctx, src, mirror, Options{}); err != nil {
			t.Fatalf("clone: %v", err)
		}

		has, err := g.HasRefs(ctx, mirror)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if has {
			t.Error("HasRefs = true, want false for a mirror of an empty repository")
		}
	})

	t.Run("a tag alone counts as a ref", func(t *testing.T) {
		// for-each-ref covers refs/tags as well as refs/heads. A repository holding only a tag
		// has something to bundle, and must not be read as empty.
		work := t.TempDir()
		git(t, work, "init", "--quiet", work)
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "a.txt")
		git(t, work, "commit", "--quiet", "-m", "first")
		git(t, work, "tag", "v1")
		// Leave only the tag.
		git(t, work, "checkout", "--quiet", "v1")
		git(t, work, "branch", "-D", currentBranch(t, work))

		has, err := g.HasRefs(ctx, work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !has {
			t.Error("HasRefs = false, want true for a repository holding only a tag")
		}
	})

	t.Run("errors on a directory that is not a repository", func(t *testing.T) {
		// Never reported as "empty". A path that is not a repository is a fault worth failing
		// the run for, and answering false here would turn it into a silent skip.
		if _, err := g.HasRefs(ctx, t.TempDir()); err == nil {
			t.Error("expected an error for a non-repository directory")
		}
	})
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname:short)", "refs/heads")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	name := ""
	for _, line := range splitLines(string(out)) {
		if line != "" {
			name = line
			break
		}
	}
	if name == "" {
		t.Fatal("no branch found")
	}
	return name
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
