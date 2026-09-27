package pipeline_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// Exercises the LFS path end to end.
//
// Requires git-lfs. It skips when the binary is absent, but not in CI: the container gitdr
// ships includes git-lfs, so this is a supported feature, and for its whole life this test
// skipped silently in CI because the Go image has no git-lfs. The package still reported
// ok, which is indistinguishable from the feature working.
func TestLFSBackupRestore(t *testing.T) {
	if !gitexec.LFSAvailable() {
		if os.Getenv("CI") != "" {
			t.Fatal("git-lfs is not installed; in CI the LFS path must be exercised, not skipped")
		}
		t.Skip("git-lfs not installed")
	}
	ctx := context.Background()
	repoDir, want := initLFSFixture(t)

	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "lfsrepo", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)

	cfg := testConfig()
	cfg.Source.Repo = "octo/lfsrepo"

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	kinds := map[string]bool{}
	for _, a := range res.Manifest.Repos[0].Artifacts {
		kinds[a.Kind] = true
	}
	if !kinds["lfs"] {
		t.Fatalf("expected an lfs artifact; got kinds=%v", kinds)
	}

	if _, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey); err != nil {
		t.Fatalf("verify: %v", err)
	}

	out := filepath.Join(t.TempDir(), "restored")
	rres, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil), PublicKey: pub}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "lfsrepo", Date: "2026-06-13", OutDir: out,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(rres.Verification, "bundle and lfs tar verified") {
		t.Fatalf("Verification = %q, want bundle and lfs tar verified", rres.Verification)
	}
	got, err := os.ReadFile(filepath.Join(out, "big.bin"))
	if err != nil {
		t.Fatalf("read restored LFS file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored LFS content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func requireLFS(t *testing.T) {
	t.Helper()
	if !gitexec.LFSAvailable() {
		if os.Getenv("CI") != "" {
			t.Fatal("git-lfs is not installed; in CI the LFS path must be exercised, not skipped")
		}
		t.Skip("git-lfs not installed")
	}
}

// Issue #59: an LFS repository backed up without its LFS content.
//
// backup.lfs off, or no git-lfs where the backup ran, and the backup records success with no LFS
// archive. The bundle holds the pointers, so the restore and the drill of it passed, with a
// 130-byte pointer where each LFS file should be: the pointer check only ran when there was an
// archive to extract. Now every restore reads the tree back, and says the content was never
// backed up. The backup itself is unchanged; recording the gap there needs gitdr.manifest/v6.
func TestAnLFSRepositoryBackedUpWithoutItsLFSContentDoesNotRestore(t *testing.T) {
	requireLFS(t)
	t.Chdir(t.TempDir())
	ctx := context.Background()
	repoDir, _ := initLFSFixture(t)
	md := newMemDest(true)
	pub, signer := drillKeys(t)

	cfg := testConfig()
	cfg.Source.Repo = "octo/lfsrepo"
	cfg.Backup.LFS = false // what a backup without git-lfs installed does too
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: []source.Repo{{
			Host: "github.com", Owner: "octo", Name: "lfsrepo", CloneURL: repoDir, DefaultBranch: "main",
		}}},
		Dest: md, Git: gitexec.New(nil), SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	for _, a := range res.Manifest.Repos[0].Artifacts {
		if a.Kind == "lfs" {
			t.Fatalf("the backup stored an LFS archive with backup.lfs off: %s", a.Key)
		}
	}

	want := []string{
		`1 file(s) came back as git-lfs pointers, first is "big.bin"`,
		"this backup holds no LFS archive (github.com/octo/lfsrepo/2026-06-13/lfsrepo.lfs.tar)",
		"the content of those files was not backed up",
	}
	for _, withKey := range []bool{true, false} {
		deps := pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil)}
		if withKey {
			deps.PublicKey = pub
		}
		out := filepath.Join(t.TempDir(), "restored")
		_, err := pipeline.Restore(ctx, deps, pipeline.RestoreRequest{
			Host: "github.com", Owner: "octo", Name: "lfsrepo", Date: "2026-06-13", OutDir: out,
		})
		if err == nil {
			t.Fatalf("public key %v: restored clean, with a pointer where big.bin should be", withKey)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("public key %v: error %q\ndoes not say %q", withKey, err, w)
			}
		}
		// The tree stays where it was put, pointers and all, for whoever wants to look.
		if b, err := os.ReadFile(filepath.Join(out, "big.bin")); err != nil || !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1")) {
			t.Errorf("public key %v: the restored tree is not left in place: %v", withKey, err)
		}
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return fixedClock()().Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if !errors.Is(err, pipeline.ErrDrillFailures) {
		t.Fatalf("drill: err = %v, want ErrDrillFailures", err)
	}
	r := drilled.Report.Repos[0]
	if r.Status != pipeline.StatusFailed {
		t.Errorf("drill status %s, want failed", r.Status)
	}
	for _, w := range want {
		if !strings.Contains(r.Error, w) {
			t.Errorf("drill error %q\ndoes not say %q", r.Error, w)
		}
	}
}

