package pipeline_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
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

// Pins the `restore --output json` shape. Verification is deliberately absent: the
// shape is a versioned public contract (SPEC §11), and the manifest-verification work
// added no field to it. If this test fails the contract changed; that takes a version
// bump and a note in SPEC.md, made deliberately.
func TestRestoreResultJSONShape(t *testing.T) {
	res := pipeline.RestoreResult{
		BundleKey: "k", SHA256: "s", OutDir: "o", Verified: true,
		Verification: "words about what was checked",
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	checkKeys(t, "restore", decode(t, b), "bundleKey", "sha256", "outDir", "verified")
}

// restoreFixture is one finished backup in an in-memory destination, plus what a
// restore test needs to check it or rewrite it.
type restoreFixture struct {
	md      *memDest
	res     *pipeline.BackupResult
	pub     ed25519.PublicKey
	privPEM []byte
	name    string
}

func backupForRestore(t *testing.T, lfs bool) *restoreFixture {
	t.Helper()
	ctx := context.Background()
	name, repoDir := "hello", ""
	if lfs {
		name = "lfsrepo"
		repoDir, _ = initLFSFixture(t)
	} else {
		repoDir = initFixtureRepo(t)
	}
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: name, CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Source.Repo = "octo/" + name
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return &restoreFixture{md: md, res: res, pub: pub, privPEM: privPEM, name: name}
}

// The mutations below reach into the memDest map directly: they play the attacker who
// can rewrite a bucket without WORM, which is exactly the exposure issue #50 is about.

// flipByte flips one byte in every stored object whose key has the suffix.
func flipByte(suffix string) func(*testing.T, *restoreFixture) {
	return func(t *testing.T, f *restoreFixture) {
		t.Helper()
		flipped := 0
		for k, v := range f.md.objs {
			if strings.HasSuffix(k, suffix) && len(v) > 0 {
				v[len(v)/2] ^= 1
				flipped++
			}
		}
		if flipped == 0 {
			t.Fatalf("nothing stored matches %q", suffix)
		}
	}
}

// rewriteBundleAndSidecar rewrites the bundle and its sha256 sidecar together, the one
// rewrite the unsigned sidecar can never catch. Only the signed manifest can.
func rewriteBundleAndSidecar(t *testing.T, f *restoreFixture) {
	t.Helper()
	for k, v := range f.md.objs {
		if strings.HasSuffix(k, ".bundle") {
			tampered := append(append([]byte{}, v...), 0x00)
			f.md.objs[k] = tampered
			shaKey := strings.TrimSuffix(k, ".bundle") + ".sha256"
			f.md.objs[shaKey] = []byte(fmt.Sprintf("%s  %s\n", crypto.SHA256Bytes(tampered), f.name+".bundle"))
			return
		}
	}
	t.Fatal("no bundle stored")
}

// dropKeys removes every stored object whose key contains the substring.
func dropKeys(substr string) func(*testing.T, *restoreFixture) {
	return func(t *testing.T, f *restoreFixture) {
		t.Helper()
		dropped := 0
		for k := range f.md.objs {
			if strings.Contains(k, substr) {
				delete(f.md.objs, k)
				dropped++
			}
		}
		if dropped == 0 {
			t.Fatalf("nothing stored matches %q", substr)
		}
	}
}

// plantLfsTar stores an LFS tar the manifest never recorded.
func plantLfsTar(t *testing.T, f *restoreFixture) {
	t.Helper()
	key := "github.com/octo/" + f.name + "/2026-06-13/" + f.name + ".lfs.tar"
	junk := "planted, not backed up"
	if _, err := f.md.PutImmutable(context.Background(), key, strings.NewReader(junk), int64(len(junk)), dest.Retention{}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreManifestVerification(t *testing.T) {
	// Not a git repository, for the same reason the other restore tests move: `git
	// bundle verify` behaves differently inside one.
	t.Chdir(t.TempDir())
	ctx := context.Background()

	tests := []struct {
		name    string
		lfs     bool
		key     bool
		mutate  func(*testing.T, *restoreFixture)
		wantErr string // substring of the error; empty means the restore must succeed
		wantSay string // substring of RestoreResult.Verification on success
	}{
		{
			name:    "clean restore verifies against the manifest",
			key:     true,
			wantSay: "bundle verified against the signed manifest",
		},
		{
			name:    "clean lfs restore verifies bundle and tar",
			lfs:     true,
			key:     true,
			wantSay: "bundle and lfs tar verified against the signed manifest",
		},
		{
			name:    "no key still restores and says what went unchecked",
			wantSay: "unsigned sha256 sidecar",
		},
		{
			name:    "no key with lfs says the tar was not checked",
			lfs:     true,
			wantSay: "lfs tar not checked",
		},
		{
			name:    "tampered lfs tar is refused",
			lfs:     true,
			key:     true,
			mutate:  flipByte(".lfs.tar"),
			wantErr: "does not match the signed manifest",
		},
		{
			name:    "bundle and sidecar rewritten together are refused",
			key:     true,
			mutate:  rewriteBundleAndSidecar,
			wantErr: "does not match the signed manifest",
		},
		{
			name:    "missing manifest fails instead of downgrading",
			key:     true,
			mutate:  dropKeys("/manifests/"),
			wantErr: "no signed manifest",
		},
		{
			name:    "manifest that does not verify fails",
			key:     true,
			mutate:  flipByte(".manifest.json"),
			wantErr: "did not verify",
		},
		{
			name:    "stored lfs tar the manifest does not record is refused",
			key:     true,
			mutate:  plantLfsTar,
			wantErr: "does not record",
		},
		{
			name:    "recorded lfs tar missing from the destination fails",
			lfs:     true,
			key:     true,
			mutate:  dropKeys(".lfs.tar"),
			wantErr: "does not have it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lfs && !gitexec.LFSAvailable() {
				if os.Getenv("CI") != "" {
					t.Fatal("git-lfs is not installed; in CI the LFS path must be exercised, not skipped")
				}
				t.Skip("git-lfs not installed")
			}
			f := backupForRestore(t, tc.lfs)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			deps := pipeline.RestoreDeps{Dest: f.md, Git: gitexec.New(nil)}
			if tc.key {
				deps.PublicKey = f.pub
			}
			res, err := pipeline.Restore(ctx, deps, pipeline.RestoreRequest{
				Host: "github.com", Owner: "octo", Name: f.name, Date: "2026-06-13",
				OutDir: filepath.Join(t.TempDir(), "restored"),
			})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("restore succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if !res.Verified {
				t.Fatal("restore not verified")
			}
			if !strings.Contains(res.Verification, tc.wantSay) {
				t.Fatalf("Verification = %q, want it to contain %q", res.Verification, tc.wantSay)
			}
		})
	}
}

// Several manifests on one date, and only the oldest records the bundle. A resume run
// writes a newer manifest that records the repo as skipped with no artifacts, and a run
// signed by a rotated key writes one restore cannot verify at all. The scan has to warn
// past the unverifiable one, pass over the artifact-less one, and verify the restore
// against the manifest of the run that actually stored the bundle.
func TestRestorePicksTheManifestThatRecordsTheBundle(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	f := backupForRestore(t, false)

	resumeRun := func(hour int, signer ed25519.PrivateKey) {
		t.Helper()
		src := &fixtureSource{repos: []source.Repo{{
			Host: "github.com", Owner: "octo", Name: f.name,
			CloneURL: "unused: resume skips before cloning", DefaultBranch: "main",
		}}}
		cfg := testConfig()
		cfg.Source.Repo = "octo/" + f.name
		cfg.Backup.Resume = true
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: cfg, Source: src, Dest: f.md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test",
			Now: func() time.Time { return time.Date(2026, 6, 13, hour, 0, 0, 0, time.UTC) },
		})
		if err != nil {
			t.Fatalf("resume backup: %v", err)
		}
		if res.Manifest.Repos[0].Status != pipeline.StatusSkipped {
			t.Fatalf("resume run status = %s, want skipped", res.Manifest.Repos[0].Status)
		}
	}

	// 13:00, same key: verifies, but records no artifacts.
	sameSigner, err := crypto.ParsePrivateKey(f.privPEM)
	if err != nil {
		t.Fatal(err)
	}
	resumeRun(13, sameSigner)
	// 14:00, rotated key: newest of the three, and does not verify with f.pub.
	_, rotatedPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := crypto.ParsePrivateKey(rotatedPEM)
	if err != nil {
		t.Fatal(err)
	}
	resumeRun(14, rotated)

	res, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: f.md, Git: gitexec.New(nil), PublicKey: f.pub}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: f.name, Date: "2026-06-13",
		OutDir: filepath.Join(t.TempDir(), "restored"),
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(res.Verification, "signed manifest") {
		t.Fatalf("Verification = %q, want the signed manifest used", res.Verification)
	}
}
