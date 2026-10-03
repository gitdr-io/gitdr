package pipeline_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// How restore, drill and the next backup find a run's manifest, end to end: issues #57, #58
// (the pipeline half), #60 and #61.

// slugRepos is one fixture repository per owner/name slug, split at the last slash, all cloned
// from dir.
func slugRepos(host, dir string, slugs ...string) []source.Repo {
	repos := make([]source.Repo, 0, len(slugs))
	for _, s := range slugs {
		i := strings.LastIndex(s, "/")
		repos = append(repos, source.Repo{Host: host, Owner: s[:i], Name: s[i+1:], CloneURL: dir, DefaultBranch: "main"})
	}
	return repos
}

// backupAt backs up repos into md on a clock stopped at at, the store's clock with it.
func backupAt(t *testing.T, md *memDest, signer ed25519.PrivateKey, at time.Time, repos []source.Repo) *pipeline.BackupResult {
	t.Helper()
	md.storeAt(at)
	cfg := testConfig()
	cfg.Source.Repo = ""
	res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("backup at %s: %v", at, err)
	}
	return res
}

func restoreDeps(md *memDest, pub ed25519.PublicKey) pipeline.RestoreDeps {
	return pipeline.RestoreDeps{Dest: md, Git: gitexec.New(nil), PublicKey: pub}
}

// plantCopy stores a byte copy of the manifest at from, and of its signature, under to: two
// create-only writes, which is all #60 takes.
func plantCopy(t *testing.T, md *memDest, from, to string) {
	t.Helper()
	md.mu.Lock()
	defer md.mu.Unlock()
	if md.objs[from] == nil || md.objs[from+".sig"] == nil {
		t.Fatalf("nothing stored at %s to copy", from)
	}
	md.objs[to] = bytes.Clone(md.objs[from])
	md.objs[to+".sig"] = bytes.Clone(md.objs[from+".sig"])
}

// resign rewrites the manifest at key and signs it again, as someone holding the signing key
// could.
func resign(t *testing.T, md *memDest, signer ed25519.PrivateKey, key string, edit func(*pipeline.Manifest)) {
	t.Helper()
	md.mu.Lock()
	defer md.mu.Unlock()
	var m pipeline.Manifest
	if err := json.Unmarshal(md.objs[key], &m); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	edit(&m)
	canon, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	md.objs[key] = canon
	md.objs[key+".sig"] = []byte(base64.StdEncoding.EncodeToString(crypto.Sign(signer, canon)))
}

// Issue #57, as reported: a GitLab run over a group, one of its subgroups and a user's namespace.
//
// The manifest went under whichever namespace the source listed first, and a restore looked only
// under the repository's own, so two of these three failed with "no signed manifest", and a drill
// failed them for the same reason. Now the manifest is filed at the host, since the three share no
// namespace, and every repository restores both ways: found from its namespace upward, and named
// with -manifest, the way the drill that passed it names it.
func TestEveryRepositoryOfARunOverSeveralNamespacesRestores(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	at := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	res := backupAt(t, md, signer, at, slugRepos("gitlab.com", initFixtureRepo(t), "acme/app", "acme/platform/api", "alice/tool"))
	if want := "gitlab.com/manifests/20260613T120000Z.manifest.json"; res.ManifestKey != want {
		t.Fatalf("manifest filed at %s, want %s", res.ManifestKey, want)
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return at.Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if r := drilled.Report; r.Status != pipeline.StatusSuccess || r.Drilled != 3 || !r.ManifestSigned {
		t.Fatalf("drill %s of %d, signed %v: %+v", r.Status, r.Drilled, r.ManifestSigned, r.Repos)
	}

	for _, r := range drilled.Report.Repos {
		t.Run(r.Slug, func(t *testing.T) {
			i := strings.LastIndex(r.Slug, "/")
			owner, name := r.Slug[:i], r.Slug[i+1:]
			wantBundle := "gitlab.com/" + r.Slug + "/2026-06-13/" + name + ".bundle"

			found, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
				Host: "gitlab.com", Owner: owner, Name: name, Date: "2026-06-13",
				OutDir: filepath.Join(t.TempDir(), "found"),
			})
			if err != nil {
				t.Fatalf("restore without -manifest: %v", err)
			}
			named, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
				ManifestKey: drilled.Report.ManifestKey, Owner: owner, Name: name,
				OutDir: filepath.Join(t.TempDir(), "named"),
			})
			if err != nil {
				t.Fatalf("restore -manifest %s: %v", drilled.Report.ManifestKey, err)
			}
			for how, got := range map[string]*pipeline.RestoreResult{"found": found, "named": named} {
				if got.BundleKey != wantBundle || !strings.Contains(got.Verification, "bundle verified against the signed manifest") {
					t.Errorf("%s: restored %s (%s), want %s verified against the signed manifest", how, got.BundleKey, got.Verification, wantBundle)
				}
			}
		})
	}
}

