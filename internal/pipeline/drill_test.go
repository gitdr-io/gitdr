package pipeline_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// A drill against a real backup, and then against a damaged one.
//
// The second half is the point. A drill that passes is worth nothing on its own — the question
// an auditor is really asking is whether it would have failed, and the only way to answer that
// is to break the artifact and watch it.
func TestADrillProvesTheBackupRestores(t *testing.T) {
	t.Chdir(t.TempDir())
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

	clock := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }
	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Captured, because the report's own key is logged and nothing else names it: an operator
	// had no supported way to fetch the document they had just produced.
	var logged bytes.Buffer

	report, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		ToolVersion: "test", Now: func() time.Time { return clock().Add(time.Hour) },
		Logger: slog.New(slog.NewTextHandler(&logged, nil)),
	}, pipeline.DrillRequest{Host: "github.com", Owner: "octo", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}

	if out := logged.String(); !strings.Contains(out, "drill report written") ||
		!strings.Contains(out, ".drill.json") || !strings.Contains(out, ".drill.json.sig") {
		t.Errorf("the report's key and its signature were not logged:\n%s", out)
	}

	if report.Status != pipeline.StatusSuccess {
		t.Fatalf("status = %s, want success: %+v", report.Status, report.Repos)
	}
	if report.Schema != pipeline.DrillSchema {
		t.Errorf("schema = %q", report.Schema)
	}
	if !report.ManifestSigned {
		t.Error("the manifest's signature was not checked, so this proves less than it looks like")
	}
	if report.Eligible != 1 || report.Drilled != 1 {
		t.Errorf("eligible=%d drilled=%d, want 1 and 1", report.Eligible, report.Drilled)
	}

	r := report.Repos[0]
	if r.BundleRefs == 0 {
		t.Error("the bundle declared no refs, so the comparison proved nothing")
	}
	if r.RestoredRefs != r.BundleRefs-len(r.Unreferenced) {
		t.Errorf("restored %d of %d declared, %d unreferenced", r.RestoredRefs, r.BundleRefs, len(r.Unreferenced))
	}
	// The half that only became possible when the manifest started recording source refs.
	if r.SourceRefs == 0 {
		t.Error("the manifest recorded no source refs, so the drill could not check the artifact against what the source had")
	}
	if r.SourceMatch == nil || !*r.SourceMatch {
		t.Errorf("sourceMatch = %v, want true", r.SourceMatch)
	}

	// The report is evidence, so it is stored and signed like everything else.
	var stored, sig bool
	for _, k := range keysOf(md) {
		if strings.Contains(k, "/drills/") && strings.HasSuffix(k, ".drill.json") {
			stored = true
		}
		if strings.HasSuffix(k, ".drill.json.sig") {
			sig = true
		}
	}
	if !stored {
		t.Error("the drill report was not written to the destination")
	}
	if !sig {
		t.Error("the drill report was not signed; unsigned evidence is a text file anybody can write")
	}
}

func TestADrillFailsOnADamagedBundle(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)

	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)
	clock := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Corrupt the stored bundle. The destination has no overwrite by construction, so this
	// reaches past the interface and into the map behind it — which is the only way to
	// simulate storage that was tampered with rather than storage gitdr wrote.
	md.mu.Lock()
	for k := range md.objs {
		if strings.HasSuffix(k, ".bundle") {
			md.objs[k] = []byte("this is not a git bundle")
		}
	}
	md.mu.Unlock()

	report, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		ToolVersion: "test", Now: func() time.Time { return clock().Add(time.Hour) },
	}, pipeline.DrillRequest{Host: "github.com", Owner: "octo", WorkDir: t.TempDir()})

	if err == nil {
		t.Fatal("a drill against a corrupted bundle reported no error")
	}
	if report == nil {
		t.Fatal("no report, so nobody can see what failed")
	}
	if report.Status != pipeline.StatusFailed {
		t.Errorf("status = %s, want failed", report.Status)
	}
	if len(report.Repos) != 1 || report.Repos[0].Status != pipeline.StatusFailed {
		t.Fatalf("repos = %+v, want one failure", report.Repos)
	}
	if report.Repos[0].Error == "" {
		t.Error("the failure has no reason on it")
	}
}

func TestADrillRefusesAManifestThatDoesNotMatchItsSignature(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)
	clock := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Rewrite the manifest so it no longer matches the signature beside it.
	md.mu.Lock()
	for k, v := range md.objs {
		if strings.HasSuffix(k, ".manifest.json") {
			md.objs[k] = append(v[:len(v)-1], []byte(` `)...)
		}
	}
	md.mu.Unlock()

	_, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		ToolVersion: "test", Now: clock,
	}, pipeline.DrillRequest{Host: "github.com", Owner: "octo", WorkDir: t.TempDir()})

	if err == nil {
		t.Fatal("drilled a manifest that does not match its signature")
	}
	// Refused rather than recorded: evidence about an artifact set nobody can attribute to
	// gitdr is worse than no evidence.
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %q, want it to name the signature", err)
	}
}

func keysOf(md *memDest) []string {
	md.mu.Lock()
	defer md.mu.Unlock()
	out := make([]string, 0, len(md.objs))
	for k := range md.objs {
		out = append(out, k)
	}
	return out
}

