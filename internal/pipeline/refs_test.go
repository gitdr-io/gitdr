package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/gitexec"
)

// Object ids, shortened. compareRefs never interprets them, it only compares them.
const (
	oidMain    = "aaaa1111"
	oidFeature = "bbbb2222"
	oidTag     = "cccc3333"
	oidWrong   = "dddd4444"
)

func decl(pairs ...string) []gitexec.BundleRef {
	refs := make([]gitexec.BundleRef, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		refs = append(refs, gitexec.BundleRef{Name: pairs[i], OID: pairs[i+1]})
	}
	return refs
}

// The normalisation matrix. Every rule in compareRefs has a row here, and every row was
// checked against what git actually produces before it was written down.
func TestCompareRefs(t *testing.T) {
	tests := []struct {
		name string
		// what the bundle header declares
		declared []gitexec.BundleRef
		// what for-each-ref reports in the restored repository
		restored map[string]string
		// what rev-parse HEAD resolves to there; empty means it could not be resolved
		head string

		wantDeclared      int
		wantMatched       int
		wantUnreferenced  []string
		wantFirstMismatch string // the Ref of Mismatches[0]; empty means OK()
		wantGot           string // Mismatches[0].Got
	}{
		{
			// The shape `git clone <bundle>` really produces: every branch under the
			// remote-tracking namespace, the checked-out one also under refs/heads, tags
			// under their own names, and an origin/HEAD the bundle never declared.
			name:     "clean restore matches every ref",
			declared: decl("refs/heads/main", oidMain, "refs/heads/feature", oidFeature, "refs/tags/v1", oidTag, "HEAD", oidMain),
			restored: map[string]string{
				"refs/heads/main":             oidMain,
				"refs/remotes/origin/main":    oidMain,
				"refs/remotes/origin/feature": oidFeature,
				"refs/remotes/origin/HEAD":    oidMain,
				"refs/tags/v1":                oidTag,
			},
			head:         oidMain,
			wantDeclared: 4, wantMatched: 4,
		},
		{
			name:     "a branch and a tag, nothing else",
			declared: decl("refs/heads/main", oidMain, "refs/tags/v1", oidTag, "HEAD", oidMain),
			restored: map[string]string{
				"refs/heads/main":          oidMain,
				"refs/remotes/origin/main": oidMain,
				"refs/tags/v1":             oidTag,
			},
			head:         oidMain,
			wantDeclared: 3, wantMatched: 3,
		},
		{
			// The smallest thing that can be backed up: one commit, one branch. A
			// repository with no commits at all never reaches here, because gitdr skips it
			// before bundling; that case is covered in TestCompareRestoredRefsRealGit.
			name:         "a single-branch repository",
			declared:     decl("refs/heads/main", oidMain, "HEAD", oidMain),
			restored:     map[string]string{"refs/heads/main": oidMain, "refs/remotes/origin/main": oidMain, "refs/remotes/origin/HEAD": oidMain},
			head:         oidMain,
			wantDeclared: 2, wantMatched: 2,
		},
		{
			// A mirror whose HEAD was detached clones with no refs/heads/* at all. Requiring
			// the branch under its own name would fail this perfectly good restore.
			name:         "a branch that landed only under refs/remotes/origin",
			declared:     decl("refs/heads/main", oidMain, "HEAD", oidFeature),
			restored:     map[string]string{"refs/remotes/origin/main": oidMain},
			head:         oidFeature,
			wantDeclared: 2, wantMatched: 2,
		},
		{
			name:         "object ids compare without regard to case",
			declared:     decl("refs/heads/main", strings.ToUpper(oidMain), "HEAD", oidMain),
			restored:     map[string]string{"refs/heads/main": oidMain},
			head:         strings.ToUpper(oidMain),
			wantDeclared: 2, wantMatched: 2,
		},
		{
			// Extra refs are git doing its job, not the backup losing anything. The proof
			// runs bundle-to-restore only.
			name:         "refs in the restore that the bundle never declared are fine",
			declared:     decl("refs/heads/main", oidMain),
			restored:     map[string]string{"refs/heads/main": oidMain, "refs/remotes/origin/HEAD": oidMain, "refs/heads/scratch": oidWrong},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 1,
		},
		{
			name:         "a declared branch missing from the restore fails",
			declared:     decl("refs/heads/main", oidMain, "refs/heads/feature", oidFeature),
			restored:     map[string]string{"refs/heads/main": oidMain},
			head:         oidMain,
			wantDeclared: 2, wantMatched: 1,
			wantFirstMismatch: "refs/heads/feature",
		},
		{
			name:         "a declared branch at the wrong commit fails and reports both",
			declared:     decl("refs/heads/main", oidMain),
			restored:     map[string]string{"refs/heads/main": oidWrong},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/heads/main", wantGot: oidWrong,
		},
		{
			// The remote-tracking name is the same history under another name, so a wrong
			// object there is just as much a failure.
			name:         "a branch wrong under the remote-tracking name fails",
			declared:     decl("refs/heads/main", oidMain),
			restored:     map[string]string{"refs/remotes/origin/main": oidWrong},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/heads/main", wantGot: oidWrong,
		},
		{
			name:         "a declared tag missing from the restore fails",
			declared:     decl("refs/tags/v1", oidTag),
			restored:     map[string]string{},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/tags/v1",
		},
		{
			name:         "a declared tag moved to another object fails",
			declared:     decl("refs/tags/v1", oidTag),
			restored:     map[string]string{"refs/tags/v1": oidWrong},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/tags/v1", wantGot: oidWrong,
		},
		{
			// A tag must not be satisfied by a branch of the same name, and vice versa.
			name:         "a tag is not satisfied by a branch of the same name",
			declared:     decl("refs/tags/v1", oidTag),
			restored:     map[string]string{"refs/heads/v1": oidTag, "refs/remotes/origin/v1": oidTag},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/tags/v1",
		},
		{
			name:         "HEAD at the wrong commit fails",
			declared:     decl("HEAD", oidMain),
			restored:     map[string]string{},
			head:         oidWrong,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "HEAD", wantGot: oidWrong,
		},
		{
			name:         "HEAD that cannot be resolved fails",
			declared:     decl("HEAD", oidMain),
			restored:     map[string]string{},
			head:         "",
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "HEAD",
		},
		{
			// HEAD is never satisfied by a ref literally named HEAD in the restore; it is
			// compared against what the repository actually has checked out.
			name:         "HEAD is not satisfied by origin/HEAD",
			declared:     decl("HEAD", oidMain),
			restored:     map[string]string{"refs/remotes/origin/HEAD": oidMain},
			head:         oidWrong,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "HEAD", wantGot: oidWrong,
		},
		{
			// git clone's refspec creates none of these. Held out of Matched and named, but
			// not a failure: it is the same for a perfect bundle and a damaged one.
			name:         "namespaces a clone does not create are reported, not failed",
			declared:     decl("refs/heads/main", oidMain, "refs/notes/commits", oidTag, "refs/merge-requests/1/head", oidFeature),
			restored:     map[string]string{"refs/heads/main": oidMain},
			head:         oidMain,
			wantDeclared: 3, wantMatched: 1,
			wantUnreferenced: []string{"refs/merge-requests/1/head", "refs/notes/commits"},
		},
		{
			// The upgrade path: the day a restore does materialise these, a wrong object
			// there becomes a failure with no change to this code.
			name:         "a ref outside heads and tags that is present must still match",
			declared:     decl("refs/notes/commits", oidTag),
			restored:     map[string]string{"refs/notes/commits": oidWrong},
			head:         oidMain,
			wantDeclared: 1, wantMatched: 0,
			wantFirstMismatch: "refs/notes/commits", wantGot: oidWrong,
		},
		{
			// Mismatches are sorted by ref name so the one named in the error is the same
			// on every run. Declared here in an order that would otherwise pick zzz first.
			name:         "the first mismatch reported is stable, not header order",
			declared:     decl("refs/heads/zzz", oidMain, "refs/heads/aaa", oidMain),
			restored:     map[string]string{},
			head:         oidMain,
			wantDeclared: 2, wantMatched: 0,
			wantFirstMismatch: "refs/heads/aaa",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compareRefs(tc.declared, tc.restored, tc.head)

			if got.Declared != tc.wantDeclared {
				t.Errorf("Declared = %d, want %d", got.Declared, tc.wantDeclared)
			}
			if got.Matched != tc.wantMatched {
				t.Errorf("Matched = %d, want %d", got.Matched, tc.wantMatched)
			}
			if strings.Join(got.Unreferenced, ",") != strings.Join(tc.wantUnreferenced, ",") {
				t.Errorf("Unreferenced = %v, want %v", got.Unreferenced, tc.wantUnreferenced)
			}
			if tc.wantFirstMismatch == "" {
				if !got.OK() {
					t.Fatalf("OK() = false, want true; mismatches = %v", got.Mismatches)
				}
				return
			}
			if got.OK() {
				t.Fatalf("OK() = true, want a mismatch on %s", tc.wantFirstMismatch)
			}
			if got.Mismatches[0].Ref != tc.wantFirstMismatch {
				t.Errorf("first mismatch = %s, want %s", got.Mismatches[0].Ref, tc.wantFirstMismatch)
			}
			if got.Mismatches[0].Got != tc.wantGot {
				t.Errorf("first mismatch Got = %q, want %q", got.Mismatches[0].Got, tc.wantGot)
			}
			// Matched and the two failure buckets must account for every declared ref.
			// Otherwise a ref could go missing from the arithmetic itself.
			if n := got.Matched + len(got.Unreferenced) + len(got.Mismatches); n != got.Declared {
				t.Errorf("matched(%d) + unreferenced(%d) + mismatches(%d) = %d, want Declared %d",
					got.Matched, len(got.Unreferenced), len(got.Mismatches), n, got.Declared)
			}
		})
	}
}

