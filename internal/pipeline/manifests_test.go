package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
			// picked, and then refused by its name: every repository is copied, and the log says why.
			name:    "a copy under a later name is refused",
			plant:   map[string][]byte{"github.com/octo/manifests/20260902T000000Z.manifest.json": old},
			want:    "",
			warning: []string{"is named for a run that finished at 2026-09-02T00:00:00Z, but it records finishedAt 2026-08-31T12:00:00Z"},
		},
		{
			name: "a document that is not a manifest is refused",
			plant: map[string][]byte{
				"github.com/octo/manifests/20260902T000000Z.manifest.json": []byte(`{"schema":"gitdr.drill/v1","finishedAt":"2026-09-02T00:00:00Z"}`),
			},
			want:    "",
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
			r := &backupRun{
				dst: &stubDest{objs: objs}, log: slog.New(slog.NewTextHandler(&logged, nil)),
				now: func() time.Time { return sept(2, 12) },
			}
			got := r.loadPrevious(context.Background(), "github.com/octo/manifests")
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
