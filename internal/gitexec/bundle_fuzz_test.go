package gitexec

import (
	"strings"
	"testing"
)

// FuzzParseBundleHeads hands the bundle-header parser arbitrary text.
//
// What it parses is the output of `git bundle list-heads` on a bundle pulled out of object
// storage, and that map is what the restore proof is measured against. The property under
// test is the one the proof rests on: the parser either returns a ref set every entry of
// which is a well-formed name and object id, or it returns an error. It must never quietly
// drop a line, because a ref that is never declared is a ref the comparison can never find
// missing -- the proof would narrow itself to whatever happened to parse and still report a
// full score.
func FuzzParseBundleHeads(f *testing.F) {
	f.Add("aa11 refs/heads/main\nbb22 refs/tags/v1\naa11 HEAD\n")
	f.Add("")
	f.Add("\n\n\n")
	f.Add("aa11 refs/notes/commits\n")
	f.Add("AA11 refs/heads/main\n")                       // uppercase hex
	f.Add("aa11 refs/heads/main")                         // no trailing newline
	f.Add("aa11  refs/heads/main\n")                      // a second space
	f.Add("aa11 refs/heads/main extra\n")                 // trailing junk on the line
	f.Add("aa11\n")                                       // no ref name
	f.Add(" refs/heads/main\n")                           // no object id
	f.Add("zzzz refs/heads/main\n")                       // non-hex id
	f.Add("fatal: not a bundle\n")                        // git shouting instead of answering
	f.Add("aa11 refs/heads/main\r\nbb22 refs/tags/v\r\n") // CRLF
	f.Add("aa11 \u202erefs/heads/main\n")                 // a bidi override in the ref name
	f.Add("aa11 refs/heads/\x00main\n")                   // a NUL in the ref name

	f.Fuzz(func(t *testing.T, out string) {
		if len(out) > 1<<16 {
			return
		}
		refs, err := parseBundleHeads(out)
		if err != nil {
			if refs != nil {
				t.Fatalf("parseBundleHeads returned %d refs alongside an error", len(refs))
			}
			return
		}
		// No line may be silently discarded: every non-blank line must have produced a ref.
		lines := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) != "" {
				lines++
			}
		}
		if lines != len(refs) {
			t.Fatalf("%d non-blank lines produced %d refs; a line was dropped\ninput: %q", lines, len(refs), out)
		}
		for _, r := range refs {
			if r.Name == "" {
				t.Fatalf("empty ref name from %q", out)
			}
			if !isHex(r.OID) {
				t.Fatalf("ref %q accepted a non-hex object id %q", r.Name, r.OID)
			}
			// A name carrying whitespace would mean the split ran off the end of the line
			// and swallowed a following field.
			if strings.ContainsAny(r.Name, " \t\n") {
				t.Fatalf("ref name %q carries whitespace", r.Name)
			}
		}
	})
}