// The summary is what an operator reads and what an audit file records, so it must never
// describe an unverified restore in the words of a verified one.
func TestRefComparisonSummary(t *testing.T) {
	full := RefComparison{Declared: 4, Matched: 4}
	partial := RefComparison{Declared: 4, Matched: 3, Unreferenced: []string{"refs/notes/commits"}}

	tests := []struct {
		name        string
		c           RefComparison
		signed      bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "signed and complete", c: full, signed: true,
			wantContain: []string{"all 4 refs", "the signed bundle", "same commits"},
			wantAbsent:  []string{"not verified against a signed manifest"},
		},
		{
			name: "unsigned and complete says the bundle was not verified", c: full, signed: false,
			wantContain: []string{"all 4 refs", "the bundle declares", "not verified against a signed manifest"},
			wantAbsent:  []string{"the signed bundle"},
		},
		{
			name: "unreferenced refs are counted out loud", c: partial, signed: true,
			wantContain: []string{"3 of 4 refs", "refs/notes/commits", "git clone creates no ref"},
			wantAbsent:  []string{"all 4 refs"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.Summary(tc.signed)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("summary %q does not contain %q", got, want)
				}
			}
			for _, no := range tc.wantAbsent {
				if strings.Contains(got, no) {
					t.Errorf("summary %q must not contain %q", got, no)
				}
			}
		})
	}
}

