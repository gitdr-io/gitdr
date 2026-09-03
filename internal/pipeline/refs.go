package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gitdr.io/gitdr/internal/gitexec"
)

// This file answers the question a backup tool is actually bought to answer: did the restore
// reproduce the original history, or does a copy merely exist?
//
// Git is content-addressed, so a commit id transitively covers its tree, its blobs and its
// whole ancestry. The bundle's own header declares a ref-to-object map, and the signed
// manifest fixes the bytes of the bundle that header came from. Comparing that map against
// the refs of the restored repository is therefore not a spot check; equality of the ids is
// equality of the history. Nothing here needs to read a commit to know that.
//
// The normalisation below is the whole difficulty, and every rule in it was established by
// running the real commands rather than by reasoning about what git ought to do.

// RefComparison reports a restored repository against the ref map its bundle declares.
//
// Declared counts every line of the bundle header, HEAD included, because every one of them
// is checked. Matched counts those the restore accounts for at the same object. The two are
// equal on a healthy restore of a repository that holds only branches and tags; where they
// differ, Unreferenced and Mismatches say exactly why, so nobody can read a full score off a
// partial one.
type RefComparison struct {
	Declared int
	Matched  int
	// Unreferenced lists refs the bundle declares that `git clone` does not create.
	//
	// A clone uses the default refspec, +refs/heads/*:refs/remotes/origin/* plus tags, so a
	// bundle's refs/notes/*, refs/merge-requests/*, refs/pull/* and refs/keep-around/*
	// entries arrive as objects and get no ref. Verified: a mirror carrying notes, a merge
	// request ref and a keep-around ref bundles all five refs, and the clone from that
	// bundle has three, while `count-objects -v` shows the same six objects on both sides.
	// The data is there; nothing points at it, so a future gc can drop it.
	//
	// Not a failure, because the cause is `git clone`'s refspec and not the backup: it is
	// the same for a perfect bundle and a damaged one, so failing on it would fail every
	// restore of every GitLab project, and a check that fires on the happy path gets turned
	// off. Not silent either, because these refs genuinely are not in the restored
	// repository: they are held out of Matched and named in the summary.
	Unreferenced []string
	// Mismatches are the failures, in ref-name order, so the first one named is stable.
	Mismatches []RefMismatch
}

// RefMismatch is one ref the restored repository does not account for.
type RefMismatch struct {
	Ref  string // the ref as the declaring side names it
	Want string // the object the declaring side records
	Got  string // what the restore has; empty when the ref is absent altogether
}

// OK reports whether the restored repository carries every ref the bundle declares that a
// clone can carry, each at the declared object.
func (c RefComparison) OK() bool { return len(c.Mismatches) == 0 }

// Describe names which side declared the object, because two different comparisons produce
// these and only one of them is against the bundle.
//
// `CompareSourceRefs` fills the declared side from the manifest's record of what the source
// advertised, so a mismatch from it printed as "bundle declares X" said the bundle declares a
// ref whose absence from the bundle is the entire finding. That went into a signed report an
// auditor reads, stating the result backwards.
//
// `declaredBy` is the whole clause, not a noun, because the two sides do different things: a
// bundle declares, a source advertised.
func (m RefMismatch) Describe(declaredBy string) string {
	if m.Got == "" {
		return fmt.Sprintf("%s (%s %s, the restored repository has no such ref)", m.Ref, declaredBy, m.Want)
	}
	return fmt.Sprintf("%s (%s %s, the restored repository has %s)", m.Ref, declaredBy, m.Want, m.Got)
}

// String is the bundle comparison, which is what every caller that does not say otherwise
// means, and what `restore` prints.
func (m RefMismatch) String() string { return m.Describe("bundle declares") }

