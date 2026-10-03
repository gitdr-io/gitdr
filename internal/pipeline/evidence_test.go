package pipeline_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

// A skip trusts only evidence a run could have produced. The Vault record's gate 5, for the two
// checks that need no signature: a copy's copiedAt, and the time the store wrote an object.

// plantManifest stores a copy of the manifest at from, finished at finished and edited by edit,
// under the name of that finish, signed with signer, as if the store had written it then. It
// returns the key.
func plantManifest(t *testing.T, md *memDest, signer ed25519.PrivateKey, from string, finished time.Time, edit func(*pipeline.Manifest)) string {
	t.Helper()
	md.mu.Lock()
	defer md.mu.Unlock()
	var m pipeline.Manifest
	if err := json.Unmarshal(md.objs[from], &m); err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	m.FinishedAt = finished
	edit(&m)
	canon, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	key := path.Join(path.Dir(from), finished.UTC().Format("20060102T150405Z")+".manifest.json")
	md.objs[key] = canon
	md.objs[key+".sig"] = []byte(base64.StdEncoding.EncodeToString(crypto.Sign(signer, canon)))
	md.modified[key], md.modified[key+".sig"] = finished, finished
	return key
}

// A copiedAt later than the manifest that holds it, or later than now, is not one a run recorded,
// and no skip rests on it.
//
// A skip measures the age of the copy it relies on from copiedAt, and refreshes the copy once that
// age passes a bound. An age that is negative never passes it. So one manifest that named a copy
// made in the future skipped the repository for good, every run green, until that copy's object
// lock ran out and there was no copy at all.
func TestASkipRefusesACopiedAtNoRunCouldHaveRecorded(t *testing.T) {
	day1 := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	planted := day1.Add(time.Hour) // the planted manifest's finish

	for _, when := range []struct {
		name     string
		copiedAt time.Time
	}{
		{"in the future", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"after its manifest finished", day1.Add(14 * time.Hour)},
	} {
		for _, judge := range []struct {
			name string
			// at is when the run that has to judge the planted manifest runs.
			at time.Time
			// want is what that run must record for the repository: a copy, or a refusal.
			want string
		}{
			{"the next day, comparing refs", day1.AddDate(0, 0, 1), pipeline.StatusSuccess},
			{"a same-day rerun", day1.Add(3 * time.Hour), pipeline.StatusFailed},
		} {
			t.Run(when.name+", "+judge.name, func(t *testing.T) {
				t.Chdir(t.TempDir())
				md := newMemDest(true)
				signer := testSigner(t)
				repos := slugRepos("github.com", initFixtureRepo(t), "octo/hello")
				first := backupAt(t, md, signer, day1, repos)

				plantManifest(t, md, signer, first.ManifestKey, planted, func(m *pipeline.Manifest) {
					at := when.copiedAt
					m.Repos[0].CopiedAt = &at
				})

				md.storeAt(judge.at)
				res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
					Config: testConfig(), Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
					SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return judge.at },
				})
				e := res.Manifest.Repos[0]
				if e.Status != judge.want {
					t.Fatalf("the run recorded %s %q %q (err %v), want %s: it relied on a copiedAt of %s",
						e.Status, e.Reason, e.Error, err, judge.want, when.copiedAt.Format(time.RFC3339))
				}
				if judge.want == pipeline.StatusFailed && !strings.Contains(e.Error, "cannot be believed") {
					t.Errorf("the refusal does not say why: %q", e.Error)
				}
			})
		}
	}
}

// An object counts toward a same-day rerun only if the store wrote it on the date its key names.
//
// A run writes the objects of a date on that date. Objects planted ahead of a date, a manifest
// recording them among them, used to make that day's run skip the repository: it found a copy
// and a record of it, and the copy was never made by that day's run. Now that day's run fails the
// repository by name, and so does a store that does not say when it wrote an object.
func TestAnObjectNotWrittenOnItsDateIsNotACopy(t *testing.T) {
	day := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// storedAt is when the store wrote the first run's objects; zero for a store that does
		// not say.
		storedAt time.Time
		want     string
	}{
		{
			name:     "written two days before its date",
			storedAt: day.AddDate(0, 0, -2),
			want:     "was not written that day (github.com/octo/hello/2026-06-13/hello.bundle is filed under the date and the destination wrote it at 2026-06-11T09:00:00Z)",
		},
		{
			name: "written at a time the store does not say",
			want: "is filed under the date and the destination does not say when it was written",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			md := newMemDest(true)
			signer := testSigner(t)
			repos := slugRepos("github.com", initFixtureRepo(t), "octo/hello")
			run := func(at, storedAt time.Time) (*pipeline.BackupResult, error) {
				md.mu.Lock()
				md.clock = nil
				if !storedAt.IsZero() {
					md.clock = func() time.Time { return storedAt }
				}
				md.mu.Unlock()
				return pipeline.Backup(context.Background(), pipeline.BackupDeps{
					Config: testConfig(), Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
					SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return at },
				})
			}

			if _, err := run(day, tc.storedAt); err != nil {
				t.Fatalf("the first run: %v", err)
			}
			res, err := run(day.Add(3*time.Hour), day.Add(3*time.Hour))
			if err == nil {
				t.Error("the rerun exited 0")
			}
			e := res.Manifest.Repos[0]
			if e.Status != pipeline.StatusFailed || !strings.Contains(e.Error, tc.want) {
				t.Errorf("rerun = %s %q %q\nwant failed saying %q", e.Status, e.Reason, e.Error, tc.want)
			}
		})
	}
}
