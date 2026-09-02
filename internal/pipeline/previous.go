package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/source"
)

// Reading the last successful run, so this one can tell what has changed since.
//
// One List and one Get for the whole run, not per repository. Manifests live under
// {host}/{org}/manifests/{ts}.manifest.json with a lexicographically sortable timestamp, so
// the newest is the last key, and one small object carries the ref map of every repository the
// previous run copied.

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

// loadPrevious reads the newest manifest under the organisation and returns, per repository
// slug, what it recorded.
//
// Every failure here returns an empty map and no error. Not being able to read the last
// manifest means this run cannot tell what changed, and the correct response to not knowing is
// to copy everything — which is exactly what gitdr did before any of this existed. A backup
// that fails because an optimisation could not read its own bookkeeping would be a worse
// product than one that never had the optimisation.
func (r *backupRun) loadPrevious(ctx context.Context, anchor source.Repo) map[string]previousCopy {
	prefix := path.Join(anchor.Host, anchor.Owner, "manifests") + "/"

	objs, err := r.dst.List(ctx, prefix)
	if err != nil || len(objs) == 0 {
		return nil
	}

	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		// The signature sits beside the manifest under the same prefix.
		if strings.HasSuffix(o.Key, ".manifest.json") {
			keys = append(keys, o.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	// The timestamp is 20060102T150405Z, so lexical order is chronological order.
	sort.Strings(keys)
	newest := keys[len(keys)-1]

	rc, err := r.dst.Get(ctx, newest)
	if err != nil {
		r.log.Debug("could not read the previous manifest; every repository will be copied", "key", newest, "err", err)
		return nil
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes {
		return nil
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
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
	out := make(map[string]previousCopy, len(m.Repos))
	for _, entry := range m.Repos {
		// A skipped entry counts, and it has to: a skip means the previous copy is still the
		// current one, and its refs were carried forward for exactly this read. Excluding it
		// made every third run a full copy.
		//
		// A failed entry does not. Its refs describe a repository nothing was written for,
		// and trusting them would skip the retry.
		if entry.Status == StatusFailed || len(entry.Refs) == 0 {
			continue
		}
		// The age of the copy, not the age of the run that mentioned it. CopiedAt is carried
		// through every skip; falling back to the run's own finish time is only for a v3
		// manifest written before that field existed, where the effect is a copy refreshed
		// sooner than it needed to be. Erring towards writing is the safe direction.
		copiedAt := m.FinishedAt
		if entry.CopiedAt != nil {
			copiedAt = *entry.CopiedAt
		}
		out[entry.Slug] = previousCopy{refs: entriesToRefs(entry.Refs), copiedAt: copiedAt}
	}
	if len(out) > 0 {
		r.log.Debug("read the previous manifest", "key", newest, "repos", len(out))
	}
	return out
}

// currentRefs asks the source what it has now.
//
// A failure is not fatal and not a skip: it returns nil, the comparison finds no evidence, and
// the repository is copied in full. That is the same answer as "something changed", and it is
// the right one — a source that will not answer is not a source that has stayed the same.
func (r *backupRun) currentRefs(ctx context.Context, repo source.Repo, authHeader string) map[string]string {
	cloneURL, err := r.src.CloneURL(ctx, repo)
	if err != nil {
		r.log.Debug("could not resolve the clone url; copying in full", "repo", repo.Slug(), "err", err)
		return nil
	}
	refs, err := r.git.LsRemote(ctx, cloneURL, gitexec.Options{AuthHeader: authHeader})
	if err != nil {
		r.log.Debug("could not list the source's refs; copying in full", "repo", repo.Slug(), "err", err)
		return nil
	}
	return refs
}