// Summary describes the comparison in the words a restore prints.
//
// signedBundle says whether the bundle these refs came from was itself verified against a
// signed manifest. It changes the wording rather than the check: without a key the
// comparison still runs and still proves the restore matches the bundle, but the bundle is
// then an unauthenticated document, and a sentence that did not say so would let an
// unverified restore read exactly like a verified one.
func (c RefComparison) Summary(signedBundle bool) string {
	which := "the bundle"
	if signedBundle {
		which = "the signed bundle"
	}
	var b strings.Builder
	if c.Matched == c.Declared {
		fmt.Fprintf(&b, "all %d refs %s declares are present in the restored repository at the same commits", c.Declared, which)
	} else {
		fmt.Fprintf(&b, "%d of %d refs %s declares are present in the restored repository at the same commits", c.Matched, c.Declared, which)
	}
	if n := len(c.Unreferenced); n > 0 {
		fmt.Fprintf(&b, "; %d (%s) %s in the bundle but git clone creates no ref for %s, so those objects are restored with nothing pointing at them",
			n, strings.Join(firstN(c.Unreferenced, 3), ", "), plural(n, "is", "are"), plural(n, "it", "them"))
	}
	if !signedBundle {
		b.WriteString(", and the bundle itself was not verified against a signed manifest")
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("and %d more", len(s)-n))
}

// CompareRestoredRefs reads the ref map the bundle declares and the refs the restored
// repository holds, and reports whether they agree.
func CompareRestoredRefs(ctx context.Context, g *gitexec.Git, bundlePath, repoDir string) (RefComparison, error) {
	declared, err := g.BundleHeads(ctx, bundlePath)
	if err != nil {
		return RefComparison{}, err
	}
	// A bundle that declares nothing would score 0 of 0 and pass, which is a check that
	// cannot fail. It also cannot legitimately happen: `git bundle create` refuses to write
	// an empty bundle, and gitdr skips a repository with no refs before it ever bundles one.
	// So an empty header means the file is not the bundle it was taken for, and the restore
	// must stop rather than report a vacuous success.
	if len(declared) == 0 {
		return RefComparison{}, fmt.Errorf("bundle declares no refs, so there is nothing to compare the restored repository against")
	}
	restored, err := g.ListRefs(ctx, repoDir)
	if err != nil {
		return RefComparison{}, err
	}
	// Resolved separately: for-each-ref does not list HEAD, and the bundle declares it.
	// An error here is not fatal on its own — it becomes a mismatch below if the bundle
	// declares HEAD, and is irrelevant if it does not.
	head, headErr := g.HeadOID(ctx, repoDir)
	if headErr != nil {
		head = ""
	}
	return compareRefs(declared, restored, head), nil
}

