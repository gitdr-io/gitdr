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
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // dirs/symlinks: header only
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

// extractTarFile extracts srcFile into destDir, refusing any entry that would escape
// destDir (tar-slip guard).
func extractTarFile(srcFile, destDir string) error {
	f, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	clean := filepath.Clean(destDir)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		if target != clean && !strings.HasPrefix(target, clean+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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
		}
	}
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
