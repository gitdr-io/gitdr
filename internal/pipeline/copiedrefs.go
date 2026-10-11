package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/source"
)

// The refs a copy records, and what the clone and the bundle are held to.
//
// A copy records the refs of the mirror its bundle is written from, read back once the clone is
// done: every ref, and HEAD when it resolves, the shape `git ls-remote` gives. They are the
// advertisement the clone itself received and what the bundle declares, and the next run compares a
// listing of the source with them as they are (unchanged.go).
//
// Up to v0.1.22 a copy recorded the listing that decides whether to copy at all, taken before the
// clone. A busy repository is pushed to between the two most nights once its clone takes hours, and
// the copy then held what the record did not say: a branch moved in between was recorded at its old
// commit, and one deleted in between as present. A drill failed the good copy on the source join,
// and the next day copied the repository again although nothing had moved since the copy.
//
// What that record could show and this one cannot is a clone that received less than the source
// has: a drill compares the restore with the record, and the two would lack the same branch. So the
// clone is held to the listing taken before it, here, before anything of the copy is written, and
// the bundle to the clone.

// cloneHolds checks that the clone holds every branch and tag the source advertised before the
// clone started and still advertises after it. advertised is that first listing, nil when it could
// not be made, and copied is what the clone received.
//
// By name: a branch pushed to while the clone ran holds whichever commit the clone was told of, and
// either was the source's. Only branches and tags, the refs a clone of the bundle gives back and a
// drill fails on (clonable). The other namespaces are the forge's own, a pull request's refs among
// them, made and dropped by the forge rather than pushed, and a drill counts them apart.
//
// The source is asked again only when the clone lacks one, so a copy that holds everything costs no
// second listing. A ref the source no longer advertises was deleted while the clone ran, and the
// copy is of what the clone found. One it still advertises fails the repository, and so does a
// second listing that fails: then nothing shows the clone holds what the source has.
func (r *backupRun) cloneHolds(ctx context.Context, repo source.Repo, cloneURL string, advertised, copied map[string]string) error {
	var lacking []string
	for name := range advertised {
		if _, ok := copied[name]; !ok && clonable(name) {
			lacking = append(lacking, name)
		}
	}
	if len(lacking) == 0 {
		return nil
	}
	sort.Strings(lacking)
	unasked := func(err error) error {
		return fmt.Errorf("the clone lacks %s, which the source advertised before the clone, and the source could not be asked whether it still does: %w",
			strings.Join(firstN(lacking, 3), ", "), err)
	}

	// With a credential asked for immediately before, as for every git command (gitAuthHeader).
	auth, err := gitAuthHeader(ctx, r.src)
	if err != nil {
		return unasked(fmt.Errorf("source auth: %w", err))
	}
	now, err := r.git.LsRemote(ctx, cloneURL, gitexec.Options{AuthHeader: auth})
	if err != nil {
		return unasked(err)
	}

	var still []string
	for _, name := range lacking {
		if _, ok := now[name]; ok {
			still = append(still, name)
		}
	}
	if len(still) > 0 {
		return fmt.Errorf("the clone lacks %s, which the source advertised before the clone and still advertises",
			strings.Join(firstN(still, 3), ", "))
	}
	r.log.Info("refs the source advertised before the clone were deleted while it ran; the copy is of what the clone received",
		"repo", repo.Slug(), "count", len(lacking), "refs", firstN(lacking, 3))
	return nil
}

// bundleHolds checks that the bundle written from the mirror declares the refs the copy records,
// every one at the same object and nothing else, so what the record names is what a restore of the
// bundle gives back.
//
// The bundle is held to the record whatever git was asked to write, because git has not always
// written what it was asked to. In a mirror whose HEAD named a branch that did not exist, `git bundle
// create --all HEAD` took HEAD for the path of the mirror's own HEAD file and left every branch out,
// and git exited 0 with the annotated tags alone in the bundle. Up to v0.1.22 that copy was stored
// as a good one: verify and restore pass such a bundle, which restores to what it declares, and only
// a drill compared it with the source. BundleAll now leaves HEAD out there (SPEC §2), and this is
// what refuses the next surprise of that kind before anything is written.
func (r *backupRun) bundleHolds(ctx context.Context, bundlePath string, copied map[string]string) error {
	heads, err := r.git.BundleHeads(ctx, bundlePath)
	if err != nil {
		return fmt.Errorf("read the bundle's refs: %w", err)
	}
	declared := make(map[string]string, len(heads))
	for _, h := range heads {
		declared[h.Name] = h.OID
	}
	if sameRefs(copied, declared) {
		return nil
	}

	var lacks, moved, extra []string
	for name, oid := range copied {
		got, ok := declared[name]
		switch {
		case !ok:
			lacks = append(lacks, name)
		case got != oid:
			moved = append(moved, name)
		}
	}
	for name := range declared {
		if _, ok := copied[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(lacks)
	sort.Strings(moved)
	sort.Strings(extra)
	var what []string
	if len(lacks) > 0 {
		what = append(what, "it lacks "+strings.Join(firstN(lacks, 3), ", "))
	}
	if len(moved) > 0 {
		what = append(what, "it declares "+strings.Join(firstN(moved, 3), ", ")+" at another object")
	}
	if len(extra) > 0 {
		what = append(what, "it declares "+strings.Join(firstN(extra, 3), ", ")+", which the clone does not hold")
	}
	return fmt.Errorf("the bundle written from the clone does not declare the refs the clone holds: %s", strings.Join(what, "; "))
}