// A run inside one group files its manifest under the group, and a restore of a project in a
// subgroup finds it one namespace up. A drill of the group picks it as the newest.
func TestARunInsideOneGroupIsFoundFromItsSubgroups(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	at := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	res := backupAt(t, md, signer, at, slugRepos("gitlab.com", initFixtureRepo(t), "acme/platform/api", "acme/web"))
	if want := "gitlab.com/acme/manifests/20260613T120000Z.manifest.json"; res.ManifestKey != want {
		t.Fatalf("manifest filed at %s, want %s", res.ManifestKey, want)
	}

	// gitlab.com/acme/platform/manifests/ holds nothing; gitlab.com/acme/manifests/ does.
	if _, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
		Host: "gitlab.com", Owner: "acme/platform", Name: "api", Date: "2026-06-13",
		OutDir: filepath.Join(t.TempDir(), "api"),
	}); err != nil {
		t.Fatalf("restore of acme/platform/api: %v", err)
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return at.Add(time.Hour) },
	}, pipeline.DrillRequest{Host: "gitlab.com", Owner: "acme", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if r := drilled.Report; r.ManifestKey != res.ManifestKey || r.Status != pipeline.StatusSuccess || r.Drilled != 2 {
		t.Errorf("drilled %s: %s, %d repositories; want %s, success, 2", r.ManifestKey, r.Status, r.Drilled, res.ManifestKey)
	}
}

// The next run finds the previous manifest however the source lists the repositories.
//
// Filed under the first repository's owner, a new project that happened to be listed first moved
// the manifest, and the next run read an empty directory and copied everything.
func TestTheNextRunFindsThePreviousManifestWhateverTheListingOrder(t *testing.T) {
	t.Chdir(t.TempDir())
	md := newMemDest(true)
	_, signer := drillKeys(t)
	dir := initFixtureRepo(t)
	day1 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	first := backupAt(t, md, signer, day1, slugRepos("gitlab.com", dir, "acme/app", "alice/tool"))
	second := backupAt(t, md, signer, day1.AddDate(0, 0, 1), slugRepos("gitlab.com", dir, "alice/tool", "acme/app"))

	for _, key := range []string{first.ManifestKey, second.ManifestKey} {
		if path.Dir(key) != "gitlab.com/manifests" {
			t.Errorf("manifest filed at %s, want it under gitlab.com/manifests/: the two namespaces share nothing", key)
		}
	}
	for _, e := range second.Manifest.Repos {
		if e.Status != pipeline.StatusSkipped || !strings.HasPrefix(e.Reason, pipeline.ReasonUnchanged) {
			t.Errorf("%s: %s %q, want skipped as unchanged: the second run did not read the first's manifest", e.Slug, e.Status, e.Reason)
		}
	}
}

// stoppedClock is a clock a test moves by hand, safe to read from the backup's goroutines.
type stoppedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stoppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *stoppedClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// midnightSource moves the clock past midnight while the backup of a repository is under way:
// after its bundle is dated and before the run finishes.
type midnightSource struct {
	*fixtureSource
	clock *stoppedClock
	after time.Time
}

