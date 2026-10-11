package pipeline_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/logging"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// recopyConfig is the config of a run over every repository the source lists, with recopy named for
// a recopy.
func recopyConfig(recopy ...string) *config.Config {
	cfg := testConfig()
	cfg.Source.Repo = ""
	cfg.Backup.Recopy = recopy
	return cfg
}

// runLogged backs up repos into md under cfg, on a clock stopped at at and the store's with it, and
// returns what the run logged, in the JSON gitdr writes to stderr, beside what it returned.
func runLogged(md *memDest, cfg *config.Config, signer ed25519.PrivateKey, at time.Time, repos []source.Repo) (*pipeline.BackupResult, string, error) {
	md.storeAt(at)
	var logged bytes.Buffer
	res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
		Config: cfg, Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
		Logger: logging.New("info", "json", &logged),
	})
	return res, logged.String(), err
}

// entryFor is the manifest's entry for slug.
func entryFor(t *testing.T, res *pipeline.BackupResult, slug string) pipeline.RepoEntry {
	t.Helper()
	if res == nil || res.Manifest == nil {
		t.Fatalf("no manifest to find %s in", slug)
	}
	for _, e := range res.Manifest.Repos {
		if e.Slug == slug {
			return e
		}
	}
	t.Fatalf("the manifest has no entry for %s", slug)
	return pipeline.RepoEntry{}
}

// warnings is the WARN lines of a JSON log that name repo.
func warnings(t *testing.T, log, repo string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var l struct{ Level, Repo string }
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		if l.Level == "WARN" && l.Repo == repo {
			out = append(out, line)
		}
	}
	return out
}

// A repository named for a recopy is copied although its refs have not moved, and one that is not
// named is still skipped. The copy is an ordinary one, which the next run relies on like any other.
//
// The unchanged skip relies on the last copy until its refresh, up to thirty days later, and there
// was no way to stop it. A copy that failed when it was read back went on standing for its
// repository all that time, while every run skipped the repository as protected.
func TestARecopyCopiesAnUnchangedRepositoryItNames(t *testing.T) {
	t.Chdir(t.TempDir())
	md := newMemDest(true)
	signer := testSigner(t)
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/named", "octo/other")
	day := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	backupAt(t, md, signer, day, repos)

	res, _, err := runLogged(md, recopyConfig("octo/named"), signer, day.AddDate(0, 0, 1), repos)
	if err != nil {
		t.Fatalf("the run that names a recopy: %v", err)
	}
	named := entryFor(t, res, "octo/named")
	if named.Status != pipeline.StatusSuccess {
		t.Errorf("octo/named = %s %q, want copied again although nothing moved", named.Status, named.Reason)
	}
	if got := storedNames(md, "github.com/octo/named/2026-06-14/"); !slices.Equal(got, []string{"named.bundle", "named.meta.json", "named.sha256"}) {
		t.Errorf("octo/named has %v under 2026-06-14, want a whole copy", got)
	}
	if named.CopiedAt == nil || len(named.Refs) == 0 {
		t.Errorf("the recopy recorded copiedAt %v and %d refs; the next run has nothing to compare against", named.CopiedAt, len(named.Refs))
	}
	other := entryFor(t, res, "octo/other")
	if other.Status != pipeline.StatusSkipped || other.Reason != pipeline.ReasonUnchanged+" 2026-06-13" {
		t.Errorf("octo/other = %s %q, want skipped as %q", other.Status, other.Reason, pipeline.ReasonUnchanged+" 2026-06-13")
	}
	if got := storedNames(md, "github.com/octo/other/2026-06-14/"); len(got) != 0 {
		t.Errorf("octo/other, not named, has %v under 2026-06-14", got)
	}

	// The next day names nothing, and both are skipped, octo/named on the copy the recopy made.
	next := backupAt(t, md, signer, day.AddDate(0, 0, 2), repos)
	for slug, since := range map[string]string{"octo/named": "2026-06-14", "octo/other": "2026-06-13"} {
		if e := entryFor(t, next, slug); e.Status != pipeline.StatusSkipped || e.Reason != pipeline.ReasonUnchanged+" "+since {
			t.Errorf("the next day %s = %s %q, want skipped as %q", slug, e.Status, e.Reason, pipeline.ReasonUnchanged+" "+since)
		}
	}
}

// A name the source does not list is warned about, by name, and the run goes on: the repositories
// it did list are backed up as they would have been, and the run exits 0. A name is the slug as the
// manifest records it, matched exactly, so one that differs only in case is not that repository.
func TestARecopyOfARepositoryTheSourceDoesNotListWarnsAndTheRunGoesOn(t *testing.T) {
	t.Chdir(t.TempDir())
	md := newMemDest(true)
	signer := testSigner(t)
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/named")
	day := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	backupAt(t, md, signer, day, repos)

	res, logs, err := runLogged(md, recopyConfig("octo/gone", "Octo/Named"), signer, day.AddDate(0, 0, 1), repos)
	if err != nil {
		t.Fatalf("a name the source does not list failed the run: %v", err)
	}
	if res.Manifest.Status != pipeline.StatusSuccess || len(res.Manifest.Repos) != 1 {
		t.Errorf("the run = %s with %d entries, want success with the one repository the source listed", res.Manifest.Status, len(res.Manifest.Repos))
	}
	if e := entryFor(t, res, "octo/named"); e.Status != pipeline.StatusSkipped || e.Reason != pipeline.ReasonUnchanged+" 2026-06-13" {
		t.Errorf("octo/named = %s %q, want skipped as %q", e.Status, e.Reason, pipeline.ReasonUnchanged+" 2026-06-13")
	}
	for _, name := range []string{"octo/gone", "Octo/Named"} {
		if got := warnings(t, logs, name); len(got) != 1 {
			t.Errorf("%d warnings name %s, want one:\n%s", len(got), name, logs)
		}
	}
}

