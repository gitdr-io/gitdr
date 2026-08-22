package pipeline

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	// PublicKey is optional. When set, restore locates the signed run-manifest for the
	// requested date, verifies its signature, and checks every artifact it downloads
	// against the checksums the manifest records. The unsigned .sha256 sidecar catches
	// corruption but not tampering; the manifest catches both. Without a key restore
	// keeps the sidecar-only check and says so in RestoreResult.Verification.
	PublicKey ed25519.PublicKey
	Logger    *slog.Logger
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
	// Verification says in plain words which integrity checks this restore ran, so a
	// restore that was not checked against the signed manifest announces itself
	// instead of looking identical to one that was. Deliberately kept out of the JSON:
	// the --output json shape is a versioned public contract (SPEC §11), and widening
	// it is its own change, made on purpose, not as a side effect of a read-side fix.
	Verification string `json:"-"`
}

// Restore fetches a bundle, verifies its checksum against the stored sidecar (and,
// when a public key is configured, against the signed run-manifest), checks the
// bundle, and clones it into OutDir. Read-only against the destination.
func Restore(ctx context.Context, d RestoreDeps, req RestoreRequest) (*RestoreResult, error) {
	log := orDefault(d.Logger)
	prefix := path.Join(req.Host, req.Owner, req.Name, req.Date)
	bundleKey := path.Join(prefix, req.Name+".bundle")
	shaKey := path.Join(prefix, req.Name+".sha256")
	lfsKey := path.Join(prefix, req.Name+".lfs.tar")

	// With a public key the signed manifest is located and its signature verified
	// before a single artifact byte is trusted. Failing to find or verify one is a
	// failure, not a downgrade to the sidecar-only check.
	var checks *restoreChecks
	if d.PublicKey != nil {
		var err error
		checks, err = findRestoreChecks(ctx, d.Dest, d.PublicKey, log, req, bundleKey, lfsKey)
		if err != nil {
			return nil, err
		}
		log.Info("manifest verified", "manifest", checks.manifestKey)
	}

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
	// The sidecar is unsigned, so the check above catches corruption but not a rewrite
	// that updated the bundle and the sidecar together. The manifest checksum is under
	// the signature and closes that. Both cover the stored object, ciphertext when
	// encrypted, exactly as backup recorded them.
	if checks != nil && !strings.EqualFold(checks.bundleSHA, gotSHA) {
		return nil, fmt.Errorf("bundle %s does not match the signed manifest %s: manifest records sha256 %s, the stored object has %s",
			bundleKey, checks.manifestKey, checks.bundleSHA, gotSHA)
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
	objs, listErr := d.Dest.List(ctx, lfsKey)
	lfsStored := listErr == nil && len(objs) > 0
	if checks != nil {
		// The signed manifest and the destination must agree on whether this restore
		// carries LFS data. A recorded tar that is missing would hand back an
		// incomplete repository; a stored tar the manifest never recorded is not ours
		// to extract.
		if checks.lfsSHA != "" && !lfsStored {
			if listErr != nil {
				return nil, fmt.Errorf("the signed manifest records %s but listing it failed: %w", lfsKey, listErr)
			}
			return nil, fmt.Errorf("the signed manifest records %s but the destination does not have it", lfsKey)
		}
		if checks.lfsSHA == "" && lfsStored {
			return nil, fmt.Errorf("%s exists in the destination but the signed manifest does not record it; refusing to extract it", lfsKey)
		}
	}
	if lfsStored {
		storedLfs := filepath.Join(tmp, req.Name+".lfs.tar.stored")
		if err := downloadToFile(ctx, d.Dest, lfsKey, storedLfs); err != nil {
			return nil, err
		}
		// Checked before anything reads the data, the same order the bundle gets:
		// checksum first, so a tampered archive is refused before it is extracted.
		if checks != nil {
			gotLfs, _, err := crypto.SHA256File(storedLfs)
			if err != nil {
				return nil, err
			}
			if !strings.EqualFold(checks.lfsSHA, gotLfs) {
				return nil, fmt.Errorf("lfs tar %s does not match the signed manifest %s: manifest records sha256 %s, the stored object has %s",
					lfsKey, checks.manifestKey, checks.lfsSHA, gotLfs)
			}
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

	res := &RestoreResult{BundleKey: bundleKey, SHA256: gotSHA, OutDir: req.OutDir, Verified: true}
	// Never claim more verification than happened. The restore that could not check
	// against the signed manifest says so, in the result and in the log.
	switch {
	case checks != nil && lfsStored:
		res.Verification = "bundle and lfs tar verified against the signed manifest"
	case checks != nil:
		res.Verification = "bundle verified against the signed manifest"
	case lfsStored:
		res.Verification = "bundle checked against its unsigned sha256 sidecar, lfs tar not checked; no public key configured, so the signed manifest was not used"
	default:
		res.Verification = "bundle checked against its unsigned sha256 sidecar; no public key configured, so the signed manifest was not used"
	}
	if checks == nil {
		log.Warn("restore was not verified against the signed manifest; set manifest.publicKeyPath to verify restores",
			"checked", res.Verification)
	}
	log.Info("restored", "bundle", bundleKey, "out", req.OutDir, "verification", res.Verification)
	return res, nil
}

// restoreChecks are the checksums the signed manifest records for one restore.
type restoreChecks struct {
	manifestKey string
	bundleSHA   string
	lfsSHA      string // empty when the manifest records no LFS tar for this repo
}

// findRestoreChecks locates the signed manifest covering this restore and returns the
// checksums it records. Manifest objects are named with a compact UTC timestamp
// (20260613T120000Z.manifest.json), so the requested YYYY-MM-DD date with the dashes
// dropped is their prefix.
//
// Several manifests can match one date: a resume run writes a second manifest that
// records this repo as skipped with no artifacts, and a run over a different repo
// selection does not mention it at all. Artifact keys are create-only, though, so
// exactly one run wrote this bundle and only that run's manifest records the artifact.
// Scanning newest to oldest for the first verified manifest that records the bundle
// finds that run; the order only makes the common case, restoring a recent backup,
// cheap.
//
// A manifest that cannot be verified is skipped with a loud warning rather than failing
// the restore outright: the configured key may have been rotated since an older run,
// and a bucket shared by several backup jobs holds manifests signed by other keys.
// Skipping trusts nothing, no byte of an unverified manifest is used, and if no
// verified manifest records the bundle the restore fails below, naming how many could
// not be verified.
func findRestoreChecks(ctx context.Context, d dest.Destination, pub ed25519.PublicKey, log *slog.Logger, req RestoreRequest, bundleKey, lfsKey string) (*restoreChecks, error) {
	dir := path.Join(req.Host, req.Owner, "manifests")
	prefix := path.Join(dir, strings.ReplaceAll(req.Date, "-", ""))
	objs, err := d.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list manifests under %s: %w", prefix, err)
	}
	var keys []string
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".manifest.json") {
			keys = append(keys, o.Key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no signed manifest for %s under %s/: either the date is wrong or that backup wrote no manifest; to restore without manifest verification, unset manifest.publicKeyPath", req.Date, dir)
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(b, a) }) // newest first

	unverified := 0
	for _, key := range keys {
		m, err := verifiedManifest(ctx, d, pub, key)
		if err != nil {
			unverified++
			log.Warn("manifest cannot be verified with the configured public key; skipping it", "manifest", key, "err", err)
			continue
		}
		for _, repo := range m.Repos {
			checks := restoreChecks{manifestKey: key}
			for _, a := range repo.Artifacts {
				switch a.Key {
				case bundleKey:
					checks.bundleSHA = a.SHA256
				case lfsKey:
					checks.lfsSHA = a.SHA256
				}
			}
			if checks.bundleSHA != "" {
				return &checks, nil
			}
		}
	}
	if unverified > 0 {
		return nil, fmt.Errorf("no verified manifest for %s records %s: %d of %d manifest(s) did not verify with the configured public key; if the signing key was rotated, point manifest.publicKeyPath at the key that signed this backup", req.Date, bundleKey, unverified, len(keys))
	}
	return nil, fmt.Errorf("%d manifest(s) for %s verify but none records %s: the run that wrote this bundle left no manifest here", len(keys), req.Date, bundleKey)
}

// verifiedManifest fetches a manifest and its detached signature and parses it only
// after the signature verifies over the exact stored bytes.
func verifiedManifest(ctx context.Context, d dest.Destination, pub ed25519.PublicKey, key string) (*Manifest, error) {
	canon, err := getBytes(ctx, d, key)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	sigB64, err := getBytes(ctx, d, key+".sig")
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	if err := crypto.Verify(pub, canon, sig); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(canon, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
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