func (s *midnightSource) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	s.clock.set(s.after)
	return s.fixtureSource.FetchMetadata(ctx, r)
}

// Issue #57, the other half: a repository copied before midnight UTC in a run that finished after
// it. The copy is dated the 13th and the manifest is named for the 14th, and a restore by the date
// of the copy looked only at manifests of the 13th.
func TestARunThatCrossesMidnightIsFoundFromTheDateOfTheCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)

	clock := &stoppedClock{at: time.Date(2026, 6, 13, 23, 59, 50, 0, time.UTC)}
	src := &midnightSource{
		fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/hello")},
		clock:         clock,
		after:         time.Date(2026, 6, 14, 0, 0, 5, 0, time.UTC),
	}
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock.now,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if want := "github.com/octo/manifests/20260614T000005Z.manifest.json"; res.ManifestKey != want {
		t.Fatalf("manifest %s, want %s", res.ManifestKey, want)
	}
	if got := res.Manifest.Repos[0].Artifacts[0].Key; got != "github.com/octo/hello/2026-06-13/hello.bundle" {
		t.Fatalf("bundle %s, want it dated the 13th", got)
	}

	got, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
		Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13",
		OutDir: filepath.Join(t.TempDir(), "restored"),
	})
	if err != nil {
		t.Fatalf("restore by the date of the copy: %v", err)
	}
	if !strings.Contains(got.Verification, "bundle verified against the signed manifest") {
		t.Errorf("Verification = %q", got.Verification)
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return time.Date(2026, 6, 14, 1, 0, 0, 0, time.UTC) },
	}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if err != nil || drilled.Report.Status != pipeline.StatusSuccess {
		t.Fatalf("drill of a run that crossed midnight: %v", err)
	}
}

// midnightBetween moves the clock past midnight when the run first asks about the repository
// named second. With one repository at a time, that is after the first one's copy has finished.
type midnightBetween struct {
	*fixtureSource
	clock  *stoppedClock
	second string
	after  time.Time
}

func (s *midnightBetween) CloneURL(ctx context.Context, r source.Repo) (string, error) {
	if r.Name == s.second {
		s.clock.set(s.after)
	}
	return s.fixtureSource.CloneURL(ctx, r)
}

// A run files every copy under the date it started, whatever the clock says when it reaches each
// repository, and each copy's copiedAt is when that repository finished.
//
// The date was read again for each repository, so a run that crossed midnight UTC filed the
// repositories it reached before midnight under one date and the rest under the next. A same-day
// rerun then looked for half of them under the wrong date.
func TestARunFilesEveryCopyUnderTheDateItStarted(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)

	started := time.Date(2026, 6, 13, 23, 59, 50, 0, time.UTC)
	midnight := time.Date(2026, 6, 14, 0, 0, 5, 0, time.UTC)
	clock := &stoppedClock{at: started}
	src := &midnightBetween{
		fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/before", "octo/after")},
		clock:         clock, second: "after", after: midnight,
	}
	cfg := testConfig()
	cfg.Source.Repo = ""
	cfg.Backup.Concurrency = 1
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: clock.now,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	// The second repository was reached after midnight, or this test proves nothing.
	if !res.Manifest.FinishedAt.Equal(midnight) {
		t.Fatalf("the run finished at %s, want %s: the clock never crossed midnight", res.Manifest.FinishedAt, midnight)
	}

	finished := map[string]time.Time{"octo/before": started, "octo/after": midnight}
	for _, e := range res.Manifest.Repos {
		name := strings.TrimPrefix(e.Slug, "octo/")
		want := "github.com/" + e.Slug + "/2026-06-13/" + name + ".bundle"
		var got string
		for _, a := range e.Artifacts {
			if a.Kind == "bundle" {
				got = a.Key
			}
		}
		if got != want {
			t.Errorf("%s: bundle stored at %q, want %q, under the date the run started", e.Slug, got, want)
		}
		if e.CopiedAt == nil || !e.CopiedAt.Equal(finished[e.Slug]) {
			t.Errorf("%s: copiedAt %v, want %s, when this repository's copy finished", e.Slug, e.CopiedAt, finished[e.Slug])
		}

		if _, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
			Host: "github.com", Owner: "octo", Name: name, Date: "2026-06-13",
			OutDir: filepath.Join(t.TempDir(), name),
		}); err != nil {
			t.Errorf("%s: restore by the date the run started: %v", e.Slug, err)
		}
	}
}

