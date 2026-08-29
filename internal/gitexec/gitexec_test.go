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

func TestBundleVerify(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)

	// A real bundle of a real repository.
	work := t.TempDir()
	git(t, work, "init", "--quiet", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "a.txt")
	git(t, work, "commit", "--quiet", "-m", "first")

	bundle := filepath.Join(t.TempDir(), "t.bundle")
	if err := g.BundleAll(ctx, work, bundle); err != nil {
		t.Fatalf("bundle: %v", err)
	}

	t.Run("verifies a good bundle from outside any repository", func(t *testing.T) {
		// The case that was broken: git refuses with "need a repository to verify a bundle"
		// unless it is run inside one, and restore runs nowhere near a repository — it is in
		// the business of creating one. Every restore failed on this, which for a backup tool
		// is the worst possible thing to have untested.
		if err := g.BundleVerify(ctx, bundle); err != nil {
			t.Fatalf("BundleVerify on a good bundle: %v", err)
		}
	})

	t.Run("verifies from a working directory that is not a repository", func(t *testing.T) {
		// Belt and braces: the caller's cwd must not matter either.
		dir := t.TempDir()
		old, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(old) })

		if err := g.BundleVerify(ctx, bundle); err != nil {
			t.Fatalf("BundleVerify from a non-repository cwd: %v", err)
		}
	})

	t.Run("refuses a structurally damaged bundle", func(t *testing.T) {
		// What `git bundle verify` actually checks is the header: the format signature, the
		// prerequisites and the refs. Damage there is caught.
		bad := filepath.Join(t.TempDir(), "bad.bundle")
		data, err := os.ReadFile(bundle)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 32 && i < len(data); i++ {
			data[i] ^= 0xff
		}
		if err := os.WriteFile(bad, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := g.BundleVerify(ctx, bad); err == nil {
			t.Error("accepted a bundle with a damaged header")
		}
	})

	t.Run("does not notice damage inside the packfile", func(t *testing.T) {
		// Written down because it is surprising and load-bearing. `git bundle verify` reads the
		// header and stops; flipping bytes in the middle of the pack passes. So this call is a
		// structural check, not an integrity check, and the name promises more than it delivers.
		//
		// Integrity is the SHA-256 the restore path compares against the sidecar before it gets
		// here, and that sidecar is covered by the manifest signature. If that check is ever
		// removed or made conditional, this test is the reason not to.
		bad := filepath.Join(t.TempDir(), "middle.bundle")
		data, err := os.ReadFile(bundle)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(data) / 2; i < len(data)/2+64 && i < len(data); i++ {
			data[i] ^= 0xff
		}
		if err := os.WriteFile(bad, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := g.BundleVerify(ctx, bad); err != nil {
			t.Skip("this git does check the pack body; the checksum still owns integrity")
		}
	})

	t.Run("refuses a file that is not a bundle", func(t *testing.T) {
		notBundle := filepath.Join(t.TempDir(), "nope.bundle")
		if err := os.WriteFile(notBundle, []byte("this is not a bundle"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := g.BundleVerify(ctx, notBundle); err == nil {
			t.Error("accepted a file that is not a bundle")
		}
	})

	t.Run("refuses a bundle that does not exist", func(t *testing.T) {
		if err := g.BundleVerify(ctx, filepath.Join(t.TempDir(), "missing.bundle")); err == nil {
			t.Error("accepted a missing bundle")
		}
	})
}

// bundleFixture builds a repository with a branch, a lightweight tag and an annotated tag,
// bundles it the way the backup pipeline does, and returns the bundle path plus the mirror
// it came from.
func bundleFixture(t *testing.T) (bundle, mirror string) {
	t.Helper()
	ctx := context.Background()
	g := New(nil)

	work := t.TempDir()
	git(t, work, "init", "--quiet", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "a.txt")
	git(t, work, "commit", "--quiet", "-m", "first")
	git(t, work, "tag", "v1")
	git(t, work, "tag", "-a", "annotated", "-m", "an annotated tag")
	git(t, work, "branch", "feature")

	mirror = filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, work, mirror, Options{}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	bundle = filepath.Join(t.TempDir(), "t.bundle")
	if err := g.BundleAll(ctx, mirror, bundle); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	return bundle, mirror
}

func TestBundleHeads(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)
	bundle, mirror := bundleFixture(t)

	t.Run("reads the declared refs from outside any repository", func(t *testing.T) {
		// The same standing this has in production: the container runs at /, nowhere near a
		// repository. `git bundle verify` refuses in that position and every restore gitdr
		// shipped before v0.1.11 died on it, so the sibling operation is proven here rather
		// than assumed.
		dir := t.TempDir()
		old, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(old) })

		refs, err := g.BundleHeads(ctx, bundle)
		if err != nil {
			t.Fatalf("BundleHeads from a non-repository cwd: %v", err)
		}
		if len(refs) == 0 {
			t.Fatal("no refs declared")
		}
	})

	t.Run("declares every branch and tag, plus HEAD", func(t *testing.T) {
		refs, err := g.BundleHeads(ctx, bundle)
		if err != nil {
			t.Fatalf("BundleHeads: %v", err)
		}
		got := map[string]string{}
		for _, r := range refs {
			got[r.Name] = r.OID
		}
		for _, want := range []string{"refs/heads/main", "refs/heads/feature", "refs/tags/v1", "refs/tags/annotated", "HEAD"} {
			if _, ok := got[want]; !ok {
				t.Errorf("bundle does not declare %s; got %v", want, got)
			}
		}
	})

	t.Run("agrees with the mirror it was made from", func(t *testing.T) {
		// The bundle's declared map is only a proof if it is the mirror's map. Compared
		// against for-each-ref on the source, which is the same read the comparison does on
		// the restored side.
		refs, err := g.BundleHeads(ctx, bundle)
		if err != nil {
			t.Fatalf("BundleHeads: %v", err)
		}
		want, err := g.ListRefs(ctx, mirror)
		if err != nil {
			t.Fatalf("ListRefs: %v", err)
		}
		for _, r := range refs {
			if r.Name == "HEAD" {
				continue
			}
			if want[r.Name] != r.OID {
				t.Errorf("%s: bundle declares %s, mirror has %s", r.Name, r.OID, want[r.Name])
			}
		}
	})

	t.Run("records an annotated tag as its tag object, not the commit", func(t *testing.T) {
		// Load-bearing for the comparison: if the bundle recorded the peeled commit while
		// for-each-ref reports the tag object, every annotated tag would read as a mismatch.
		// It does not, so the two sides compare with no peeling on either.
		refs, err := g.BundleHeads(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}
		var tagOID, commitOID string
		for _, r := range refs {
			switch r.Name {
			case "refs/tags/annotated":
				tagOID = r.OID
			case "refs/heads/main":
				commitOID = r.OID
			}
		}
		if tagOID == "" || commitOID == "" {
			t.Fatalf("missing refs: tag=%q commit=%q", tagOID, commitOID)
		}
		if tagOID == commitOID {
			t.Error("annotated tag recorded as the commit; the tag object was peeled away")
		}
	})

	t.Run("refuses a file that is not a bundle", func(t *testing.T) {
		notBundle := filepath.Join(t.TempDir(), "nope.bundle")
		if err := os.WriteFile(notBundle, []byte("this is not a bundle"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := g.BundleHeads(ctx, notBundle); err == nil {
			t.Error("accepted a file that is not a bundle")
		}
	})

	t.Run("refuses a bundle that does not exist", func(t *testing.T) {
		if _, err := g.BundleHeads(ctx, filepath.Join(t.TempDir(), "missing.bundle")); err == nil {
			t.Error("accepted a missing bundle")
		}
	})
}

func TestParseBundleHeads(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []BundleRef
		wantErr bool
	}{
		{
			name: "branch, tag and HEAD",
			in:   "aa11 refs/heads/main\nbb22 refs/tags/v1\naa11 HEAD\n",
			want: []BundleRef{
				{Name: "refs/heads/main", OID: "aa11"},
				{Name: "refs/tags/v1", OID: "bb22"},
				{Name: "HEAD", OID: "aa11"},
			},
		},
		{
			// Every ref a real bundle can carry, including the namespaces a clone drops.
			name: "namespaces a clone does not materialise",
			in:   "aa11 refs/notes/commits\nbb22 refs/merge-requests/1/head\ncc33 refs/keep-around/x\n",
			want: []BundleRef{
				{Name: "refs/notes/commits", OID: "aa11"},
				{Name: "refs/merge-requests/1/head", OID: "bb22"},
				{Name: "refs/keep-around/x", OID: "cc33"},
			},
		},
		{name: "empty output declares nothing", in: "", want: nil},
		{name: "blank lines are skipped", in: "\n\naa11 refs/heads/main\n\n", want: []BundleRef{{Name: "refs/heads/main", OID: "aa11"}}},
		{name: "uppercase hex is an object id", in: "AA11 refs/heads/main\n", want: []BundleRef{{Name: "refs/heads/main", OID: "AA11"}}},
		// Anything unreadable is an error, never a skipped line: a ref that is never
		// declared is a ref the comparison can never find missing.
		{name: "a line with no ref name", in: "aa11\n", wantErr: true},
		{name: "a line with no object id", in: " refs/heads/main\n", wantErr: true},
		{name: "a non-hex object id", in: "zzzz refs/heads/main\n", wantErr: true},
		{name: "prose instead of a header", in: "fatal: not a bundle\n", wantErr: true},
		// A ref name can hold neither whitespace nor a control character, so a line with a
		// third field is not the line it was taken for, and an escape sequence in a name is
		// a way to write over the output that says how many refs matched.
		{name: "a third field on the line", in: "aa11 refs/heads/main junk\n", wantErr: true},
		{name: "an escape sequence in the ref name", in: "aa11 refs/heads/\x1b[2Kmain\n", wantErr: true},
		{name: "a NUL in the ref name", in: "aa11 refs/heads/\x00main\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBundleHeads(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBundleHeads(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBundleHeads(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d refs %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ref %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestListRefsAndHeadOID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	g := New(nil)
	_, mirror := bundleFixture(t)

	refs, err := g.ListRefs(ctx, mirror)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	for _, want := range []string{"refs/heads/main", "refs/heads/feature", "refs/tags/v1", "refs/tags/annotated"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("ListRefs missing %s; got %v", want, refs)
		}
	}
	// HEAD is not a ref for-each-ref reports, which is why it is resolved separately.
	if _, ok := refs["HEAD"]; ok {
		t.Error("ListRefs reported HEAD; the comparison resolves it separately")
	}
	head, err := g.HeadOID(ctx, mirror)
	if err != nil {
		t.Fatalf("HeadOID: %v", err)
	}
	if head != refs["refs/heads/main"] {
		t.Errorf("HeadOID = %s, want refs/heads/main %s", head, refs["refs/heads/main"])
	}

	t.Run("errors on a directory that is not a repository", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := g.ListRefs(ctx, dir); err == nil {
			t.Error("ListRefs accepted a non-repository")
		}
		if _, err := g.HeadOID(ctx, dir); err == nil {
			t.Error("HeadOID accepted a non-repository")
		}
	})

	t.Run("errors on an unborn HEAD", func(t *testing.T) {
		empty := t.TempDir()
		git(t, empty, "init", "--bare", "--quiet", empty)
		if _, err := g.HeadOID(ctx, empty); err == nil {
			t.Error("HeadOID resolved an unborn HEAD")
		}
	})
}
