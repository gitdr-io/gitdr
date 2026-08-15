package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
)

// RestoreDeps are the inputs to a restore.
type RestoreDeps struct {
	Dest          dest.Destination
	Git           *gitexec.Git
	EncryptionKey []byte // optional; must match the backup's key
	Logger        *slog.Logger
}

// RestoreRequest selects which dated bundle to restore and where to put it.
type RestoreRequest struct {
	Host   string // e.g. github.com
	Owner  string
	Name   string
	Date   string // YYYY-MM-DD
	OutDir string
}

// RestoreResult reports what was restored.
type RestoreResult struct {
	BundleKey string `json:"bundleKey"`
	SHA256    string `json:"sha256"`
	OutDir    string `json:"outDir"`
	Verified  bool   `json:"verified"`
}

// Restore fetches a bundle, verifies its checksum against the stored sidecar, checks
// the bundle, and clones it into OutDir. Read-only against the destination.
func Restore(ctx context.Context, d RestoreDeps, req RestoreRequest) (*RestoreResult, error) {
	log := orDefault(d.Logger)
	prefix := path.Join(req.Host, req.Owner, req.Name, req.Date)
	bundleKey := path.Join(prefix, req.Name+".bundle")
	shaKey := path.Join(prefix, req.Name+".sha256")

	tmp, err := os.MkdirTemp("", "gitdr-restore-")
	if err != nil {
		return nil, fmt.Errorf("tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// Download the stored object (ciphertext if encrypted) and verify its checksum.
	storedBundle := filepath.Join(tmp, req.Name+".bundle.stored")
	if err := downloadToFile(ctx, d.Dest, bundleKey, storedBundle); err != nil {
		return nil, err
	}
	// A stored artifact beginning with the envelope magic is encrypted; without a key we
	// can neither read the sidecar nor decrypt the bundle. Fail clearly here instead of
	// comparing unreadable ciphertext and surfacing a confusing checksum mismatch.
	if d.EncryptionKey == nil {
		if enc, err := fileIsEncrypted(storedBundle); err != nil {
			return nil, err
		} else if enc {
			return nil, fmt.Errorf("%s is encrypted; set the encryption key (GITDR_ENCRYPTION_KEY) to restore", bundleKey)
		}
	}
	wantSHA, err := readSHASidecar(ctx, d.Dest, shaKey, d.EncryptionKey)
	if err != nil {
		return nil, err
	}
	gotSHA, _, err := crypto.SHA256File(storedBundle)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(wantSHA, gotSHA) {
		return nil, fmt.Errorf("checksum mismatch for %s: want %s, got %s", bundleKey, wantSHA, gotSHA)
	}

	bundlePath := storedBundle
	if d.EncryptionKey != nil {
		bundlePath = filepath.Join(tmp, req.Name+".bundle")
		if err := crypto.DecryptFile(storedBundle, bundlePath, d.EncryptionKey); err != nil {
			return nil, fmt.Errorf("decrypt bundle: %w", err)
		}
	}

	if err := d.Git.BundleVerify(ctx, bundlePath); err != nil {
		return nil, fmt.Errorf("bundle verify: %w", err)
	}
	if err := d.Git.CloneFromBundle(ctx, bundlePath, req.OutDir); err != nil {
		return nil, fmt.Errorf("clone from bundle: %w", err)
	}

	// LFS: if a tar artifact exists for this date, restore the objects and check out.
	lfsKey := path.Join(prefix, req.Name+".lfs.tar")
	if objs, err := d.Dest.List(ctx, lfsKey); err == nil && len(objs) > 0 {
		storedLfs := filepath.Join(tmp, req.Name+".lfs.tar.stored")
		if err := downloadToFile(ctx, d.Dest, lfsKey, storedLfs); err != nil {
			return nil, err
		}
		lfsTar := storedLfs
		if d.EncryptionKey != nil {
			lfsTar = filepath.Join(tmp, req.Name+".lfs.tar")
			if err := crypto.DecryptFile(storedLfs, lfsTar, d.EncryptionKey); err != nil {
				return nil, fmt.Errorf("decrypt lfs: %w", err)
			}
		}
		if err := extractTarFile(lfsTar, filepath.Join(req.OutDir, ".git", "lfs")); err != nil {
			return nil, fmt.Errorf("lfs extract: %w", err)
		}
		if gitexec.LFSAvailable() {
			// The filters first. A clone from a bundle carries no filter.lfs.* config, and
			// without it the checkout below exits 0 having done nothing — which made a
			// successful restore depend on whether this machine had ever run
			// "git lfs install". In a disaster it is a new machine, and it has not.
			if err := d.Git.LFSInstallLocal(ctx, req.OutDir); err != nil {
				return nil, fmt.Errorf("lfs install: %w", err)
			}
			if err := d.Git.LFSCheckout(ctx, req.OutDir); err != nil {
				return nil, fmt.Errorf("lfs checkout: %w", err)
			}

			// Then read the working tree back rather than trusting the exit code, for the
			// same reason the rest of this tool re-reads what it writes. Handing someone a
			// 130-byte pointer where their file should be, and calling it a restore, is the
			// failure this product exists to prevent.
			remaining, err := d.Git.LFSPointersRemaining(ctx, req.OutDir)
			if err != nil {
				return nil, fmt.Errorf("lfs verify: %w", err)
			}
			if len(remaining) > 0 {
				return nil, fmt.Errorf(
					"lfs restore incomplete: %d file(s) are still pointers, first is %q",
					len(remaining), remaining[0])
			}
		} else {
			// Fail closed. The objects were restored but the working tree still holds
			// pointers, so this is a partial restore, and a warning an operator may not read
			// is not enough to let it be reported as success.
			return nil, fmt.Errorf(
				"repository uses git-lfs and git-lfs is not installed: LFS objects were restored to %s but the working tree still holds pointer files",
				req.OutDir)
		}
	}

	log.Info("restored", "bundle", bundleKey, "out", req.OutDir)
	return &RestoreResult{BundleKey: bundleKey, SHA256: gotSHA, OutDir: req.OutDir, Verified: true}, nil
}

// fileIsEncrypted reports whether the file at p begins with the gitdr envelope magic.
// It peeks a few bytes so it need not read a whole (possibly large) bundle.
func fileIsEncrypted(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 8)
	n, err := io.ReadFull(f, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return false, nil // too short to be an envelope
	}
	if err != nil {
		return false, fmt.Errorf("read %q: %w", p, err)
	}
	return crypto.IsEncrypted(buf[:n]), nil
}

func downloadToFile(ctx context.Context, d dest.Destination, key, dstPath string) error {
	rc, err := d.Get(ctx, key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	f, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create %q: %w", dstPath, err)
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr // surface a flush error so the checksum runs on a complete file
	}
	if err != nil {
		return fmt.Errorf("download %q: %w", key, err)
	}
	return nil
}

// readSHASidecar reads a `sha256sum`-format sidecar (decrypting it when a key is given)
// and returns the hex digest of the stored bundle object.
func readSHASidecar(ctx context.Context, d dest.Destination, key string, encKey []byte) (string, error) {
	rc, err := d.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 8192))
	if err != nil {
		return "", fmt.Errorf("read %q: %w", key, err)
	}
	if encKey != nil {
		var buf bytes.Buffer
		if err := crypto.Decrypt(&buf, bytes.NewReader(b), encKey); err != nil {
			return "", fmt.Errorf("decrypt %q: %w", key, err)
		}
		b = buf.Bytes()
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum sidecar %q", key)
	}
	return fields[0], nil
}