// A recopy cannot replace a copy made earlier the same UTC date: its keys are that date's, and they
// are create-only. The rerun does with the repository exactly what it does without the name, and
// says the recopy waits for the next date, where a run that names the repository copies it.
func TestARecopyCannotReplaceASameDayCopy(t *testing.T) {
	morning := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	afternoon := morning.Add(6 * time.Hour)
	const dated = "github.com/octo/named/2026-06-13/"

	for _, tc := range []struct {
		name   string
		refuse string // what the morning's copy is refused, so that it does not finish
		want   string // the afternoon's entry, as status: reason or error
	}{
		{"a copy a manifest records", "", pipeline.StatusSkipped + ": " + pipeline.ReasonResume},
		{"a copy that did not finish", "named.sha256", pipeline.StatusFailed + ": an incomplete copy for 2026-06-13 exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			signer := testSigner(t)
			repos := slugRepos("github.com", initFixtureRepo(t), "octo/named")
			// Two buckets with the same morning. The afternoon's rerun names the repository in one of
			// them and not in the other, and both have to come out the same.
			md, twin := newMemDest(true), newMemDest(true)
			for _, d := range []*memDest{md, twin} {
				d.refuse = tc.refuse
				if _, _, err := runLogged(d, recopyConfig(), signer, morning, repos); (err == nil) != (tc.refuse == "") {
					t.Fatalf("the morning run: %v", err)
				}
				d.refuse = ""
			}
			before := storedNames(md, dated)

			res, logs, err := runLogged(md, recopyConfig("octo/named"), signer, afternoon, repos)
			twinRes, _, twinErr := runLogged(twin, recopyConfig(), signer, afternoon, repos)
			e := entryFor(t, res, "octo/named")
			if got := e.Status + ": " + e.Reason + e.Error; !strings.HasPrefix(got, tc.want) {
				t.Errorf("the rerun that names it = %q, want it to start %q", got, tc.want)
			}
			got, _ := json.Marshal(e)
			want, _ := json.Marshal(entryFor(t, twinRes, "octo/named"))
			if !bytes.Equal(got, want) {
				t.Errorf("the rerun records\n%s\nfor the repository it names, and without the name\n%s", got, want)
			}
			if (err == nil) != (twinErr == nil) {
				t.Errorf("the rerun that names it returned %v, and without the name %v", err, twinErr)
			}
			if after := storedNames(md, dated); !slices.Equal(after, before) {
				t.Errorf("the rerun changed what is under the date: %v, was %v", after, before)
			}
			if got := warnings(t, logs, "octo/named"); len(got) != 1 || !strings.Contains(got[0], "2026-06-13") {
				t.Errorf("want one warning that the recopy waits for the next date:\n%s", logs)
			}

			next, _, err := runLogged(md, recopyConfig("octo/named"), signer, morning.AddDate(0, 0, 1), repos)
			if err != nil || entryFor(t, next, "octo/named").Status != pipeline.StatusSuccess {
				t.Fatalf("the next date's run that names it did not copy it: %v", err)
			}
			if got := storedNames(md, "github.com/octo/named/2026-06-14/"); !slices.Equal(got, []string{"named.bundle", "named.meta.json", "named.sha256"}) {
				t.Errorf("octo/named has %v under 2026-06-14, want a whole copy", got)
			}
		})
	}

	// With resume off nothing looks under the date first. The copy is attempted, and the first key
	// already there is refused, as for any repository whose refs moved since its copy that day.
	t.Run("with resume off", func(t *testing.T) {
		t.Chdir(t.TempDir())
		md := newMemDest(true)
		signer := testSigner(t)
		repos := slugRepos("github.com", initFixtureRepo(t), "octo/named")
		cfg := recopyConfig()
		cfg.Backup.Resume = false
		if _, _, err := runLogged(md, cfg, signer, morning, repos); err != nil {
			t.Fatalf("the morning run: %v", err)
		}
		before := storedNames(md, dated)

		cfg.Backup.Recopy = []string{"octo/named"}
		res, _, err := runLogged(md, cfg, signer, afternoon, repos)
		if e := entryFor(t, res, "octo/named"); err == nil || e.Status != pipeline.StatusFailed || !strings.Contains(e.Error, "already exists") {
			t.Errorf("the rerun = %s %q %q (%v), want failed on a key that already exists", e.Status, e.Reason, e.Error, err)
		}
		if after := storedNames(md, dated); !slices.Equal(after, before) {
			t.Errorf("the rerun changed what is under the date: %v, was %v", after, before)
		}
	})
}
