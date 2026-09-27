package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// lfsPointerLimit is the size every git-lfs pointer file is under. The spec: "Pointer files must
// be less than 1024 bytes in size, including any pointer extension lines."
const lfsPointerLimit = 1024

// lfsPointerLines are the lines a git-lfs pointer file starts with: the v1 spec's, and the one
// the pre-release tool wrote, which git-lfs still reads.
var lfsPointerLines = [][]byte{
	[]byte("version https://git-lfs.github.com/spec/v1"),
	[]byte("version https://hawser.github.com/spec/v1"),
}

// lfsPointers returns the files in a restored working tree that are git-lfs pointers instead of
// the content they point at, as slash-separated paths relative to root, in lexical order.
//
// A pointer is a regular file outside the root .git, under 1024 bytes, that starts with a pointer
// line. Nothing here asks git-lfs. The check runs on every restore, on machines without git-lfs
// too, and `git lfs checkout` exits 0 whether or not it replaced anything, so reading the files
// back is the only way to know what is on disk.
//
// A symlink is not counted and not followed. git stores it as a link, and what it points at is
// either another file in the tree, which is read on its own, or outside the restore.
func lfsPointers(ctx context.Context, root string) ([]string, error) {
	// The root itself is resolved, because a walk does not descend into a symlink, and an output
	// directory reached through one would otherwise read as a tree with nothing in it.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("read the restored tree back: %w", err)
	}
	gitDir := filepath.Join(root, ".git")
	var found []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == gitDir {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() >= lfsPointerLimit {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, line := range lfsPointerLines {
			if bytes.HasPrefix(b, line) {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				found = append(found, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the restored tree back: %w", err)
	}
	return found, nil
}