// A bundle that restores perfectly, from a manifest that honestly records a branch the bundle
// does not have.
//
// This is the failure the source comparison exists for and the one no other check in the
// product can see: a mirror that half-fetched, or a bundle written while a push was landing.
// Every artifact is intact, every checksum matches, the restore reproduces exactly what the
// bundle declares — and history is missing.
//
// Built by re-signing a modified manifest with the same key, because the drill refuses one
// whose signature does not match, and it should.
func TestADrillCatchesABundleMissingHistoryTheSourceHad(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)
	clock := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// The source advertised a branch the bundle does not carry.
	rewriteManifest(t, md, signer, func(m *pipeline.Manifest) {
		m.Repos[0].Refs = append(m.Repos[0].Refs, pipeline.RefEntry{
			Name:   "refs/heads/release",
			Commit: "0000000000000000000000000000000000000001",
		})
	})

	report, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		ToolVersion: "test", Now: func() time.Time { return clock().Add(time.Hour) },
	}, pipeline.DrillRequest{Host: "github.com", Owner: "octo", WorkDir: t.TempDir()})

	if err == nil {
		t.Fatal("a restore missing a branch the source had was reported as a success")
	}
	r := report.Repos[0]
	if r.Status != pipeline.StatusFailed {
		t.Errorf("status = %s, want failed", r.Status)
	}
	if r.SourceMatch == nil || *r.SourceMatch {
		t.Errorf("sourceMatch = %v, want false", r.SourceMatch)
	}
	// The bundle itself was fine, and the report must say so rather than blaming the artifact.
	if r.BundleRefs == 0 || r.RestoredRefs == 0 {
		t.Error("the bundle comparison was not run or found nothing; the report reads as a corrupt artifact")
	}
	if len(r.Mismatches) == 0 {
		t.Error("the report does not name what was missing")
	}
	var named bool
	for _, mm := range r.Mismatches {
		if strings.Contains(mm, "refs/heads/release") {
			named = true
		}
	}
	if !named {
		t.Errorf("mismatches do not name the missing branch: %v", r.Mismatches)
	}
}

// A pre-v3 copy: the manifest records no source refs. The drill still proves the bundle
// restores, says so, and does not claim the half it could not check.
func TestADrillDoesNotClaimASourceMatchItCouldNotMake(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)
	clock := func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	rewriteManifest(t, md, signer, func(m *pipeline.Manifest) { m.Repos[0].Refs = nil })

	report, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		ToolVersion: "test", Now: func() time.Time { return clock().Add(time.Hour) },
	}, pipeline.DrillRequest{Host: "github.com", Owner: "octo", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}

	r := report.Repos[0]
	if r.Status != pipeline.StatusSuccess {
		t.Errorf("status = %s: the bundle restores and that is still worth reporting", r.Status)
	}
	// Null, not false. "Not recorded" and "did not match" are different answers, and a reader
	// has to be able to tell them apart.
	if r.SourceMatch != nil {
		t.Errorf("sourceMatch = %v, want null: nothing was recorded to compare against", *r.SourceMatch)
	}
	if r.SourceRefs != 0 {
		t.Errorf("sourceRefs = %d, want 0", r.SourceRefs)
	}
}

// rewriteManifest edits the stored manifest and re-signs it, so the drill still accepts it.
func rewriteManifest(t *testing.T, md *memDest, signer ed25519.PrivateKey, edit func(*pipeline.Manifest)) {
	t.Helper()
	md.mu.Lock()
	defer md.mu.Unlock()

	var key string
	for k := range md.objs {
		if strings.HasSuffix(k, ".manifest.json") {
			key = k
		}
	}
	if key == "" {
		t.Fatal("no manifest to rewrite")
	}

	var m pipeline.Manifest
	if err := json.Unmarshal(md.objs[key], &m); err != nil {
		t.Fatal(err)
	}
	edit(&m)

	canon, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	md.objs[key] = canon
	md.objs[key+".sig"] = []byte(base64.StdEncoding.EncodeToString(crypto.Sign(signer, canon)))
}

// The artifact key is parsed from the end, because an owner can be more than one segment.
//
// GitLab groups nest. A real key from a real run reads
// `gitlab.com/pitici/gitdr/gitdr/2026-09-02/gitdr.bundle`, and counting from the front made the
// owner "pitici", the name "gitdr", and sent the restore looking under a path that does not
// exist. Every drill of a GitLab project in a subgroup failed, with an error about the date.
//
// The fixtures all use a single-segment owner, which is every GitHub repository and only some
// GitLab ones, so nothing here could have caught it. A real backup did.
func TestLocateHandlesANestedGroupPath(t *testing.T) {
	cases := []struct {
		name                    string
		key                     string
		host, owner, repo, date string
	}{
		{
			name: "github, one segment",
			key:  "github.com/octo/hello/2026-06-13/hello.bundle",
			host: "github.com", owner: "octo", repo: "hello", date: "2026-06-13",
		},
		{
			name: "gitlab subgroup",
			key:  "gitlab.com/pitici/gitdr/gitdr/2026-09-02/gitdr.bundle",
			host: "gitlab.com", owner: "pitici/gitdr", repo: "gitdr", date: "2026-09-02",
		},
		{
			name: "gitlab, three levels deep",
			key:  "gitlab.example.com/a/b/c/proj/2026-01-01/proj.bundle",
			host: "gitlab.example.com", owner: "a/b/c", repo: "proj", date: "2026-01-01",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, o, n, d, err := pipeline.LocateForTest(
				&pipeline.Manifest{},
				pipeline.RepoEntry{Artifacts: []pipeline.ArtifactInfo{{Key: c.key}}},
			)
			if err != nil {
				t.Fatalf("locate: %v", err)
			}
			if h != c.host || o != c.owner || n != c.repo || d != c.date {
				t.Errorf("got %s / %s / %s / %s, want %s / %s / %s / %s",
					h, o, n, d, c.host, c.owner, c.repo, c.date)
			}
		})
	}
}