// compareRefs is the normalisation, kept free of git so every rule in it is testable.
//
// Rules, each one established by running the commands:
//
//   - refs/heads/X may land in the restore as refs/heads/X or as refs/remotes/origin/X. A
//     clone from a bundle puts every branch under refs/remotes/origin/* and materialises
//     only the checked-out one under refs/heads/*, so requiring refs/heads/X would fail on
//     every branch but one. Worse, a bundle taken from a mirror whose HEAD was detached
//     produces a clone with no refs/heads/* at all.
//   - refs/tags/X compares directly. Tags survive a clone under their own names, and the
//     object ids match without peeling: an annotated tag is recorded in the bundle header as
//     its tag object and read back by %(objectname) as the same tag object.
//   - HEAD is not a ref any repository stores; comparing it by name would always be missing.
//     It is checked instead against what the restored repository's HEAD resolves to, which
//     is what proves the restore checked out the history the bundle says it should. Verified
//     against a detached-HEAD source and a tag-only repository, where HEAD is the only thing
//     tying the restore to the right commit.
//   - Any other namespace is Unreferenced when absent, for the reason given on that field.
//     When present it must still match: if a restore ever does materialise these, a wrong
//     object there is a failure, and this check upgrades itself the day that changes.
//
// A ref in the restore that the bundle does not declare is expected, not a failure. `git
// clone` manufactures the whole refs/remotes/origin/* namespace and refs/remotes/origin/HEAD
// on its own; none of it comes from the bundle. The direction that catches lost history is
// bundle-to-restore, and only that direction is a proof: every declared ref accounted for
// means the history is all there. The reverse direction can only report git doing its job,
// and treating it as a failure would make a correct restore exit non-zero.
func compareRefs(declared []gitexec.BundleRef, restored map[string]string, restoredHead string) RefComparison {
	c := RefComparison{Declared: len(declared)}

	// Sorted so "the first ref that differs" is the same ref on every run and in every
	// message. `git bundle list-heads` prints in header order, which is not a promise.
	sorted := append([]gitexec.BundleRef{}, declared...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, d := range sorted {
		if d.Name == headRef {
			switch {
			case restoredHead == "":
				c.Mismatches = append(c.Mismatches, RefMismatch{Ref: d.Name, Want: d.OID})
			case strings.EqualFold(restoredHead, d.OID):
				c.Matched++
			default:
				c.Mismatches = append(c.Mismatches, RefMismatch{Ref: d.Name, Want: d.OID, Got: restoredHead})
			}
			continue
		}

		// The places this ref is allowed to have landed. Presence at any of them is
		// presence: the restored repository is one we just created from this bundle, so a
		// branch under its remote-tracking name is the same history under the name a clone
		// gives it.
		var found string
		var matched bool
		for _, cand := range candidates(d.Name) {
			got, ok := restored[cand]
			if !ok {
				continue
			}
			if found == "" {
				found = got
			}
			if strings.EqualFold(got, d.OID) {
				matched = true
				break
			}
		}
		switch {
		case matched:
			c.Matched++
		case found != "":
			c.Mismatches = append(c.Mismatches, RefMismatch{Ref: d.Name, Want: d.OID, Got: found})
		case clonable(d.Name):
			c.Mismatches = append(c.Mismatches, RefMismatch{Ref: d.Name, Want: d.OID})
		default:
			c.Unreferenced = append(c.Unreferenced, d.Name)
		}
	}
	return c
}

const (
	headRef    = "HEAD"
	headsPfx   = "refs/heads/"
	tagsPfx    = "refs/tags/"
	remotesPfx = "refs/remotes/origin/"
)

// candidates lists the ref names a declared ref may legitimately appear under in a clone.
func candidates(name string) []string {
	if branch, ok := strings.CutPrefix(name, headsPfx); ok {
		return []string{name, remotesPfx + branch}
	}
	return []string{name}
}

// clonable reports whether `git clone`'s default refspec creates this ref, and so whether
// its absence from the restore is a failure rather than a fact about clone.
func clonable(name string) bool {
	return strings.HasPrefix(name, headsPfx) || strings.HasPrefix(name, tagsPfx)
}

// CompareSourceRefs answers the second question a drill asks: does the restored repository
// carry the refs the *source* advertised when the copy was made?
//
// `CompareRestoredRefs` proves the restored repository reproduces what the bundle declares.
// That is a closed loop — the artifact is consistent with itself — and a bundle written from a
// half-fetched mirror would pass it perfectly while missing branches the source had. Nothing
// could check the other half until the manifest started recording the source's own ref map.
//
// The same normalisation as the bundle comparison, and for the same reasons: `git clone` writes
// a branch as refs/remotes/origin/<name>, and refs a clone's refspec does not create are
// counted apart rather than folded into the matched total.
//
// One direction only. A restored repository holding a ref the source did not advertise is not
// missing history — `ls-remote` and a mirror clone can legitimately differ on hidden refs — but
// a source ref that is not there is exactly the loss a backup exists to prevent.
func CompareSourceRefs(ctx context.Context, g *gitexec.Git, sourceRefs map[string]string, repoDir string) (RefComparison, error) {
	if len(sourceRefs) == 0 {
		// The same refusal CompareRestoredRefs makes on an empty bundle header. Comparing
		// against nothing scores 0 of 0 and passes, which is a check that cannot fail, and a
		// drill exists to produce evidence rather than the appearance of it.
		return RefComparison{}, fmt.Errorf("no source refs recorded, so there is nothing to compare the restored repository against")
	}

	declared := make([]gitexec.BundleRef, 0, len(sourceRefs))
	for name, oid := range sourceRefs {
		declared = append(declared, gitexec.BundleRef{Name: name, OID: oid})
	}

	restored, err := g.ListRefs(ctx, repoDir)
	if err != nil {
		return RefComparison{}, err
	}
	head, err := g.HeadOID(ctx, repoDir)
	if err != nil {
		head = ""
	}
	return compareRefs(declared, restored, head), nil
}