// gitRun runs git in dir to build a fixture, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The same path as a real restore, driven by real git: mirror, bundle, clone, compare. The
// table above pins the rules; this proves the rules are the ones git produces.
func TestCompareRestoredRefsRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		// The git-lfs skips beside this one already fail in CI; git itself did not, and it is
		// the more fundamental dependency. This test is what proves the ref comparison matches
		// what git actually produces, which is the basis of every claim a drill makes.
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed; in CI the real-git comparison must run, not skip")
		}
		t.Skip("git is not installed")
	}
	// Not inside a repository, the standing the container runs in.
	t.Chdir(t.TempDir())
	ctx := context.Background()
	g := gitexec.New(nil)

	// build makes a source repo, mirrors it, bundles it as the backup pipeline does and
	// clones the bundle as restore does. Returns the bundle and the restored working copy.
	build := func(t *testing.T, seed func(work string)) (bundle, restored string) {
		t.Helper()
		work := t.TempDir()
		gitRun(t, work, "init", "--quiet", "-b", "main", work)
		seed(work)
		mirror := filepath.Join(t.TempDir(), "m.git")
		if err := g.CloneMirror(ctx, work, mirror, gitexec.Options{}); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		bundle = filepath.Join(t.TempDir(), "b.bundle")
		if err := g.BundleAll(ctx, mirror, bundle); err != nil {
			t.Fatalf("bundle: %v", err)
		}
		restored = filepath.Join(t.TempDir(), "restored")
		if err := g.CloneFromBundle(ctx, bundle, restored); err != nil {
			t.Fatalf("clone from bundle: %v", err)
		}
		return bundle, restored
	}

	commit := func(t *testing.T, work, file, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, file), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", ".")
		gitRun(t, work, "commit", "--quiet", "-m", msg)
	}

	t.Run("a clean restore matches every ref", func(t *testing.T) {
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "branch", "feature")
			gitRun(t, work, "tag", "v1")
			gitRun(t, work, "tag", "-a", "annotated", "-m", "annotated")
		})
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if !c.OK() {
			t.Fatalf("clean restore reported mismatches: %v", c.Mismatches)
		}
		// main, feature, v1, annotated, HEAD.
		if c.Declared != 5 || c.Matched != 5 {
			t.Fatalf("matched %d of %d, want 5 of 5", c.Matched, c.Declared)
		}
	})

	t.Run("a bundle with one branch and one tag", func(t *testing.T) {
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "tag", "v1.0.0")
		})
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if !c.OK() || c.Matched != c.Declared || c.Declared != 3 {
			t.Fatalf("matched %d of %d (want 3 of 3), mismatches %v", c.Matched, c.Declared, c.Mismatches)
		}
	})

	t.Run("an empty repository never produces a bundle to compare", func(t *testing.T) {
		// A project created and never pushed to. git refuses to bundle it, which is why
		// gitdr skips such a repository at backup time and there is no restore to check.
		// Asserted rather than assumed: if git ever started writing empty bundles, the
		// comparison would be handed one that declares nothing.
		work := t.TempDir()
		gitRun(t, work, "init", "--bare", "--quiet", work)
		err := g.BundleAll(ctx, work, filepath.Join(t.TempDir(), "empty.bundle"))
		if err == nil {
			t.Fatal("git wrote a bundle for a repository with no refs")
		}
	})

	t.Run("a bundle declaring no refs is refused, not scored 0 of 0", func(t *testing.T) {
		// Hand-built, because git will not write one: a valid v2 header with no ref lines,
		// followed by a real pack. `git bundle list-heads` reads it and exits 0 with no
		// output, so without the guard this would pass as a vacuous success.
		good, restored := build(t, func(work string) { commit(t, work, "a.txt", "one") })
		data, err := os.ReadFile(good)
		if err != nil {
			t.Fatal(err)
		}
		i := strings.Index(string(data), "\n\n")
		if i < 0 {
			t.Fatal("no header terminator in the bundle")
		}
		norefs := filepath.Join(t.TempDir(), "norefs.bundle")
		if err := os.WriteFile(norefs, append([]byte("# v2 git bundle\n\n"), data[i+2:]...), 0o600); err != nil {
			t.Fatal(err)
		}
		declared, err := g.BundleHeads(ctx, norefs)
		if err != nil {
			t.Fatalf("BundleHeads on a zero-ref header: %v", err)
		}
		if len(declared) != 0 {
			t.Fatalf("fixture declares %d refs, want 0", len(declared))
		}
		if _, err := CompareRestoredRefs(ctx, g, norefs, restored); err == nil {
			t.Fatal("a bundle declaring no refs passed the comparison")
		}
	})

	t.Run("a deleted branch in the restore is caught", func(t *testing.T) {
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "branch", "feature")
		})
		// Remove the branch under both names a clone could have put it under.
		gitRun(t, restored, "update-ref", "-d", "refs/remotes/origin/feature")
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if c.OK() {
			t.Fatal("a restore missing a branch passed")
		}
		if c.Mismatches[0].Ref != "refs/heads/feature" {
			t.Errorf("first mismatch = %s, want refs/heads/feature", c.Mismatches[0].Ref)
		}
	})

	t.Run("a tag moved to another commit is caught", func(t *testing.T) {
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "tag", "v1")
			commit(t, work, "b.txt", "two")
		})
		gitRun(t, restored, "update-ref", "refs/tags/v1", "HEAD")
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if c.OK() {
			t.Fatal("a restore with a moved tag passed")
		}
		if c.Mismatches[0].Ref != "refs/tags/v1" || c.Mismatches[0].Got == "" {
			t.Errorf("first mismatch = %+v, want refs/tags/v1 with the object it actually has", c.Mismatches[0])
		}
	})

	t.Run("a rewritten history under the same branch name is caught", func(t *testing.T) {
		// The case the whole feature exists for: the refs are all there, with the right
		// names, and the commit under one of them is not the one that was backed up.
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
		})
		commit(t, restored, "evil.txt", "an extra commit nobody backed up")
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if c.OK() {
			t.Fatal("a restore whose branch moved passed")
		}
	})

	t.Run("an operator's clone.defaultRemoteName does not break the comparison", func(t *testing.T) {
		// git puts every branch under refs/remotes/<remote>/*, and clone.defaultRemoteName
		// renames that namespace. Left to the machine's own gitconfig, a restore would
		// report every branch but the checked-out one as missing -- a check failing on a
		// perfect restore, which is worse than no check. CloneFromBundle pins --origin so
		// this cannot happen; the config below is set exactly as an operator's would be.
		cfg := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(cfg, []byte("[clone]\n\tdefaultRemoteName = upstream\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_CONFIG_GLOBAL", cfg)

		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "branch", "feature")
		})
		// Read through git, not the filesystem: a fresh clone packs its refs.
		got, err := g.ListRefs(ctx, restored)
		if err != nil {
			t.Fatal(err)
		}
		for name := range got {
			if strings.HasPrefix(name, "refs/remotes/upstream/") {
				t.Fatalf("the clone honoured clone.defaultRemoteName (%s); --origin is not pinned", name)
			}
		}
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if !c.OK() {
			t.Fatalf("a machine-local clone.defaultRemoteName failed a clean restore: %v", c.Mismatches)
		}
	})

	t.Run("notes and merge-request refs are reported, not failed", func(t *testing.T) {
		bundle, restored := build(t, func(work string) {
			commit(t, work, "a.txt", "one")
			gitRun(t, work, "notes", "add", "-m", "a note")
			gitRun(t, work, "update-ref", "refs/merge-requests/1/head", "HEAD")
			gitRun(t, work, "update-ref", "refs/keep-around/x", "HEAD")
		})
		c, err := CompareRestoredRefs(ctx, g, bundle, restored)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if !c.OK() {
			t.Fatalf("a GitLab-shaped repository failed a clean restore: %v", c.Mismatches)
		}
		if len(c.Unreferenced) != 3 {
			t.Fatalf("Unreferenced = %v, want the three refs a clone does not create", c.Unreferenced)
		}
		if c.Matched == c.Declared {
			t.Fatal("unreferenced refs were counted as matched; the score must show the gap")
		}
		if !strings.Contains(c.Summary(true), "refs/notes/commits") {
			t.Errorf("summary does not name the unreferenced refs: %s", c.Summary(true))
		}
	})
}

// Comparing against nothing is a check that cannot fail, and this is where it would happen.
func TestCompareSourceRefsRefusesAnEmptySet(t *testing.T) {
	_, err := CompareSourceRefs(context.Background(), gitexec.New(nil), nil, t.TempDir())
	if err == nil {
		t.Fatal("comparing against no source refs returned a passing comparison")
	}
	if !strings.Contains(err.Error(), "nothing to compare") {
		t.Errorf("error = %q, want it to say there was nothing to compare", err)
	}

	if _, err := CompareSourceRefs(context.Background(), gitexec.New(nil), map[string]string{}, t.TempDir()); err == nil {
		t.Error("an empty map was treated as a comparison that passed")
	}
}
