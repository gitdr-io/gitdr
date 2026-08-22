package pipeline_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/crypto"
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
