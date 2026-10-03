package pipeline

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"gitdr.io/gitdr/internal/dest"
)

// What counts as a copy, for a same-day rerun and for a restore by date.
//
// Both ask one question about a date folder, {host}/{owner}/{name}/{date}/: did a run finish a
// copy there, and which one. Up to v0.1.20 they answered it two ways. The rerun took any bundle,
// or any metadata, under the date as a finished copy, so a copy whose checksum or LFS archive
// never landed was skipped as "already backed up" and counted as protected. Restore by date took
// any manifest entry that named the bundle, the entry of a repository that failed among them.
//
// One rule now, and it is restore's search. A copy for date D is recorded when a manifest filed in
// the repository's namespace, in one above it or at the host, and named for D or the day after,
// has an entry for the repository that records the artifact as a copy that run made: a success
// that lists it, or for a repository with no commits, a skip as ReasonEmpty that lists its
// metadata. A failed entry lists what its run wrote, and that is not a copy.

// recordsCopy reports whether e records the artifact at key as part of a copy of slug that its
// run made.
func recordsCopy(e RepoEntry, slug, key string) bool {
	if e.Slug != slug {
		return false
	}
	switch {
	case e.Status == StatusSuccess:
	case e.Status == StatusSkipped && e.Reason == ReasonEmpty:
	default:
		return false
	}
	return namesArtifact(e, key)
}

// namesArtifact reports whether e lists the artifact at key, whatever its status.
func namesArtifact(e RepoEntry, key string) bool {
	for _, a := range e.Artifacts {
		if a.Key == key {
			return true
		}
	}
	return false
}

// copiedAtOf is when the copy an entry relies on was made: its copiedAt, or for an entry that has
// refs and no copiedAt, the finish of the manifest that holds it. Only a manifest from before
// copiedAt was carried through skips has that shape.
func copiedAtOf(e RepoEntry, finished time.Time) *time.Time {
	if e.CopiedAt != nil {
		at := *e.CopiedAt
		return &at
	}
	if len(e.Refs) == 0 {
		return nil
	}
	at := finished
	return &at
}

// recordedCopy is a copy a manifest records, with the manifest that records it.
type recordedCopy struct {
	manifestKey string
	finishedAt  time.Time
	entry       RepoEntry
}

// copySearchReport is what a search passed over, for an error that says why it found nothing.
type copySearchReport struct {
	seen   int // manifests looked at
	passed int // of those, the ones that could not be read or were refused
	// named is the first entry for the repository that lists the artifact without recording it
	// as a copy, a failed one, and namedIn the manifest that holds it.
	named   *RepoEntry
	namedIn string
}

// copySearch finds the manifest that records a copy, and keeps what it read for the next
// question. A restore asks once. A same-day rerun asks for every repository with objects under
// the date, and finds most of them in the same one or two manifests, so each listing and each
// manifest is read once per run.
type copySearch struct {
	dst dest.Destination
	pub ed25519.PublicKey // nil: a manifest's signature is not checked
	log *slog.Logger
	// keep says which repositories' entries are worth keeping, by slug; nil keeps all of them.
	keep func(slug string) bool

	mu       sync.Mutex
	listings map[string][]dest.Object
	loaded   map[string]*searchedManifest
}

// searchedManifest is what a search kept of one manifest.
type searchedManifest struct {
	finishedAt time.Time
	err        error                // why it cannot be relied on; nil when it can
	entries    map[string]RepoEntry // the kept repositories' entries, by slug
}

func newCopySearch(d dest.Destination, pub ed25519.PublicKey, log *slog.Logger, keep func(string) bool) *copySearch {
	return &copySearch{
		dst: d, pub: pub, log: orDefault(log), keep: keep,
		listings: map[string][]dest.Object{}, loaded: map[string]*searchedManifest{},
	}
}

// find returns the copy of slug a manifest records at one of keys, or nil when none does.
//
// It looks where backup files a manifest, in restore's order: dirs, deepest first, and in each the
// manifests named for day and then for the day after, newest first. A run files its manifest under
// the deepest namespace holding every repository it covered and names it for the moment it
// finished, which is the date of the copy or, for a run that crossed midnight UTC, the day after.
// notAfter, when set, leaves out a manifest named later than it, which no run that has finished
// can have written.
//
// A manifest that cannot be read, or that the loader refuses, is passed over with a warning and
// counted. Passing over it trusts nothing.
func (s *copySearch) find(ctx context.Context, dirs []string, day time.Time, slug string, keys []string, notAfter time.Time) (*recordedCopy, copySearchReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var rep copySearchReport
	for _, dir := range dirs {
		for _, on := range []time.Time{day, day.AddDate(0, 0, 1)} {
			prefix := path.Join(dir, on.Format("20060102"))
			objs, err := s.list(ctx, prefix)
			if err != nil {
				return nil, rep, fmt.Errorf("list manifests under %s: %w", prefix, err)
			}
			for _, key := range filedManifests(objs, dir) {
				if !notAfter.IsZero() {
					at, err := time.Parse(manifestStamp, strings.TrimSuffix(path.Base(key), manifestSuffix))
					if err != nil || at.After(notAfter) {
						continue
					}
				}
				rep.seen++
				m := s.load(ctx, key)
				if m.err != nil {
					rep.passed++
					continue
				}
				e, ok := m.entries[slug]
				if !ok {
					continue
				}
				for _, k := range keys {
					if recordsCopy(e, slug, k) {
						return &recordedCopy{manifestKey: key, finishedAt: m.finishedAt, entry: e}, rep, nil
					}
				}
				if rep.named == nil {
					for _, k := range keys {
						if namesArtifact(e, k) {
							named := e
							rep.named, rep.namedIn = &named, key
							break
						}
					}
				}
			}
		}
	}
	return nil, rep, nil
}

// list is the destination's listing of prefix, read once per search. Called with s.mu held.
func (s *copySearch) list(ctx context.Context, prefix string) ([]dest.Object, error) {
	if objs, ok := s.listings[prefix]; ok {
		return objs, nil
	}
	objs, err := s.dst.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	s.listings[prefix] = objs
	return objs, nil
}

// load reads the manifest at key once per search, through the one loader, and keeps the entries
// of the repositories the search keeps. Called with s.mu held.
func (s *copySearch) load(ctx context.Context, key string) *searchedManifest {
	if m, ok := s.loaded[key]; ok {
		return m
	}
	out := &searchedManifest{entries: map[string]RepoEntry{}}
	m, err := loadManifest(ctx, s.dst, s.pub, key)
	if err != nil {
		out.err = err
		s.log.Warn("skipping a manifest that cannot be relied on", "manifest", key, "err", err)
	} else {
		out.finishedAt = m.FinishedAt
		for _, e := range m.Repos {
			if s.keep != nil && !s.keep(e.Slug) {
				continue
			}
			// One entry per repository is what a run writes. A second is not believed over the
			// first.
			if _, dup := out.entries[e.Slug]; !dup {
				out.entries[e.Slug] = e
			}
		}
	}
	s.loaded[key] = out
	return out
}
