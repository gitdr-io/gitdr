package pipeline

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// writeTarFile tars srcDir's contents (paths relative to srcDir) into dstFile.
func writeTarFile(srcDir, dstFile string) error {
	f, err := os.Create(dstFile)
	if err != nil {
		return fmt.Errorf("tar create %q: %w", dstFile, err)
	}
	tw := tar.NewWriter(f)
	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Only directories and regular files go in. A symlink would be written with an
		// empty link target and, on the way back out, is the one entry type that can make
		// a later write land outside the destination.
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		_, err = io.Copy(tw, src)
		return err
	})
	closeErr := tw.Close()
	if fErr := f.Close(); closeErr == nil {
		closeErr = fErr
	}
	if walkErr != nil {
		return fmt.Errorf("tar %q: %w", srcDir, walkErr)
	}
	return closeErr
}

// extractTarFile extracts srcFile into destDir. The archive comes back from the
// destination, which the threat model treats as hostile, and restore unpacks it into an
// operator-supplied directory. So containment is enforced by the kernel rather than by
// inspecting entry names: every write goes through an os.Root anchored at destDir, which
// refuses any path resolving outside it, including through a symlink that was already
// sitting in the destination. The name checks below are the second layer, not the only one.
func extractTarFile(srcFile, destDir string) error {
	f, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open destination %q: %w", destDir, err)
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		name, err := tarEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if dir := filepath.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return err
				}
			}
			out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Uncompressed tar (no decompression amplification) from our own
			// WORM-immutable backup; tar.Reader also bounds each entry to its declared
			// size, so no zip/decompression-bomb risk applies here.
			// nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Fail, don't skip. A symlink or hard link entry is the one shape that can
			// turn a later write into an escape, and writeTarFile never emits one, so an
			// archive carrying it is not ours.
			return fmt.Errorf("tar entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// tarEntryPath turns a tar entry name into a path relative to the destination, rejecting
// the shapes writeTarFile never produces. An absolute name would otherwise be re-rooted
// under the destination and restore a file the backup never held.
func tarEntryPath(raw string) (string, error) {
	name := filepath.Clean(filepath.FromSlash(raw))
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("tar entry is absolute: %q", raw)
	}
	if name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry escapes destination: %q", raw)
	}
	return name, nil
}

// dirHasFiles reports whether dir exists and contains at least one regular file.
func dirHasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}
