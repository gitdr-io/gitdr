package pipeline_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// memDest is an in-memory, create-only Destination for tests. It emulates WORM by
// refusing overwrites; locked is what VerifyWorm reports.
type memDest struct {
	mu     sync.Mutex
	objs   map[string][]byte
	locked bool
}

func newMemDest(locked bool) *memDest { return &memDest{objs: map[string][]byte{}, locked: locked} }

func (m *memDest) VerifyWorm(context.Context) (dest.WormStatus, error) {
	return dest.WormStatus{Enabled: m.locked, Locked: m.locked, Mode: "COMPLIANCE", Details: "in-memory"}, nil
}

func (m *memDest) PutImmutable(_ context.Context, key string, r io.Reader, _ int64, ret dest.Retention) (dest.PutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objs[key]; exists {
		return dest.PutResult{}, fmt.Errorf("create-only: %s already exists", key)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return dest.PutResult{}, err
	}
	m.objs[key] = b
	return dest.PutResult{Key: key, Size: int64(len(b)), RetainUntil: ret.Until}, nil
}

func (m *memDest) List(_ context.Context, prefix string) ([]dest.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []dest.Object
	for k, v := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, dest.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *memDest) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// fixtureSource is a local Source backed by paths on disk (no auth).
type fixtureSource struct{ repos []source.Repo }

