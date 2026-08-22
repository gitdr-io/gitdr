package pipeline

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzExtractTar feeds extractTarFile arbitrary bytes, the position of an attacker who
// rewrote the LFS artifact in the bucket. Escapes are made observable: a canary file
// sits next to the destination, so a traversal that slipped past both layers would
// overwrite it or land a sibling, and the walk refuses any non-file entry that a later
// write could follow out. Absolute-path escapes are unobservable from here; those rest
// on tarEntryPath plus os.Root, which TestExtractTarFileRefusesEscapes pins.
func FuzzExtractTar(f *testing.F) {
	srcDir := f.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "objects", "aa"), 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "objects", "aa", "obj"), []byte("lfs object"), 0o644); err != nil {
		f.Fatal(err)
	}
	genuine := filepath.Join(f.TempDir(), "genuine.tar")
	if err := writeTarFile(srcDir, genuine); err != nil {
		f.Fatal(err)
	}
	genuineBytes, err := os.ReadFile(genuine)
	if err != nil {
		f.Fatal(err)
	}

	hostile := func(entries ...tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for i := range entries {
			if err := tw.WriteHeader(&entries[i]); err != nil {
				f.Fatal(err)
			}
			if entries[i].Typeflag == tar.TypeReg && entries[i].Size > 0 {
				if _, err := tw.Write(bytes.Repeat([]byte{'x'}, int(entries[i].Size))); err != nil {
					f.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			f.Fatal(err)
		}
		return buf.Bytes()
	}

	f.Add(genuineBytes)
	f.Add(hostile(tar.Header{Name: "../canary.txt", Typeflag: tar.TypeReg, Size: 6, Mode: 0o644}))
	f.Add(hostile(tar.Header{Name: "/etc/escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}))
	f.Add(hostile(
		tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../..", Mode: 0o777},
		tar.Header{Name: "link/escaped", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
	))
	f.Add(hostile(tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o644}))
	f.Add(hostile(tar.Header{Name: "a/../../x", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}))
	f.Add(hostile(tar.Header{Name: strings.Repeat("d/", 40) + "deep", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}))
	f.Add(hostile(tar.Header{Name: strings.Repeat("n", 200), Typeflag: tar.TypeReg, Size: 1, Mode: 0o644, Format: tar.FormatPAX}))
	f.Add(genuineBytes[:1600]) // truncated mid-archive: the header claims data the stream no longer has
	f.Add([]byte{})
	f.Add([]byte("not a tar"))
	f.Add(make([]byte, 1024)) // two zero blocks: a valid, empty archive

	f.Fuzz(func(t *testing.T, data []byte) {
		// Every os.Root operation re-resolves its whole path, so extraction cost grows
		// superlinearly with entry depth: a 6 KiB archive naming a 1600-deep path costs
		// ~16s to extract and clean up, which reads as a hung worker. Depth adds no new
		// containment behavior past the first few components, so bound it (slash count
		// over the raw bytes over-approximates total path components) and the size.
		if len(data) > 4096 || bytes.Count(data, []byte{'/'}) > 64 {
			return
		}
		dir := t.TempDir()
		src := filepath.Join(dir, "in.tar")
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(dir, "out")
		dest := filepath.Join(parent, "dest")
		canary := filepath.Join(parent, "canary.txt")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canary, []byte("intact"), 0o644); err != nil {
			t.Fatal(err)
		}

		_ = extractTarFile(src, dest) // hostile input may error; it must not panic

		got, err := os.ReadFile(canary)
		if err != nil || string(got) != "intact" {
			t.Fatalf("canary outside the destination was touched: %q, %v", got, err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "dest" && e.Name() != "canary.txt" {
				t.Fatalf("entry escaped the destination: %s", e.Name())
			}
		}
		walkErr := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				t.Fatalf("extracted a %v entry: %s", d.Type(), p)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
	})
}
