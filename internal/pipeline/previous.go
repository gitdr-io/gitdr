package pipeline

import (
	"context"
	"path"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/source"
)

// Reading the recent runs, so this one can tell what has changed since.
//
// One List and a handful of Gets for the whole run, not per repository. Manifests live under
// {host}/{namespace}/manifests/{ts}.manifest.json with a lexicographically sortable timestamp,
// so the newest is the last key, and one object carries the ref map of every repository a run
// covered.

// previousCopy is what the last successful run recorded about one repository.
type previousCopy struct {
	refs     map[string]string
	copiedAt time.Time
}

// maxManifestBytes caps what will be read into memory.
//
// A manifest for a very large organisation is a few megabytes; anything past this is either
// corrupt or is not a manifest, and neither is worth exhausting a worker's memory over. The
// run proceeds without a comparison, which costs a full copy and loses nothing.
const maxManifestBytes = 32 << 20

// maxPreviousManifests is how many of the newest manifests the next run reads, at most.
const maxPreviousManifests = 10

// loadPrevious reads the recent manifests in dir, where this run files its own, and returns, per
// repository slug in selected, what the newest one that mentions it recorded. A nil selected
// keeps every repository.
//
// Newest first, and a repository is decided by the newest manifest that has an entry for it,
// whatever that entry says: a failed one means no copy to rely on and the repository is copied.
// The read stops once every selected repository is decided, after maxPreviousManifests, or at the
// first manifest older than the refresh bound, whose copies would all be refreshed anyway.
//
// It used to read the newest manifest alone. One run over a single repository, filed in the same
// directory as the organisation's runs, then hid every other repository from the next run over
// the organisation, which copied them all in full.
//
// A manifest is picked the way a drill picks one: filed directly in dir, named like a manifest,
// and not named later than now. The loader then refuses one that is not named for its own
// finishedAt, or is not a manifest at all. That is bookkeeping this run cannot trust, so it is
// warned about and passed over, like a manifest that could not be read, and the read goes on to
// the next. Passing over one trusts nothing in it.
//
// No failure here is an error. Not being able to read the recent manifests means this run cannot
// tell what changed, and the correct response to not knowing is to copy everything — which is
// exactly what gitdr did before any of this existed. A backup that fails because an optimisation
// could not read its own bookkeeping would be a worse product than one that never had the
// optimisation.
func (r *backupRun) loadPrevious(ctx context.Context, dir string, selected map[string]bool) map[string]previousCopy {
	now := r.now()
	objs, err := r.dst.List(ctx, dir+"/")
	if err != nil {
		r.log.Warn("could not list the previous manifests; every repository will be copied", "dir", dir, "err", err)
		return nil
	}

	// The manifest is signed, and this does not check the signature.
	//
	// That is deliberate and it is safe for exactly one reason: the worst an attacker who
	// could forge this file achieves is making gitdr skip a repository, which withholds a new
	// copy and cannot touch, alter or remove any copy that already exists — the destination
	// has no delete and no overwrite. It is a denial of freshness, not of integrity, and it
	// requires write access to a create-only bucket that gitdr itself refuses to overwrite.
	//
	// Verifying here would mean carrying the public half into the backup path and reading a
	// second object per run to get the signature. `gitdr verify` checks it properly, on the
	// path where the answer is load-bearing.
	out := map[string]previousCopy{}
	decided := map[string]bool{}
	read := 0
	for _, key := range filedManifests(objs, dir) {
		finished, err := time.Parse(manifestStamp, strings.TrimSuffix(path.Base(key), manifestSuffix))
		if err != nil || finished.After(now) {
			continue
		}
		if now.Sub(finished) >= r.refreshBound() || read == maxPreviousManifests {
			break
		}
		read++

		// What the skip needs of each entry, and only of the repositories still undecided: the
		// rest of a large manifest is ref maps of repositories this run does not need.
		var entries []RepoEntry
		head, err := readManifestEntries(ctx, r.dst, nil, key, func(e RepoEntry) {
			if decided[e.Slug] || (selected != nil && !selected[e.Slug]) {
				return
			}
			entries = append(entries, RepoEntry{Slug: e.Slug, Status: e.Status, Refs: e.Refs, CopiedAt: e.CopiedAt})
		})
		if err != nil {
			r.log.Warn("passing over a previous manifest this run cannot read or trust", "key", key, "err", err)
			continue
		}
		before := len(out)
		for _, entry := range entries {
			if decided[entry.Slug] {
				continue // a second entry for one repository is not believed over the first
			}
			decided[entry.Slug] = true
			if c, ok := r.previousCopy(key, head, entry); ok {
				out[entry.Slug] = c
			}
		}
		r.log.Debug("read a previous manifest", "key", key, "repos", len(out)-before)
		if selected != nil && len(decided) == len(selected) {
			break
		}
	}
	return out
}

// previousCopy is what one entry of a previous manifest gives a skip, if anything.
func (r *backupRun) previousCopy(key string, head manifestHead, entry RepoEntry) (previousCopy, bool) {
	// A skipped entry counts, and it has to: a skip means the previous copy is still the
	// current one, and its refs were carried forward for exactly this read. Excluding it
	// made every third run a full copy.
	//
	// A failed entry does not. Its refs describe a repository nothing was written for,
	// and trusting them would skip the retry.
	if entry.Status == StatusFailed || len(entry.Refs) == 0 {
		return previousCopy{}, false
	}
	// The age of the copy, not the age of the run that mentioned it. CopiedAt is carried
	// through every skip; falling back to the run's own finish time is only for a v3
	// manifest written before that field existed, where the effect is a copy refreshed
	// sooner than it needed to be. Erring towards writing is the safe direction.
	copiedAt := head.FinishedAt
	if entry.CopiedAt != nil {
		copiedAt = *entry.CopiedAt
	}
	// A copy dated after its own manifest, or in the future, is one no run recorded, and a
	// skip measured from it would never reach the refresh.
	if err := plausibleCopiedAt(copiedAt, head.FinishedAt, r.now()); err != nil {
		r.log.Warn("not relying on the previous manifest's copy of this repository; it will be copied", "key", key, "repo", entry.Slug, "err", err)
		return previousCopy{}, false
	}
	return previousCopy{refs: entriesToRefs(entry.Refs), copiedAt: copiedAt}, true
}

// currentRefs asks the source what it has now.
//
// A failure is not fatal and not a skip: it returns nil, the comparison finds no evidence, and
// the repository is copied in full. That is the same answer as "something changed", and it is
// the right one — a source that will not answer is not a source that has stayed the same. A
// credential that cannot be read is one of those failures, and the clone that follows asks for
// it again and fails the repository if it still cannot.
func (r *backupRun) currentRefs(ctx context.Context, repo source.Repo) map[string]string {
	cloneURL, err := r.src.CloneURL(ctx, repo)
	if err != nil {
		r.log.Debug("could not resolve the clone url; copying in full", "repo", repo.Slug(), "err", err)
		return nil
	}
	auth, err := gitAuthHeader(ctx, r.src)
	if err != nil {
		r.log.Debug("could not get a git credential to list the source's refs; copying in full", "repo", repo.Slug(), "err", err)
		return nil
	}
	refs, err := r.git.LsRemote(ctx, cloneURL, gitexec.Options{AuthHeader: auth})
	if err != nil {
		r.log.Debug("could not list the source's refs; copying in full", "repo", repo.Slug(), "err", err)
		return nil
	}
	return refs
}
