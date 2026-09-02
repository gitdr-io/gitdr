package pipeline

import (
	"fmt"
	"sort"
	"time"
)

// Deciding whether a repository needs copying again.
//
// `git bundle create --all` writes the entire history every time, and the only thing that
// used to stop a second write was a check on the date. So a repository that had not changed
// in two years was copied in full every night, and on WORM storage the customer could not
// delete a single one of those copies: object lock in compliance mode holds against everyone,
// including the root of the account that owns the bucket. Measured across three real
// organisations, roughly seventy per cent of an organisation does not change on a given day.
//
// The comparison is `git ls-remote` against the ref map the last successful copy recorded.
// One round trip, no objects transferred. If the refs have not moved there is nothing new to
// write, and a byte-identical bundle is waste at both ends.
//
// **The safety bound is the part that matters.** Skipping is only correct while the copy
// being relied on still exists. Object lock protects it until its retain-until date and not
// one second longer, so a repository that never changes would be skipped until its last copy
// aged out of retention and then have no copy at all — turning a saving into data loss. So a
// copy is refreshed well before it expires, however unchanged the repository is.

// unchangedDecision is why a repository was or was not skipped, in a form a test can assert
// and a log line can print.
type unchangedDecision struct {
	skip bool
	// Why, in the words that reach the manifest and the operator.
	reason string
}

// refreshFloor is the fraction of the retention period after which a copy is rewritten even
// though nothing changed.
//
// A third, not a half. Two thirds of the retention is left as headroom for a run that fails
// and is not noticed for a while: at a half, a single missed window puts the only copy inside
// its final quarter, and this decision is not the place to be clever about how attentive
// somebody is.
const refreshFloor = 3

// maxSkipDays caps the above regardless of how long the retention is.
//
// A ten-year retention would otherwise allow a three-year-old copy to be the only one, which
// is technically within policy and is not what anybody means by a backup. Thirty days is the
// longest anyone would want to explain in an incident review.
const maxSkipDays = 30

// decideUnchanged reports whether a repository can be skipped this run.
//
//   - previous is the ref map the last successful copy recorded, nil if there was none.
//   - current is what the source advertises now.
//   - copiedAt is when that last successful copy was made.
//   - retention is how long a copy is kept.
func decideUnchanged(previous, current map[string]string, copiedAt, now time.Time, retention time.Duration) unchangedDecision {
	// No previous copy, or one that recorded nothing. Both mean there is nothing to compare
	// against, and an absent comparison is never a reason to skip.
	//
	// This guard and the next are individually redundant against sameRefs, which returns false
	// whenever the lengths differ. They are load-bearing in exactly one case: when *both* sides
	// are empty, where sameRefs answers true because no length differs and there is nothing to
	// iterate. Either guard alone closes it and both are kept, because the failure they prevent
	// is a repository skipped forever on the strength of two absences agreeing with each other.
	// Established by deleting them one at a time and watching nothing fail, then both.
	if len(previous) == 0 {
		return unchangedDecision{skip: false}
	}
	// A source that advertises nothing is not "unchanged", it is unreadable. Skipping on it
	// would mean an authentication failure quietly stopped the backups of every repository at
	// once while every run reported success.
	if len(current) == 0 {
		return unchangedDecision{skip: false}
	}
	if !sameRefs(previous, current) {
		return unchangedDecision{skip: false}
	}

	// Unchanged, and now the only question is whether the copy being relied on will still be
	// there. This is the difference between saving work and losing data.
	age := now.Sub(copiedAt)
	limit := retention / refreshFloor
	if max := time.Duration(maxSkipDays) * 24 * time.Hour; limit > max || limit <= 0 {
		limit = max
	}
	if age >= limit {
		return unchangedDecision{
			skip:   false,
			reason: fmt.Sprintf("unchanged, but the last copy is %d days old and is being refreshed", int(age.Hours()/24)),
		}
	}

	return unchangedDecision{
		skip:   true,
		reason: fmt.Sprintf("%s %s", ReasonUnchanged, copiedAt.UTC().Format("2006-01-02")),
	}
}

// sameRefs compares two ref maps exactly: same names, same objects, no extras on either side.
//
// Exact and not "every previous ref is still present at the same commit". A deleted branch is
// a change, and a repository whose history was rewritten to drop a branch is precisely the
// case a backup exists for.
func sameRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, oid := range a {
		if b[name] != oid {
			return false
		}
	}
	return true
}

// refsToEntries converts a ref map into the sorted slice the manifest stores.
//
// Sorted because Canonical() signs the marshalled bytes and Go map iteration is random: an
// unsorted slice would give the same run a different signature on every attempt.
func refsToEntries(refs map[string]string) []RefEntry {
	out := make([]RefEntry, 0, len(refs))
	for name, oid := range refs {
		out = append(out, RefEntry{Name: name, Commit: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// entriesToRefs is the inverse, for reading a previous manifest.
func entriesToRefs(entries []RefEntry) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Name] = e.Commit
	}
	return out
}
