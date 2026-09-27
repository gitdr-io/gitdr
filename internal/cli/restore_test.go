package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -repo splits at the last slash.
//
// It split at the first, so acme/platform/api, the project api in the GitLab group acme/platform,
// became owner acme and name platform/api, and the restore looked for
// gitlab.com/acme/platform/api/<date>/platform/api.bundle, which no backup writes. A GitLab
// project in a subgroup could not be restored from the command line at all, with a key or without.
func TestRestoreSplitsRepoAtTheLastSlash(t *testing.T) {
	for _, tc := range []struct {
		repo, owner, name string
	}{
		{"acme/api", "acme", "api"},
		{"acme/platform/api", "acme/platform", "api"},
		{"a/b/c/proj", "a/b/c", "proj"},
		{"acme/my.repo-name", "acme", "my.repo-name"},
	} {
		owner, name, err := splitRepo(tc.repo)
		if err != nil || owner != tc.owner || name != tc.name {
			t.Errorf("splitRepo(%q) = %q, %q, %v; want %q, %q", tc.repo, owner, name, err, tc.owner, tc.name)
		}
	}

	const usage = "restore: -repo must be owner/name, or group/subgroup/name for a GitLab project in a subgroup"
	for _, repo := range []string{"", "api", "/api", "acme/", "/acme/api", "acme/api/", "acme//api", "acme/platform//api", "/"} {
		if owner, name, err := splitRepo(repo); err == nil || err.Error() != usage {
			t.Errorf("splitRepo(%q) = %q, %q, %v; want the usage error", repo, owner, name, err)
		}
	}
}

// A command line restore cannot run is exit 2 and says what is wrong, before any config is read.
func TestRestoreRefusesACommandLineItCannotRun(t *testing.T) {
	const key = "gitlab.com/acme/manifests/20260613T120000Z.manifest.json"
	out := filepath.Join(t.TempDir(), "restored")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "-manifest with -date",
			args: []string{"-manifest", key, "-repo", "acme/platform/api", "-out", out, "-date", "2026-06-13"},
			want: "the host and date come from it; drop -date",
		},
		{
			// Refused even when it says what the default says: set is set.
			name: "-manifest with -host",
			args: []string{"-manifest", key, "-repo", "acme/platform/api", "-out", out, "-host", "github.com"},
			want: "the host and date come from it; drop -host",
		},
		{
			name: "-manifest with both",
			args: []string{"-host", "gitlab.com", "-date", "2026-06-13", "-manifest", key, "-repo", "acme/api", "-out", out},
			want: "drop -date and -host",
		},
		{
			name: "-manifest without -repo",
			args: []string{"-manifest", key, "-out", out},
			want: "-repo must be owner/name",
		},
		{
			name: "-manifest without -out",
			args: []string{"-manifest", key, "-repo", "acme/api"},
			want: "-out is required",
		},
		{
			name: "neither -date nor -manifest",
			args: []string{"-repo", "acme/api", "-out", out},
			want: "give -date (YYYY-MM-DD), or -manifest",
		},
		{
			name: "a date in another form",
			args: []string{"-repo", "acme/api", "-out", out, "-date", "13/06/2026"},
			want: `-date "13/06/2026" is not a date in the form YYYY-MM-DD`,
		},
		{
			name: "a date that does not exist",
			args: []string{"-repo", "acme/api", "-out", out, "-date", "2026-02-30"},
			want: "is not a date in the form YYYY-MM-DD",
		},
		{
			name: "a nested owner with an empty segment",
			args: []string{"-repo", "acme//api", "-out", out, "-date", "2026-06-13"},
			want: "group/subgroup/name for a GitLab project in a subgroup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := restoreStderr(t, func() { code = runRestore(context.Background(), tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not say %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// restore -manifest reads a manifest only once its signature holds, so without a public key it
// refuses: exit 1, before a destination is built.
func TestRestoreFromAManifestNeedsThePublicKey(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("source:\n  type: gitlab\ndestination:\n  type: s3\n  s3:\n    bucket: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITDR_MANIFEST_PUBLICKEYPATH", "")
	t.Setenv("GITDR_DESTINATION_TYPE", "s3")
	t.Setenv("GITDR_DESTINATION_S3_BUCKET", "b")

	var code int
	stderr := restoreStderr(t, func() {
		code = runRestore(context.Background(), []string{
			"-config", cfg, "-log-format", "text",
			"-manifest", "gitlab.com/acme/manifests/20260613T120000Z.manifest.json",
			"-repo", "acme/platform/api", "-out", filepath.Join(dir, "restored"),
		})
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "restore -manifest needs the public key") {
		t.Errorf("stderr does not say the public key is missing:\n%s", stderr)
	}
}

// restoreStderr runs fn with os.Stderr pointed at a pipe and returns what was written to it.
// Read while fn runs, so a long write cannot fill the pipe and stall it.
func restoreStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return <-done
}
