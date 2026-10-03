package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/source"
)

// Where a run files its manifest: the deepest namespace that holds every repository in it.
//
// It was the first repository's owner, which is whichever namespace the source listed first. For
// a GitLab run over a group with subgroups, or over a group and a user's projects, that filed the
// manifest where a restore of every other namespace did not look, and it moved whenever a project
// was created, which made the next run copy everything again.
func TestTheManifestIsFiledUnderTheNamespaceEveryRepositoryShares(t *testing.T) {
	reposOf := func(owners ...string) []source.Repo {
		var repos []source.Repo
		for i, o := range owners {
			repos = append(repos, source.Repo{Host: "gitlab.com", Owner: o, Name: string(rune('a' + i))})
		}
		return repos
	}
	for _, tc := range []struct {
		name   string
		repos  []source.Repo
		anchor string
		dir    string
	}{
		{"one organisation", reposOf("octo", "octo", "octo"), "octo", "gitlab.com/octo/manifests"},
		{"one project in a subgroup", reposOf("acme/platform"), "acme/platform", "gitlab.com/acme/platform/manifests"},
		{"a group and its subgroup", reposOf("acme", "acme/platform"), "acme", "gitlab.com/acme/manifests"},
		{"the subgroup listed first", reposOf("acme/platform", "acme"), "acme", "gitlab.com/acme/manifests"},
		{"two subgroups of one group", reposOf("acme/platform/api", "acme/tools"), "acme", "gitlab.com/acme/manifests"},
		{"a subgroup and one below it", reposOf("acme/platform", "acme/platform/internal"), "acme/platform", "gitlab.com/acme/platform/manifests"},
		{"a group and a user", reposOf("acme", "alice"), "", "gitlab.com/manifests"},
		{"a group, its subgroup and a user", reposOf("acme/platform", "acme", "alice"), "", "gitlab.com/manifests"},
		// Segments, not string prefixes: acme-corp is not inside acme.
		{"names that share a prefix and not a segment", reposOf("acme", "acme-corp"), "", "gitlab.com/manifests"},
		// Byte for byte. GitLab routes paths case-insensitively, and the keys are written as the
		// source spells them, so two spellings are two places.
		{"case", reposOf("Acme/x", "acme/y"), "", "gitlab.com/manifests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manifestAnchor(tc.repos); got != tc.anchor {
				t.Errorf("anchor = %q, want %q", got, tc.anchor)
			}
			if got := manifestDir(tc.repos); got != tc.dir {
				t.Errorf("dir = %q, want %q", got, tc.dir)
			}
			// The listing order must not move it.
			reversed := slices.Clone(tc.repos)
			slices.Reverse(reversed)
			if got := manifestDir(reversed); got != tc.dir {
				t.Errorf("listed the other way round, dir = %q, want %q", got, tc.dir)
			}
		})
	}
}

// Where a restore without -manifest looks, and in what order: the repository's own namespace,
// each one above it, then the host.
func TestARestoreSearchesFromTheRepositoryUpToTheHost(t *testing.T) {
	for _, tc := range []struct {
		owner string
		want  []string
	}{
		{"octo", []string{"github.com/octo/manifests", "github.com/manifests"}},
		{"acme/platform/api-team", []string{
			"github.com/acme/platform/api-team/manifests",
			"github.com/acme/platform/manifests",
			"github.com/acme/manifests",
			"github.com/manifests",
		}},
		{"", []string{"github.com/manifests"}},
	} {
		if got := manifestSearchDirs("github.com", tc.owner); !slices.Equal(got, tc.want) {
			t.Errorf("manifestSearchDirs(%q) = %q, want %q", tc.owner, got, tc.want)
		}
	}
}

// Only a manifest filed directly in the directory, and named like one, is a candidate.
func TestOnlyAManifestFiledDirectlyInTheDirectoryIsACandidate(t *testing.T) {
	objs := []dest.Object{
		{Key: "gitlab.com/acme/manifests/20260613T120000Z.manifest.json"},
		{Key: "gitlab.com/acme/manifests/20260613T120000Z.manifest.json.sig"},
		{Key: "gitlab.com/acme/manifests/20260614T000500Z.manifest.json"},
		// The manifests of a subgroup called acme/manifests, one level down, sorting last.
		{Key: "gitlab.com/acme/manifests/manifests/20260615T000000Z.manifest.json"},
		// A project in that subgroup whose name starts like a date.
		{Key: "gitlab.com/acme/manifests/20260613/2026-06-13/20260613.bundle"},
		{Key: "gitlab.com/acme/manifests/latest.manifest.json"},
		{Key: "gitlab.com/acme/manifests/20260613T120000Z.drill.json"},
	}
	got := filedManifests(objs, "gitlab.com/acme/manifests")
	want := []string{
		"gitlab.com/acme/manifests/20260614T000500Z.manifest.json",
		"gitlab.com/acme/manifests/20260613T120000Z.manifest.json",
	}
	if !slices.Equal(got, want) {
		t.Errorf("filedManifests = %q, want %q, newest first", got, want)
	}
}

