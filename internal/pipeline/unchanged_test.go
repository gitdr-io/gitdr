package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/source"
)

var (
	now  = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	year = 365 * 24 * time.Hour
	day  = 24 * time.Hour
)

func refs(pairs ...string) map[string]string {
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

func TestSkipsOnlyWhenNothingMoved(t *testing.T) {
	same := refs("refs/heads/main", "aaa", "HEAD", "aaa")

	cases := []struct {
		name     string
		prev     map[string]string
		cur      map[string]string
		wantSkip bool
	}{
		{"identical", same, refs("refs/heads/main", "aaa", "HEAD", "aaa"), true},
		{"a commit landed", same, refs("refs/heads/main", "bbb", "HEAD", "bbb"), false},
		// A deleted branch is a change, and a history rewritten to drop one is exactly what a
		// backup exists for. "every old ref is still there" would have missed it.
		{"a branch was deleted", refs("refs/heads/main", "aaa", "refs/heads/old", "ccc"), refs("refs/heads/main", "aaa"), false},
		{"a branch was added", refs("refs/heads/main", "aaa"), refs("refs/heads/main", "aaa", "refs/heads/new", "ddd"), false},
		// An annotated tag replaced over the same commit is a different tag object, and
		// ls-remote reports the tag object because the peeled line is dropped.
		{"a tag object was swapped", refs("refs/tags/v1", "tag1"), refs("refs/tags/v1", "tag2"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideUnchanged(c.prev, c.cur, now.Add(-day), now, year)
			if got.skip != c.wantSkip {
				t.Errorf("skip = %v, want %v (%s)", got.skip, c.wantSkip, got.reason)
			}
		})
	}
}

func TestAbsentEvidenceIsNeverAReasonToSkip(t *testing.T) {
	same := refs("refs/heads/main", "aaa")

	t.Run("no previous copy", func(t *testing.T) {
		if decideUnchanged(nil, same, now.Add(-day), now, year).skip {
			t.Error("skipped a repository that has never been copied")
		}
	})

	t.Run("previous copy recorded no refs", func(t *testing.T) {
		// A v2 manifest, or a run that failed before it could record any.
		if decideUnchanged(refs(), same, now.Add(-day), now, year).skip {
			t.Error("skipped on an empty previous ref map")
		}
	})

	t.Run("both sides empty", func(t *testing.T) {
		// The case that makes the two length guards load-bearing rather than decorative.
		// sameRefs on two empty maps is true — no lengths differ and there is nothing to
		// iterate — so without them a repository whose source answers nothing and whose last
		// manifest recorded nothing would be skipped forever on the strength of two absences
		// agreeing with each other. Found by deleting each guard and watching nothing fail.
		if decideUnchanged(refs(), refs(), now.Add(-day), now, year).skip {
			t.Error("two absences agreeing was treated as evidence of no change")
		}
		if decideUnchanged(nil, nil, now.Add(-day), now, year).skip {
			t.Error("nil on both sides was treated as evidence of no change")
		}
	})

	t.Run("the source advertises nothing", func(t *testing.T) {
		// This is the one that matters most. An expired token makes ls-remote return nothing,
		// and treating that as "unchanged" would stop every backup in an organisation at once
		// while every run reported success. That is the failure this whole product exists to
		// make impossible.
		if decideUnchanged(same, refs(), now.Add(-day), now, year).skip {
			t.Error("an unreadable source was treated as an unchanged one")
		}
	})
}

func TestACopyIsRefreshedBeforeItsLockExpires(t *testing.T) {
	same := refs("refs/heads/main", "aaa")

	// The hazard: object lock protects a copy until its retain-until date and not one second
	// longer. A repository that never changes would be skipped until its only copy aged out
	// and then have none at all, turning a saving into data loss.
	cases := []struct {
		name      string
		retention time.Duration
		age       time.Duration
		wantSkip  bool
	}{
		{"fresh copy, long retention", year, 2 * day, true},
		{"a third of the way through a 90 day retention", 90 * day, 29 * day, true},
		{"past a third of a 90 day retention", 90 * day, 31 * day, false},
		// Thirty days caps it however long the retention is: a three-year-old copy being the
		// only one is within a ten-year policy and is not what anyone means by a backup.
		{"a decade of retention still refreshes monthly", 10 * year, 31 * day, false},
		{"a decade of retention, three weeks old", 10 * year, 21 * day, true},
		// A short retention refreshes sooner, not later.
		{"seven day retention, three days old", 7 * day, 3 * day, false},
		{"seven day retention, one day old", 7 * day, 1 * day, true},
		// Nonsense configuration must not disable the bound.
		{"zero retention", 0, 2 * day, true},
		{"zero retention, old copy", 0, 40 * day, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideUnchanged(same, same, now.Add(-c.age), now, c.retention)
			if got.skip != c.wantSkip {
				t.Errorf("skip = %v, want %v (%s)", got.skip, c.wantSkip, got.reason)
			}
			// A refusal on age says why, and every case in this table hands identical refs,
			// so a refusal here can only be about age. The rule was written as an empty
			// branch guarded by a condition that asserted nothing: it named the rule and
			// checked none of it, which is how a table of nine cases proves eight things.
			if !got.skip && !strings.Contains(got.reason, "days old") {
				t.Errorf("refused on age without saying so: reason = %q", got.reason)
			}
		})
	}
}