// restore -manifest: the key backup printed, and -repo to pick the repository in it.
func TestRestoreFromANamedManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	repos := slugRepos("gitlab.com", initFixtureRepo(t), "acme/app", "acme/platform/api")
	day1 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	first := backupAt(t, md, signer, day1, repos)
	// Nothing changed by the next day, so that run copies nothing and records both as skipped.
	second := backupAt(t, md, signer, day1.AddDate(0, 0, 1), repos)

	// A drill report signed with the same key: a signed document that is not a manifest.
	drill, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer, ToolVersion: "test",
		Now: func() time.Time { return day1.Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: first.ManifestKey, WorkDir: t.TempDir()})
	if err != nil || drill.ReportKey == "" {
		t.Fatalf("drill: %v, report %q", err, drill.ReportKey)
	}
	// And a byte copy of the first manifest under a later name.
	renamed := "gitlab.com/acme/manifests/20260613T130000Z.manifest.json"
	plantCopy(t, md, first.ManifestKey, renamed)

	for _, tc := range []struct {
		name    string
		noKey   bool
		req     pipeline.RestoreRequest
		want    string   // the bundle restored
		wantErr []string // what the refusal says
	}{
		{
			name: "a project in a subgroup",
			req:  pipeline.RestoreRequest{ManifestKey: first.ManifestKey, Owner: "acme/platform", Name: "api"},
			want: "gitlab.com/acme/platform/api/2026-06-13/api.bundle",
		},
		{
			name:    "without the public key",
			noKey:   true,
			req:     pipeline.RestoreRequest{ManifestKey: first.ManifestKey, Owner: "acme", Name: "app"},
			wantErr: []string{"needs the public key"},
		},
		{
			name:    "with a host of its own",
			req:     pipeline.RestoreRequest{ManifestKey: first.ManifestKey, Host: "gitlab.com", Owner: "acme", Name: "app"},
			wantErr: []string{"the host and date come from the manifest"},
		},
		{
			name:    "with a date of its own",
			req:     pipeline.RestoreRequest{ManifestKey: first.ManifestKey, Owner: "acme", Name: "app", Date: "2026-06-13"},
			wantErr: []string{"the host and date come from the manifest"},
		},
		{
			// The slug, exactly. The run copied an api, and it is acme/platform's, not acme's.
			name:    "a repository the run did not copy",
			req:     pipeline.RestoreRequest{ManifestKey: first.ManifestKey, Owner: "acme", Name: "api"},
			wantErr: []string{"the manifest " + first.ManifestKey + ` records no repository "acme/api"`},
		},
		{
			name: "a repository the run skipped",
			req:  pipeline.RestoreRequest{ManifestKey: second.ManifestKey, Owner: "acme", Name: "app"},
			wantErr: []string{
				`records "acme/app" as skipped (unchanged since 2026-06-13)`,
				"a copy is recorded by the manifest of the run that made it",
			},
		},
		{
			name:    "a drill report",
			req:     pipeline.RestoreRequest{ManifestKey: drill.ReportKey, Owner: "acme", Name: "app"},
			wantErr: []string{drill.ReportKey + ` is a "gitdr.drill/v1" document, not a run-manifest: refusing it`},
		},
		{
			name: "a copy under a later name",
			req:  pipeline.RestoreRequest{ManifestKey: renamed, Owner: "acme", Name: "app"},
			wantErr: []string{renamed + " is named for a run that finished at 2026-06-13T13:00:00Z, but it records finishedAt 2026-06-13T12:00:00Z: " +
				"it is not the manifest its name says; refusing it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := restoreDeps(md, pub)
			if tc.noKey {
				deps.PublicKey = nil
			}
			tc.req.OutDir = filepath.Join(t.TempDir(), "restored")
			got, err := pipeline.Restore(ctx, deps, tc.req)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("restore: %v", err)
				}
				if got.BundleKey != tc.want || !strings.Contains(got.Verification, "bundle verified against the signed manifest") {
					t.Errorf("restored %s (%s), want %s verified against the signed manifest", got.BundleKey, got.Verification, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("restored %s, want a refusal", got.BundleKey)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q\ndoes not say %q", err, want)
				}
			}
		})
	}
}