// verify reads a manifest only up to the cap, as every other reader does, and says why it stopped.
// It used to read the whole object, so it read manifests that restore and drill then refused.
func TestVerifyReadsAManifestOnlyUpToTheCap(t *testing.T) {
	const key = "github.com/octo/manifests/20260901T120000Z.manifest.json"
	d := &stubDest{objs: map[string][]byte{key: []byte(`{"schema":"gitdr.manifest/v5"}`)}}
	_, err := verifyWithin(context.Background(), VerifyDeps{Dest: d}, key, 16)
	if err == nil || !strings.Contains(err.Error(), key+" is larger than the") {
		t.Errorf("err = %v, want a refusal of a manifest past the cap", err)
	}
}

// The loader reads at most maxManifestBytes and refuses a larger object rather than parsing a
// truncated one. It used to read one byte past the cap for the drill and never look.
func TestAManifestLargerThanTheCapIsRefused(t *testing.T) {
	d := &stubDest{objs: map[string][]byte{"k": []byte("12345")}}
	if _, err := readCapped(context.Background(), d, "k", 4); err == nil || err.Error() != "larger than 4 bytes" {
		t.Errorf("5 bytes under a cap of 4: err = %v, want a refusal", err)
	}
	if b, err := readCapped(context.Background(), d, "k", 5); err != nil || string(b) != "12345" {
		t.Errorf("5 bytes under a cap of 5: %q, %v", b, err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func manifestBytes(t *testing.T, finished time.Time, commit string) []byte {
	t.Helper()
	b, err := json.Marshal(&Manifest{
		Schema: ManifestSchema, FinishedAt: finished,
		Repos: []RepoEntry{{Slug: "octo/x", Status: StatusSuccess, Refs: []RefEntry{{Name: "refs/heads/main", Commit: commit}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The next backup reads the previous run's manifest the way a drill picks one, and trusts it only
// when it is what its name says.
func TestTheNextBackupReadsTheNewestManifestItCanTrust(t *testing.T) {
	sept := func(day, hour int) time.Time { return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC) }
	real := map[string][]byte{
		"github.com/octo/manifests/20260831T120000Z.manifest.json": manifestBytes(t, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC), "old"),
		"github.com/octo/manifests/20260901T120000Z.manifest.json": manifestBytes(t, sept(1, 12), "new"),
	}
	old := real["github.com/octo/manifests/20260831T120000Z.manifest.json"]

	for _, tc := range []struct {
		name    string
		plant   map[string][]byte
		want    string   // the commit read for octo/x, or "" for nothing read
		warning []string // what a warning in the log must say, if there must be one
	}{
		{name: "the newest", want: "new"},
		{
			// A byte copy of the old manifest stored under a name in the future. Picked, it would be
			// the newest for good.
			name:  "a copy named in the future is not picked",
			plant: map[string][]byte{"github.com/octo/manifests/20991231T235959Z.manifest.json": old},
			want:  "new",
		},
		{
			// The manifests of a subgroup called octo/manifests are one level down and sort last.
			name: "a manifest filed one level down is not picked",
			plant: map[string][]byte{
				"github.com/octo/manifests/manifests/20260902T000000Z.manifest.json": manifestBytes(t, sept(2, 0), "subgroup"),
			},
			want: "new",
		},
		{
			// A copy of the old manifest under a name later than the newest and not in the future is
			// tried first, refused by its name with a warning that says why, and passed over. What it
			// says is not believed, and the newest manifest that can be trusted is read instead.
			name:    "a copy under a later name is refused",
			plant:   map[string][]byte{"github.com/octo/manifests/20260902T000000Z.manifest.json": old},
			want:    "new",
			warning: []string{"is named for a run that finished at 2026-09-02T00:00:00Z, but it records finishedAt 2026-08-31T12:00:00Z"},
		},
		{
			name: "a document that is not a manifest is refused",
			plant: map[string][]byte{
				"github.com/octo/manifests/20260902T000000Z.manifest.json": []byte(`{"schema":"gitdr.drill/v1","finishedAt":"2026-09-02T00:00:00Z"}`),
			},
			want:    "new",
			warning: []string{"gitdr.drill/v1", "document, not a run-manifest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := map[string][]byte{}
			for k, v := range real {
				objs[k] = v
			}
			for k, v := range tc.plant {
				objs[k] = v
			}
			var logged bytes.Buffer
			r := &backupRun{pub: runPub,
				dst: &stubDest{objs: objs}, log: slog.New(slog.NewTextHandler(&logged, nil)),
				now: func() time.Time { return sept(2, 12) },
			}
			got := r.loadPrevious(context.Background(), "github.com/octo/manifests", nil)
			if commit := got["octo/x"].refs["refs/heads/main"]; commit != tc.want {
				t.Errorf("read %q, want %q", commit, tc.want)
			}
			if tc.warning == nil {
				return
			}
			var warned bool
			for _, line := range strings.Split(logged.String(), "\n") {
				if strings.Contains(line, "level=WARN") && containsAll(line, tc.warning...) {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning saying %q:\n%s", tc.warning, logged.String())
			}
		})
	}
}

// previousManifests is one manifest per element, each finished an hour before the one after it,
// the last at newest. Each names the repositories in its map, with the commit as their one ref; a
// commit of "failed" records the repository as failed.
func previousManifests(t *testing.T, newest time.Time, runs ...map[string]string) map[string][]byte {
	t.Helper()
	objs := map[string][]byte{}
	for i, run := range runs {
		finished := newest.Add(-time.Duration(len(runs)-1-i) * time.Hour)
		m := Manifest{Schema: ManifestSchema, FinishedAt: finished}
		for _, slug := range slices.Sorted(maps.Keys(run)) {
			e := RepoEntry{Slug: slug, Status: StatusSuccess, Refs: []RefEntry{{Name: "refs/heads/main", Commit: run[slug]}}}
			if run[slug] == "failed" {
				e = RepoEntry{Slug: slug, Status: StatusFailed, Error: "it failed"}
			}
			m.Repos = append(m.Repos, e)
		}
		raw, err := json.Marshal(&m)
		if err != nil {
			t.Fatal(err)
		}
		objs["github.com/octo/manifests/"+finished.Format(manifestStamp)+manifestSuffix] = raw
	}
	return objs
}

// The next run reads the recent manifests newest first, and each repository is decided by the
// newest one that has an entry for it. A failed entry decides too: its repository has no copy to
// rely on, and an older copy of it is not believed over that.
func TestThePreviousReadMergesTheRecentManifestsNewestFirst(t *testing.T) {
	r := &backupRun{pub: runPub, log: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}
	r.dst = &stubDest{objs: previousManifests(t, now.Add(-time.Hour),
		map[string]string{"octo/z": "z1", "octo/y": "y1", "octo/gone": "g1"},
		map[string]string{"octo/y": "y2", "octo/x": "x2", "octo/gone": "failed"},
		map[string]string{"octo/x": "x3"},
	)}
	got := r.loadPrevious(context.Background(), "github.com/octo/manifests",
		map[string]bool{"octo/x": true, "octo/y": true, "octo/z": true, "octo/gone": true})
	for slug, want := range map[string]string{"octo/x": "x3", "octo/y": "y2", "octo/z": "z1"} {
		if c := got[slug].refs["refs/heads/main"]; c != want {
			t.Errorf("%s: read %q, want %q, the newest manifest's", slug, c, want)
		}
	}
	if c, ok := got["octo/gone"]; ok {
		t.Errorf("octo/gone failed in a newer run, and its older copy %v was read anyway", c.refs)
	}
}

// Only the selected repositories are kept, and the read stops once they are all decided.
func TestThePreviousReadKeepsOnlyTheSelectedRepositories(t *testing.T) {
	counted := &countingStub{stubDest: stubDest{objs: previousManifests(t, now.Add(-time.Hour),
		map[string]string{"octo/x": "x1", "octo/other": "o1"},
		map[string]string{"octo/x": "x2", "octo/y": "y2"},
	)}}
	r := &backupRun{pub: runPub, dst: counted, log: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}
	got := r.loadPrevious(context.Background(), "github.com/octo/manifests", map[string]bool{"octo/x": true, "octo/y": true})
	if len(got) != 2 || got["octo/x"].refs["refs/heads/main"] != "x2" || got["octo/y"].refs["refs/heads/main"] != "y2" {
		t.Errorf("read %v, want octo/x and octo/y from the newest manifest", got)
	}
	if counted.gets != 1 {
		t.Errorf("read %d manifests, want 1: both repositories were decided by the newest", counted.gets)
	}
}

// The read goes back at most maxPreviousManifests, and never past the refresh bound: every copy a
// manifest older than that records would be refreshed anyway.
func TestThePreviousReadIsBounded(t *testing.T) {
	var runs []map[string]string
	selected := map[string]bool{}
	for i := 12; i > 0; i-- { // r12 in the oldest manifest, r1 in the newest
		slug := fmt.Sprintf("octo/r%d", i)
		runs = append(runs, map[string]string{slug: "c"})
		selected[slug] = true
	}
	r := &backupRun{pub: runPub,
		dst: &stubDest{objs: previousManifests(t, now.Add(-time.Hour), runs...)},
		log: slog.New(slog.DiscardHandler), now: func() time.Time { return now },
	}
	got := r.loadPrevious(context.Background(), "github.com/octo/manifests", selected)
	for i := 1; i <= 12; i++ {
		slug := fmt.Sprintf("octo/r%d", i)
		if _, ok := got[slug]; ok != (i <= maxPreviousManifests) {
			t.Errorf("%s read: %v, want %v: only the %d newest manifests are read", slug, ok, i <= maxPreviousManifests, maxPreviousManifests)
		}
	}

	// A manifest finished before the refresh bound is not read, however few came before it.
	objs := previousManifests(t, now.Add(-time.Hour), map[string]string{"octo/fresh": "f"})
	for k, v := range previousManifests(t, now.Add(-refreshBound(0)-time.Hour), map[string]string{"octo/stale": "s"}) {
		objs[k] = v
	}
	r.dst = &stubDest{objs: objs}
	got = r.loadPrevious(context.Background(), "github.com/octo/manifests", map[string]bool{"octo/fresh": true, "octo/stale": true})
	if _, ok := got["octo/fresh"]; !ok {
		t.Error("the manifest inside the refresh bound was not read")
	}
	if _, ok := got["octo/stale"]; ok {
		t.Error("a manifest older than the refresh bound was read")
	}
}

// countingStub counts the manifests read out of a stubDest, their signatures aside.
type countingStub struct {
	stubDest
	gets int
}

func (c *countingStub) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasSuffix(key, manifestSuffix) {
		c.gets++
	}
	return c.stubDest.Get(ctx, key)
}

// The streaming reader decodes what json.Unmarshal decodes, one repository at a time, and refuses
// what it refuses.
func TestTheStreamingReaderAgreesWithUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		bad       bool
	}{
		{name: "a manifest", doc: `{"schema":"gitdr.manifest/v5","runId":"r1","tool":{"name":"gitdr"},"startedAt":"2026-06-13T12:00:00Z","finishedAt":"2026-06-13T12:03:00Z","status":"success","repos":[{"slug":"octo/a","status":"success","artifacts":[{"kind":"bundle","key":"k","size":1,"sha256":"h","retainUntil":"2026-07-13T12:00:00Z"}],"refs":[{"name":"refs/heads/main","commit":"c1"}],"copiedAt":"2026-06-13T12:01:00Z"},{"slug":"octo/b","status":"failed","error":"e"}]}`},
		{name: "repositories before the head, and keys in other cases", doc: `{"Repos":[{"slug":"octo/a","status":"skipped","reason":"repository has no commits"}],"FINISHEDAT":"2026-06-13T12:03:00Z","schema":"gitdr.manifest/v2","extra":{"nested":[1,2,3]}}`},
		{name: "no repositories", doc: `{"schema":"gitdr.manifest/v5","finishedAt":"2026-06-13T12:03:00Z","repos":null}`},
		{name: "data after the document", doc: `{"schema":"gitdr.manifest/v5","repos":[]} {}`, bad: true},
		{name: "repositories that are not an array", doc: `{"schema":"gitdr.manifest/v5","repos":{"slug":"octo/a"}}`, bad: true},
		{name: "not an object", doc: `["gitdr.manifest/v5"]`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want Manifest
			wantErr := json.Unmarshal([]byte(tc.doc), &want)
			var got []RepoEntry
			head, err := decodeManifestStream([]byte(tc.doc), func(e RepoEntry) { got = append(got, e) })
			if (err != nil) != tc.bad || (wantErr != nil) != tc.bad {
				t.Fatalf("stream err = %v, unmarshal err = %v, want an error: %v", err, wantErr, tc.bad)
			}
			if tc.bad {
				return
			}
			if head.Schema != want.Schema || head.RunID != want.RunID || !head.FinishedAt.Equal(want.FinishedAt) {
				t.Errorf("head = %+v, want schema %q, runId %q, finishedAt %s", head, want.Schema, want.RunID, want.FinishedAt)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want.Repos)
			if len(got) == 0 && len(want.Repos) == 0 {
				return
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("entries\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}
