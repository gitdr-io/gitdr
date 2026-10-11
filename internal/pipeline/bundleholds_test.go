package pipeline

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/gitexec"
)

// A bundle that does not declare the refs the copy records, each at the same object and nothing
// else, is refused before anything is written.
//
// Nothing in a backup makes the two differ any more: a mirror whose HEAD names a branch that does
// not exist did, and the bundle is now written without HEAD there (gitexec.BundleAll). So the check
// is driven here with bundles of part of a mirror, and with records that are not the mirror's.
func TestABundleThatIsNotTheCopyIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed; in CI the check on the bundle must run, not skip")
		}
		t.Skip("git is not installed")
	}
	t.Chdir(t.TempDir())
	ctx := context.Background()
	g := gitexec.New(nil)

	work := t.TempDir()
	gitRun(t, work, "init", "--quiet", "-b", "main", work)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", name)
		gitRun(t, work, "commit", "--quiet", "-m", name)
		if name == "a.txt" {
			gitRun(t, work, "branch", "feature")
		}
	}
	gitRun(t, work, "tag", "-a", "v1", "-m", "v1")
	mirror := filepath.Join(t.TempDir(), "m.git")
	if err := g.CloneMirror(ctx, work, mirror, gitexec.Options{}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	copied, err := g.MirrorRefs(ctx, mirror)
	if err != nil {
		t.Fatalf("MirrorRefs: %v", err)
	}
	whole := filepath.Join(t.TempDir(), "whole.bundle")
	if err := g.BundleAll(ctx, mirror, whole); err != nil {
		t.Fatalf("BundleAll: %v", err)
	}
	mainOnly := filepath.Join(t.TempDir(), "main.bundle")
	gitRun(t, mirror, "bundle", "create", mainOnly, "refs/heads/main")

	// edited is the copy's record with one ref changed: set to oid, or dropped when oid is empty.
	edited := func(name, oid string) map[string]string {
		m := maps.Clone(copied)
		if oid == "" {
			delete(m, name)
		} else {
			m[name] = oid
		}
		return m
	}

	r := &backupRun{git: g}
	for _, tc := range []struct {
		name   string
		bundle string
		record map[string]string
		// Every string the refusal says; none when the bundle is the copy.
		want []string
	}{
		{name: "the bundle of the whole mirror", bundle: whole, record: copied},
		{
			name: "a bundle that lacks refs the clone holds", bundle: mainOnly, record: copied,
			want: []string{
				"the bundle written from the clone does not declare the refs the clone holds",
				"it lacks HEAD, refs/heads/feature, refs/tags/v1",
			},
		},
		{
			name: "a bundle that declares a ref at another object", bundle: whole,
			record: edited("refs/heads/feature", copied["refs/heads/main"]),
			want:   []string{"it declares refs/heads/feature at another object"},
		},
		{
			name: "a bundle that declares a ref the clone does not hold", bundle: whole,
			record: edited("refs/tags/v1", ""),
			want:   []string{"it declares refs/tags/v1, which the clone does not hold"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.bundleHolds(ctx, tc.bundle, tc.record)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("the bundle of the copy was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a bundle that is not the copy was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n  %v", want, err)
				}
			}
		})
	}
}
