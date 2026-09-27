package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The pointer detector every restore runs, without git-lfs.
//
// Each file below is a case the rule decides, and each "not" case is one a looser rule would
// count: a pointer inside .git is git-lfs's own bookkeeping, a symlink is a link whatever it points
// at, and a file of 1024 bytes or more is not a pointer by the spec's own limit, however it starts.
func TestLFSPointersAreFoundByReadingTheTree(t *testing.T) {
	const pointer = "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12345\n"
	const hawser = "version https://hawser.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12345\n"
	padded := func(n int) string { return pointer + strings.Repeat("x", n-len(pointer)) }

	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("big.bin", pointer)
	write("assets/deep/model.bin", pointer)
	write("legacy.psd", hawser) // the pre-release tool's pointer line
	write("edge/1023.bin", padded(1023))
	write("edge/1024.bin", padded(1024)) // not: the spec puts every pointer under 1024 bytes
	write("README.md", "hello\n")
	write("almost.txt", "version https://git-lfs.github.com/spec/v") // not: too short to be the line
	write("indented.txt", " "+pointer)                               // not: does not start with it
	write(".git/lfs/incomplete/stub", pointer)                       // not: inside the root .git
	// Not: a link to a pointer is a link.
	if err := os.Symlink("big.bin", filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}

	got, err := lfsPointers(context.Background(), root)
	if err != nil {
		t.Fatalf("lfsPointers: %v", err)
	}
	want := []string{"assets/deep/model.bin", "big.bin", "edge/1023.bin", "legacy.psd"}
	if !slices.Equal(got, want) {
		t.Errorf("pointers = %q\n       want %q", got, want)
	}

	// An output directory reached through a symlink is read like any other. A walk does not
	// descend into a symlink, so without resolving it this found nothing, which reads as clean.
	link := filepath.Join(t.TempDir(), "out")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if got, err := lfsPointers(context.Background(), link); err != nil || !slices.Equal(got, want) {
		t.Errorf("through a symlinked root: %q, %v; want %q", got, err, want)
	}
}