func TestTheReasonSaysWhichCaseItWas(t *testing.T) {
	same := refs("refs/heads/main", "aaa")

	skipped := decideUnchanged(same, same, now.Add(-2*day), now, year)
	if !skipped.skip {
		t.Fatal("expected a skip")
	}
	if want := "unchanged since 2026-08-31"; skipped.reason != want {
		t.Errorf("reason = %q, want %q", skipped.reason, want)
	}

	refreshed := decideUnchanged(same, same, now.Add(-40*day), now, year)
	if refreshed.skip {
		t.Fatal("expected a refresh")
	}
	if refreshed.reason == "" {
		t.Error("a refusal on age must say so; the operator sees a full copy of an unchanged repository and is owed the reason")
	}
}

func TestRefsRoundTripThroughTheManifestDeterministically(t *testing.T) {
	in := refs("refs/tags/v2", "ccc", "refs/heads/main", "aaa", "HEAD", "aaa", "refs/heads/dev", "bbb")

	first := refsToEntries(in)
	for i := 0; i < 50; i++ {
		// Map iteration order is random, and Canonical() signs the marshalled bytes. An
		// unsorted slice would give the same run a different signature every attempt.
		if got := refsToEntries(in); !equalEntries(got, first) {
			t.Fatalf("refsToEntries is not deterministic:\n %v\n %v", first, got)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Name >= first[i].Name {
			t.Errorf("not sorted: %q before %q", first[i-1].Name, first[i].Name)
		}
	}

	back := entriesToRefs(first)
	if !sameRefs(in, back) {
		t.Errorf("round trip lost something:\n in:  %v\n out: %v", in, back)
	}
}

func TestAnEmptyRefListReadsBackAsNothingRatherThanAnEmptyMap(t *testing.T) {
	// decideUnchanged treats an empty previous map as "no evidence", and nil and empty must
	// mean the same thing there or a v2 manifest would be read as a comparison that passed.
	if got := entriesToRefs(nil); got != nil {
		t.Errorf("entriesToRefs(nil) = %v, want nil", got)
	}
	if decideUnchanged(entriesToRefs(nil), refs("a", "b"), now, now, year).skip {
		t.Error("skipped on refs read back from a manifest that had none")
	}
}

func equalEntries(a, b []RefEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// loadPrevious, read directly, because the guard that matters here cannot be reached from a
// real run.
//
// A failed entry never carries refs today: they are recorded after the copy succeeds. So the
// `StatusFailed` check in loadPrevious is unreachable through the pipeline, and deleting it
// broke no end-to-end test. It is still worth having — moving the ref recording earlier is a
// natural-looking refactor, and the result would be a repository whose copy failed being
// skipped on the strength of the refs that failure observed, leaving it with nothing.
//
// So it is asserted where it can be: against a manifest that has that shape.
func TestAFailedEntryIsNotEvidenceEvenWhenItCarriesRefs(t *testing.T) {
	finished := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m := Manifest{
		Schema:     ManifestSchema,
		FinishedAt: finished,
		Repos: []RepoEntry{
			{Slug: "octo/failed", Status: StatusFailed, Refs: []RefEntry{{Name: "refs/heads/main", Commit: "aaa"}}},
			{Slug: "octo/ok", Status: StatusSuccess, Refs: []RefEntry{{Name: "refs/heads/main", Commit: "bbb"}}},
			{Slug: "octo/skipped", Status: StatusSkipped, Refs: []RefEntry{{Name: "refs/heads/main", Commit: "ccc"}}},
		},
	}
	raw, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}

	r := &backupRun{
		dst: &stubDest{objs: map[string][]byte{
			"github.com/octo/manifests/20260901T120000Z.manifest.json": raw,
		}},
		log: slog.New(slog.DiscardHandler),
	}
	got := r.loadPrevious(context.Background(), source.Repo{Host: "github.com", Owner: "octo"})

	if _, ok := got["octo/failed"]; ok {
		t.Error("a failed copy's refs were read as evidence; its retry would be skipped and the repository would have no copy at all")
	}
	if _, ok := got["octo/ok"]; !ok {
		t.Error("a successful copy was not read")
	}
	// A skip means the previous copy is still current, and its refs were carried forward for
	// exactly this read. Excluding it made every third run a full copy.
	if _, ok := got["octo/skipped"]; !ok {
		t.Error("a skipped entry was not read; the run after a skip would copy in full")
	}
}

func TestAnUnreadablePreviousManifestMeansCopyEverything(t *testing.T) {
	// Every failure here has to answer "copy it", because not knowing what changed is the
	// same as everything having changed. A backup that fails because an optimisation could
	// not read its own bookkeeping would be a worse product than one without the optimisation.
	cases := map[string]map[string][]byte{
		"no manifests at all":     {},
		"not json":                {"github.com/octo/manifests/20260901T120000Z.manifest.json": []byte("{{{")},
		"json but not a manifest": {"github.com/octo/manifests/20260901T120000Z.manifest.json": []byte(`{"hello":"world"}`)},
		"only a signature":        {"github.com/octo/manifests/20260901T120000Z.manifest.json.sig": []byte("abc")},
	}
	for name, objs := range cases {
		t.Run(name, func(t *testing.T) {
			r := &backupRun{dst: &stubDest{objs: objs}, log: slog.New(slog.DiscardHandler)}
			if got := r.loadPrevious(context.Background(), source.Repo{Host: "github.com", Owner: "octo"}); len(got) != 0 {
				t.Errorf("got %d entries, want none", len(got))
			}
		})
	}
}

func TestTheNewestManifestWins(t *testing.T) {
	// Keys carry a sortable timestamp, so lexical order is chronological. If this ever stops
	// being true the comparison silently uses a stale ref map and skips a repository that has
	// moved since.
	mk := func(commit string, at time.Time) []byte {
		b, _ := json.Marshal(&Manifest{
			Schema: ManifestSchema, FinishedAt: at,
			Repos: []RepoEntry{{Slug: "octo/x", Status: StatusSuccess, Refs: []RefEntry{{Name: "refs/heads/main", Commit: commit}}}},
		})
		return b
	}
	r := &backupRun{dst: &stubDest{objs: map[string][]byte{
		"github.com/octo/manifests/20260101T000000Z.manifest.json": mk("old", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		"github.com/octo/manifests/20260901T120000Z.manifest.json": mk("new", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		"github.com/octo/manifests/20260501T000000Z.manifest.json": mk("mid", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)),
	}}, log: slog.New(slog.DiscardHandler)}

	got := r.loadPrevious(context.Background(), source.Repo{Host: "github.com", Owner: "octo"})
	if got["octo/x"].refs["refs/heads/main"] != "new" {
		t.Errorf("read %q, want the newest manifest's %q", got["octo/x"].refs["refs/heads/main"], "new")
	}
}

// stubDest is read-only: loadPrevious only ever lists and gets.
type stubDest struct{ objs map[string][]byte }

func (s *stubDest) VerifyWorm(context.Context) (dest.WormStatus, error) {
	return dest.WormStatus{}, nil
}
func (s *stubDest) PutImmutable(context.Context, string, io.Reader, int64, dest.Retention) (dest.PutResult, error) {
	return dest.PutResult{}, errors.New("stub is read-only")
}
func (s *stubDest) List(_ context.Context, prefix string) ([]dest.Object, error) {
	var out []dest.Object
	for k, v := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, dest.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}
func (s *stubDest) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := s.objs[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// The prefix is the contract, and a consumer matches on it.
//
// The other two reasons are whole constants a consumer compares exactly. This one carries a
// date, so it is a prefix — and if the prefix ever drifts, every consumer silently falls
// through to "skipped for some reason" on what is now the most common outcome of a run.
func TestTheUnchangedReasonKeepsItsPrefix(t *testing.T) {
	same := refs("refs/heads/main", "aaa")
	got := decideUnchanged(same, same, now.Add(-day), now, year)
	if !got.skip {
		t.Fatal("expected a skip")
	}
	if !strings.HasPrefix(got.reason, ReasonUnchanged) {
		t.Errorf("reason %q does not start with the contracted prefix %q", got.reason, ReasonUnchanged)
	}
	// And the date is there, because an operator wants to know which copy they still have.
	if !strings.Contains(got.reason, "2026-09-01") {
		t.Errorf("reason %q does not name the copy it is relying on", got.reason)
	}
}
