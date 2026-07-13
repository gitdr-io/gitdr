package pipeline

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHostileTar builds a tar carrying exactly the entries given, bypassing writeTarFile,
// because the point is to feed extractTarFile an archive it would never have produced.
func writeHostileTar(t *testing.T, path string, entries []tar.Header) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for i := range entries {
		if err := tw.WriteHeader(&entries[i]); err != nil {
			t.Fatal(err)
		}
		if entries[i].Typeflag == tar.TypeReg && entries[i].Size > 0 {
			if _, err := tw.Write([]byte(strings.Repeat("x", int(entries[i].Size)))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The archive is fetched from the destination, which the threat model treats as hostile.
// Every one of these must be refused rather than written.
func TestExtractTarFileRefusesEscapes(t *testing.T) {
	tests := []struct {
		name    string
		entries []tar.Header
	}{
		{
			name:    "parent traversal",
			entries: []tar.Header{{Name: "../escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}},
		},
		{
			name:    "deep parent traversal",
			entries: []tar.Header{{Name: "a/b/../../../escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}},
		},
		{
			name:    "absolute path",
			entries: []tar.Header{{Name: "/etc/escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}},
		},
		{
			// The lexical prefix check cannot see this one: "link" sits inside destDir, so
			// the entry passes, and a later write through it lands wherever it points.
			name: "symlink out of the destination",
			entries: []tar.Header{
				{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0o777},
				{Name: "link/escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
			},
		},
		{
			name: "hard link out of the destination",
			entries: []tar.Header{
				{Name: "link", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o644},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "hostile.tar")
			dest := filepath.Join(dir, "out")
			writeHostileTar(t, src, tc.entries)

			err := extractTarFile(src, dest)
			if err == nil {
				t.Fatal("extractTarFile accepted a hostile archive, want an error")
			}

			// Nothing may have been written outside the destination.
			for _, stray := range []string{
				filepath.Join(dir, "escaped"),
				filepath.Join(dir, "link"),
			} {
				if _, statErr := os.Lstat(stray); statErr == nil {
					t.Fatalf("entry escaped the destination: %s", stray)
				}
			}
		})
	}
}

// The entry names here are innocent. The trap is a symlink already sitting in the
// destination, which restore unpacks into an operator-supplied directory. A check on the
// entry name cannot see it, so the write itself has to be anchored.
func TestExtractTarFileRefusesPrePlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "dest")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(filepath.Join(dest, ".git", "lfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	lfs := filepath.Join(dest, ".git", "lfs")
	if err := os.Symlink(outside, filepath.Join(lfs, "objects")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	src := filepath.Join(dir, "archive.tar")
	writeHostileTar(t, src, []tar.Header{
		{Name: "objects/aa/payload", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
	})

	if err := extractTarFile(src, lfs); err == nil {
		t.Fatal("extractTarFile wrote through a pre-planted symlink, want an error")
	}
	if _, err := os.Stat(filepath.Join(outside, "aa", "payload")); err == nil {
		t.Fatal("the write landed outside the destination")
	}
}

// The ordinary path still has to work: a directory and a regular file round-trip.
func TestWriteAndExtractTarFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("payload")
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "object"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "out.tar")
	if err := writeTarFile(srcDir, archive); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(dir, "dest")
	if err := extractTarFile(archive, destDir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "nested", "object"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip content = %q, want %q", got, want)
	}
}

// A symlink in the source must not make it into the archive, so it can never come back out.
func TestWriteTarFileSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(srcDir, "sneaky")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	archive := filepath.Join(dir, "out.tar")
	if err := writeTarFile(srcDir, archive); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			t.Fatalf("archive carries a %q entry (%s), want only files and dirs", hdr.Typeflag, hdr.Name)
		}
	}
}