// A manifest that records a repository as copied has to say where its bundle is, in the layout
// backup writes, before restore -manifest fetches anything on its word.
func TestRestoreFromANamedManifestHoldsItToTheBundleItRecords(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name    string
		edit    func(*pipeline.Manifest)
		wantErr string
	}{
		{
			name: "no bundle recorded",
			edit: func(m *pipeline.Manifest) {
				var kept []pipeline.ArtifactInfo
				for _, a := range m.Repos[0].Artifacts {
					if a.Kind != "bundle" {
						kept = append(kept, a)
					}
				}
				m.Repos[0].Artifacts = kept
			},
			wantErr: `records "acme/app" as copied and names no bundle for it`,
		},
		{
			name: "a bundle outside the layout",
			edit: func(m *pipeline.Manifest) {
				for i, a := range m.Repos[0].Artifacts {
					if a.Kind == "bundle" {
						m.Repos[0].Artifacts[i].Key = "gitlab.com/acme/app/2026-06-13/other.bundle"
					}
				}
			},
			wantErr: "at gitlab.com/acme/app/2026-06-13/other.bundle, which is not a key gitdr writes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newMemDest(true)
			pub, signer := drillKeys(t)
			res := backupAt(t, md, signer, time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
				slugRepos("gitlab.com", initFixtureRepo(t), "acme/app"))
			resign(t, md, signer, res.ManifestKey, tc.edit)

			_, err := pipeline.Restore(context.Background(), restoreDeps(md, pub), pipeline.RestoreRequest{
				ManifestKey: res.ManifestKey, Owner: "acme", Name: "app", OutDir: filepath.Join(t.TempDir(), "restored"),
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// Issue #60: which manifest a drill without -manifest takes.
//
// It took the lexically last key under {host}/{owner}/manifests/. A byte copy of an old manifest
// stored under a later name therefore stayed the newest for good, and a GitLab subgroup called
// manifests put its own manifests under that prefix, one level down, sorting after all of them.
func TestADrillPicksTheNewestManifestFiledUnderTheNamespace(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	dir := initFixtureRepo(t)
	day := func(d int) time.Time { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC) }

	old := backupAt(t, md, signer, day(13), slugRepos("gitlab.com", dir, "acme/app"))
	commit(t, dir, "second.txt", "more")
	newest := backupAt(t, md, signer, day(14), slugRepos("gitlab.com", dir, "acme/app"))
	// The subgroup acme/manifests, backed up on its own: its manifest is a real one, filed at
	// gitlab.com/acme/manifests/manifests/.
	sub := backupAt(t, md, signer, day(15), slugRepos("gitlab.com", dir, "acme/manifests/tool"))
	if want := "gitlab.com/acme/manifests/manifests/20260615T120000Z.manifest.json"; sub.ManifestKey != want {
		t.Fatalf("the subgroup's manifest is at %s, want %s", sub.ManifestKey, want)
	}
	// A copy of the old manifest named in the future.
	plantCopy(t, md, old.ManifestKey, "gitlab.com/acme/manifests/20991231T235959Z.manifest.json")

	drill := func() (*pipeline.DrillResult, error) {
		return pipeline.Drill(ctx, pipeline.DrillDeps{
			Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
			Now: func() time.Time { return day(16) },
		}, pipeline.DrillRequest{Host: "gitlab.com", Owner: "acme", WorkDir: t.TempDir()})
	}
	res, err := drill()
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if r := res.Report; r.ManifestKey != newest.ManifestKey || r.Status != pipeline.StatusSuccess || r.Repos[0].Slug != "acme/app" {
		t.Fatalf("drilled %s (%s, %v), want %s", r.ManifestKey, r.Status, r.Repos, newest.ManifestKey)
	}

	// A copy of the old manifest under a name after the newest and not in the future is picked,
	// and refused by its name. The drill fails until a newer run is filed, and never drills the
	// old run under the new name.
	plantCopy(t, md, old.ManifestKey, "gitlab.com/acme/manifests/20260615T000000Z.manifest.json")
	res, err = drill()
	want := "gitlab.com/acme/manifests/20260615T000000Z.manifest.json is named for a run that finished at 2026-06-15T00:00:00Z, " +
		"but it records finishedAt 2026-06-13T12:00:00Z: it is not the manifest its name says; refusing it"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if res != nil {
		t.Errorf("a report about a manifest that is not what its name says: %+v", res.Report)
	}
}

// Issue #61: a drill accepted any document signed with the key as its manifest, and reported
// manifestSigned over a drill report.
func TestADrillRefusesASignedDocumentThatIsNotAManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	at := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	res := backupAt(t, md, signer, at, slugRepos("github.com", initFixtureRepo(t), "octo/hello"))

	deps := pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer, ToolVersion: "test",
		Now: func() time.Time { return at.Add(time.Hour) },
	}
	first, err := pipeline.Drill(ctx, deps, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if err != nil || first.ReportKey == "" {
		t.Fatalf("drill: %v, report %q", err, first.ReportKey)
	}

	again, err := pipeline.Drill(ctx, deps, pipeline.DrillRequest{ManifestKey: first.ReportKey, WorkDir: t.TempDir()})
	want := first.ReportKey + ` is a "gitdr.drill/v1" document, not a run-manifest: refusing it`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if again != nil {
		t.Errorf("a report about a drill report: %+v", again.Report)
	}
}

// A restore without -manifest passes over a candidate the loader refuses, says so, and counts it.
func TestARestoreWarnsPastAManifestThatIsNotWhatItsNameSays(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	res := backupAt(t, md, signer, time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC), slugRepos("github.com", initFixtureRepo(t), "octo/hello"))
	// Newer than the real one and of the same day, so it is tried first.
	copied := "github.com/octo/manifests/20260613T130000Z.manifest.json"
	plantCopy(t, md, res.ManifestKey, copied)

	var logged bytes.Buffer
	deps := restoreDeps(md, pub)
	deps.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	req := pipeline.RestoreRequest{Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13"}

	req.OutDir = filepath.Join(t.TempDir(), "restored")
	if _, err := pipeline.Restore(ctx, deps, req); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var warned bool
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, copied) && strings.Contains(line, "is named for a run that finished at") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about %s:\n%s", copied, logged.String())
	}

	// Without the real one, the copy is not enough, and the refusal counts what it passed over.
	md.mu.Lock()
	delete(md.objs, res.ManifestKey)
	delete(md.objs, res.ManifestKey+".sig")
	md.mu.Unlock()
	req.OutDir = filepath.Join(t.TempDir(), "again")
	_, err := pipeline.Restore(ctx, deps, req)
	for _, want := range []string{
		"no signed manifest records github.com/octo/hello/2026-06-13/hello.bundle",
		"1 of the 1 manifests that finished on 2026-06-13 or 2026-06-14, under github.com/octo/manifests/, github.com/manifests/",
		"pass -manifest <key>",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v\ndoes not say %q", err, want)
		}
	}
}