// The tar path reads the tree back with the same detector. An archive that lacks an object leaves
// a pointer after `git lfs checkout`, which exits 0 regardless.
//
// Without a public key nothing holds the archive to a checksum, so this is what such a restore
// extracts from a bucket where the archive was replaced.
func TestAnLFSRestoreThatLeavesAPointerFails(t *testing.T) {
	requireLFS(t)
	t.Chdir(t.TempDir())
	f := backupForRestore(t, true)

	var empty bytes.Buffer
	if err := tar.NewWriter(&empty).Close(); err != nil {
		t.Fatal(err)
	}
	const lfsKey = "github.com/octo/lfsrepo/2026-06-13/lfsrepo.lfs.tar"
	if _, ok := f.md.objs[lfsKey]; !ok {
		t.Fatalf("no archive stored at %s", lfsKey)
	}
	f.md.objs[lfsKey] = empty.Bytes()

	_, err := pipeline.Restore(context.Background(), pipeline.RestoreDeps{Dest: f.md, Git: gitexec.New(nil)}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "lfsrepo", Date: "2026-06-13", OutDir: filepath.Join(t.TempDir(), "restored"),
	})
	if want := `lfs restore incomplete: 1 file(s) are still pointers, first is "big.bin"`; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// lfsListFails is a destination whose listing of an LFS archive fails.
type lfsListFails struct{ *memDest }

func (d *lfsListFails) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	if strings.HasSuffix(prefix, ".lfs.tar") {
		return nil, errors.New("listing refused")
	}
	return d.memDest.List(ctx, prefix)
}

// The no-archive check reads the tree and nothing else, so it needs no git-lfs: this repository
// never had any, and holds a committed file that is a pointer. What the error claims depends on
// what was known. With a key, the signed manifest recording no archive is the word that there is
// none. Without one, a listing that failed has not said so.
func TestAPointerWithNoArchiveSaysWhatTheRestoreKnows(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	dir := initFixtureRepo(t)
	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "model.bin"}, {"commit", "-q", "-m", "a pointer"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	cfg := testConfig()
	cfg.Backup.LFS = false
	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: []source.Repo{{
			Host: "github.com", Owner: "octo", Name: "hello", CloneURL: dir, DefaultBranch: "main",
		}}},
		Dest: md, Git: gitexec.New(nil), SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	const lfsKey = "github.com/octo/hello/2026-06-13/hello.lfs.tar"
	for _, tc := range []struct {
		name string
		pub  ed25519.PublicKey
		want string
	}{
		{"with a key, the manifest says there is none", pub, "this backup holds no LFS archive (" + lfsKey + ")"},
		{"without one, the listing failed", nil, "looking for this backup's LFS archive (" + lfsKey + ") failed: listing refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: &lfsListFails{md}, Git: gitexec.New(nil), PublicKey: tc.pub},
				pipeline.RestoreRequest{Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: filepath.Join(t.TempDir(), "r")})
			if err == nil || !strings.Contains(err.Error(), `1 file(s) came back as git-lfs pointers, first is "model.bin"`) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v\nwant it to say %q", err, tc.want)
			}
		})
	}
}

func initLFSFixture(t *testing.T) (dir string, content []byte) {
	t.Helper()
	dir = t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
	)
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v: %s", name, args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "lfs", "install", "--local")
	run("git", "lfs", "track", "*.bin")
	content = bytes.Repeat([]byte("LFS-PAYLOAD-"), 4096) // ~48 KiB
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-q", "-m", "add lfs file")
	return dir, content
}