func (f *fixtureSource) ListRepos(context.Context, source.Filter) ([]source.Repo, error) {
	return f.repos, nil
}
func (f *fixtureSource) CloneURL(_ context.Context, r source.Repo) (string, error) {
	return r.CloneURL, nil
}
func (f *fixtureSource) FetchMetadata(_ context.Context, r source.Repo) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"schema":"gitdr.meta/v0","name":%q}`, r.Name)), nil
}

func initFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello gitdr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return dir
}

func testConfig() *config.Config {
	c := config.Default()
	c.Destination.S3.Bucket = "test-bucket"
	c.Source.Repo = "octo/hello"
	return c
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }
}

func TestBackupVerifyRestore(t *testing.T) {
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
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

	// backup
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if res.Manifest.Status != pipeline.StatusSuccess {
		t.Fatalf("status = %s, want success", res.Manifest.Status)
	}
	if n := len(res.Manifest.Repos); n != 1 {
		t.Fatalf("repos = %d, want 1", n)
	}
	if n := len(res.Manifest.Repos[0].Artifacts); n != 3 {
		t.Fatalf("artifacts = %d, want 3 (bundle, meta, sha256)", n)
	}

	// verify
	vres, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vres.SignatureValid {
		t.Fatal("signature invalid")
	}
	if vres.ArtifactsOK != vres.ArtifactsChecked || vres.ArtifactsChecked != 3 {
		t.Fatalf("artifacts ok %d/%d, want 3/3", vres.ArtifactsOK, vres.ArtifactsChecked)
	}

	// restore
	outDir := filepath.Join(t.TempDir(), "restored")
	rres, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil)}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: outDir,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !rres.Verified {
		t.Fatal("restore not verified")
	}
	if _, err := os.Stat(filepath.Join(outDir, "README.md")); err != nil {
		t.Fatalf("restored repo missing README.md: %v", err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with a stored artifact (the in-memory map lets us simulate corruption;
	// real WORM storage would reject this).
	for k := range md.objs {
		if strings.HasSuffix(k, ".bundle") {
			md.objs[k] = append(md.objs[k], 0x00)
		}
	}
	if _, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey); err == nil {
		t.Fatal("verify should fail on tampered artifact")
	}
}

func TestWormCheck(t *testing.T) {
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir}}}
	_, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)

	deps := func(md *memDest, require bool) pipeline.BackupDeps {
		return pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test", Now: fixedClock(), RequireWORM: require,
		}
	}

	// not immutable, default (recommended, not required) -> warns and proceeds
	notLocked := newMemDest(false)
	res, err := pipeline.Backup(ctx, deps(notLocked, false))
	if err != nil {
		t.Fatalf("non-WORM destination should proceed by default: %v", err)
	}
	if len(notLocked.objs) == 0 {
		t.Fatal("expected objects written to the non-WORM destination")
	}
	if res.Manifest.Destination.WormImmutable {
		t.Error("manifest must record wormImmutable=false for a non-WORM destination")
	}
	// Adoption path: a non-immutable destination must be written WITHOUT retention, so
	// stores that reject lock headers on a plain bucket (e.g. S3 Object Lock 400) still
	// accept the write. Retention only belongs on a confirmed-immutable destination.
	for _, e := range res.Manifest.Repos {
		for _, a := range e.Artifacts {
			if !a.RetainUntil.IsZero() {
				t.Errorf("non-WORM write must carry no retention, got RetainUntil=%s for %s", a.RetainUntil, a.Key)
			}
		}
	}

	// not immutable, --require-worm -> abort before any write
	required := newMemDest(false)
	if _, err := pipeline.Backup(ctx, deps(required, true)); err == nil {
		t.Fatal("expected abort on non-WORM destination when worm.require is set")
	}
	if len(required.objs) != 0 {
		t.Fatalf("wrote %d objects before aborting", len(required.objs))
	}

	// immutable -> proceeds and records it in the manifest
	locked := newMemDest(true)
	res, err = pipeline.Backup(ctx, deps(locked, false))
	if err != nil {
		t.Fatalf("WORM destination should proceed: %v", err)
	}
	if !res.Manifest.Destination.WormImmutable {
		t.Error("manifest must record wormImmutable=true for a WORM destination")
	}
	// An immutable destination must carry the configured retention on every artifact.
	retained := 0
	for _, e := range res.Manifest.Repos {
		for _, a := range e.Artifacts {
			if a.RetainUntil.IsZero() {
				t.Errorf("WORM write must carry retention, got zero for %s", a.Key)
			} else {
				retained++
			}
		}
	}
	if retained == 0 {
		t.Fatal("expected at least one retained artifact on the WORM destination")
	}
}

func TestFanOutAndResume(t *testing.T) {
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	mk := func(name string) source.Repo {
		return source.Repo{Host: "github.com", Owner: "octo", Name: name, CloneURL: repoDir, DefaultBranch: "main"}
	}
	src := &fixtureSource{repos: []source.Repo{mk("a"), mk("b"), mk("c")}}
	md := newMemDest(true)
	_, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)

	cfg := testConfig()
	cfg.Source.Repo = "" // back up all three
	cfg.Backup.Concurrency = 3
	cfg.Backup.Resume = true

	deps := func(now func() time.Time) pipeline.BackupDeps {
		return pipeline.BackupDeps{
			Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test", Now: now,
		}
	}
	// same date for both runs (so resume sees the bundles), different time (so the
	// manifest key differs).
	clock1 := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }
	clock2 := func() time.Time { return time.Date(2026, 6, 13, 12, 1, 0, 0, time.UTC) }

	res, err := pipeline.Backup(ctx, deps(clock1))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(res.Manifest.Repos) != 3 {
		t.Fatalf("repos = %d, want 3", len(res.Manifest.Repos))
	}
	for _, e := range res.Manifest.Repos {
		if e.Status != pipeline.StatusSuccess {
			t.Fatalf("%s: %s", e.Slug, e.Status)
		}
	}

	res2, err := pipeline.Backup(ctx, deps(clock2))
	if err != nil {
		t.Fatalf("resume backup: %v", err)
	}
	for _, e := range res2.Manifest.Repos {
		if e.Status != pipeline.StatusSkipped {
			t.Fatalf("%s: status %s, want skipped", e.Slug, e.Status)
		}
	}
}

func TestEncryptedBackupRestore(t *testing.T) {
	ctx := context.Background()
	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)
	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, EncryptionKey: encKey, ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// stored bundle must be ciphertext (gitdr envelope magic), not a plain git bundle.
	for k, v := range md.objs {
		if strings.HasSuffix(k, ".bundle") && !bytes.HasPrefix(v, []byte("GDRE")) {
			t.Errorf("%s stored unencrypted (prefix %q)", k, v[:4])
		}
	}

	// verify is key-free: it checks the stored (ciphertext) checksums.
	if _, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: md, PublicKey: pub}, res.ManifestKey); err != nil {
		t.Fatalf("verify: %v", err)
	}

	out := filepath.Join(t.TempDir(), "restored")
	if _, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil), EncryptionKey: encKey}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: out,
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "README.md"))
	if err != nil {
		t.Fatalf("restored repo missing README.md: %v", err)
	}
	if string(got) != "hello gitdr\n" {
		t.Fatalf("restored content = %q", got)
	}

	// the wrong key must fail.
	wrong := make([]byte, 32)
	if _, err := rand.Read(wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil), EncryptionKey: wrong}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: filepath.Join(t.TempDir(), "wrong"),
	}); err == nil {
		t.Fatal("restore with the wrong key should fail")
	}

	// no key at all on encrypted data must fail early with a clear message, not a
	// confusing checksum mismatch against unreadable ciphertext.
	_, err = pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil)}, pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: filepath.Join(t.TempDir(), "nokey"),
	})
	if err == nil {
		t.Fatal("restore without a key should fail on encrypted data")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("want an 'encrypted' hint in the error, got: %v", err)
	}
}
