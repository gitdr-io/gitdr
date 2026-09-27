package pipeline

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
)

// RestoreDeps are the inputs to a restore.
type RestoreDeps struct {
	Dest          dest.Destination
	Git           *gitexec.Git
	EncryptionKey []byte // optional; must match the backup's key
	// PublicKey is optional. When set, restore verifies the signed run-manifest of the run that
	// wrote the bundle, the one RestoreRequest.ManifestKey names or the one it finds for the
	// requested date, and checks every artifact it downloads against the checksums that manifest
	// records. The unsigned .sha256 sidecar catches corruption but not tampering; the manifest
	// catches both. Without a key restore keeps the sidecar-only check and says so in
	// RestoreResult.Verification, and a ManifestKey is refused.
	PublicKey ed25519.PublicKey
	Logger    *slog.Logger
}

// RestoreRequest selects which bundle to restore and where to put it.
type RestoreRequest struct {
	// ManifestKey names the run-manifest to restore from, the key backup prints as manifestKey.
	// Owner and Name then pick the repository in it by slug, exactly, and the host and date come
	// from the bundle key the manifest records, so Host and Date stay empty. It needs
	// RestoreDeps.PublicKey: a manifest is read only once its signature holds.
	//
	// Empty, restore looks for the manifest from Host, Owner and Date; see findRestoreChecks.
	ManifestKey string

	Host   string // e.g. github.com
	Owner  string
	Name   string
	Date   string // YYYY-MM-DD
	OutDir string

	// verified is a signed manifest's word on this restore's artifacts, for a caller that has
	// already verified that manifest with the same public key. Drill sets it: it verifies the
	// manifest it drills before it restores anything, and finding it again cost every
	// repository a listing plus a fetch and a verification of each manifest of that date.
	// Unexported, so only this package can vouch for a manifest.
	verified *restoreChecks
}

// artifactKeys names one repository's dated artifacts: the bundle, its checksum sidecar and
// the LFS archive. Restore and Drill both look them up, and must look in the same place.
func artifactKeys(host, owner, name, date string) (bundle, sidecar, lfs string) {
	prefix := path.Join(host, owner, name, date)
	return path.Join(prefix, name+".bundle"), path.Join(prefix, name+".sha256"), path.Join(prefix, name+".lfs.tar")
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
	// Refs is the proof that the restore reproduced the history the bundle declares:
	// how many refs the bundle's own header carries and how many of them the restored
	// repository has at the same object. Out of the JSON for the same reason as
	// Verification — the shape is a versioned contract, and putting this on it is a
	// separate, deliberate change.
	Refs RefComparison `json:"-"`
}

// Restore fetches a bundle, verifies its checksum against the stored sidecar (and,
// when a public key is configured, against the signed run-manifest), checks the
// bundle, and clones it into OutDir. Read-only against the destination.
func Restore(ctx context.Context, d RestoreDeps, req RestoreRequest) (*RestoreResult, error) {
	log := orDefault(d.Logger)

	// With a public key the signed manifest is located and its signature verified
	// before a single artifact byte is trusted. Failing to find or verify one is a
	// failure, not a downgrade to the sidecar-only check.
	var checks *restoreChecks
	switch {
	case req.verified != nil:
		// Vouched for by a caller in this package, which must have verified the manifest with
		// the key this restore was given. Anything else is a bug, and ignoring it would be a
		// silent fall back to the unsigned sidecar.
		if d.PublicKey == nil || !d.PublicKey.Equal(req.verified.pub) {
			return nil, fmt.Errorf("restore: %s was not verified with this restore's public key", req.verified.manifestKey)
		}
		checks = req.verified
	case req.ManifestKey != "":
		var err error
		req, checks, err = checksFromManifest(ctx, d, req)
		if err != nil {
			return nil, err
		}
	case d.PublicKey != nil:
		var err error
		checks, err = findRestoreChecks(ctx, d.Dest, d.PublicKey, log, req)
		if err != nil {
			return nil, err
		}
	}
	if checks != nil {
		log.Info("manifest verified", "manifest", checks.manifestKey)
	}
	bundleKey, shaKey, lfsKey := artifactKeys(req.Host, req.Owner, req.Name, req.Date)

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

	// The restore proof, run before anything else looks at the working tree: every ref the
	// bundle declares must be present in what was just cloned, at the same object. Git is
	// content-addressed, so that is exact equality of the history, not a sample of it. If
	// the history is wrong nothing after this matters, so it fails here.
	refs, err := CompareRestoredRefs(ctx, d.Git, bundlePath, req.OutDir)
	if err != nil {
		return nil, fmt.Errorf("compare refs: %w", err)
	}
	// Not reachable by any construction found so far, and kept anyway.
	//
	// For an intact bundle `git clone` places exactly what the header declares, so header and
	// clone are self-consistent: editing an OID in the header makes the clone follow it, and a
	// damaged pack fails earlier with a clone error rather than a ref mismatch. Deleting this
	// branch fails no test in the suite, which was established by deleting it.
	//
	// It stays because the thing it guards is the product's central claim, the cost is one
	// comparison already computed, and the failure it would catch — storage returning a
	// structurally valid bundle that is not the one gitdr wrote — is exactly the failure a
	// WORM backup tool must not paper over. `compareRefs` itself is thoroughly covered in
	// refs_test.go; what is untested is only this wiring.
	if !refs.OK() {
		return nil, fmt.Errorf(
			"restored repository does not match the history %s declares: %d of %d refs present at the same commit, %d differ, first is %s",
			bundleKey, refs.Matched, refs.Declared, len(refs.Mismatches), refs.Mismatches[0])
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
			remaining, err := lfsPointers(ctx, req.OutDir)
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
	} else {
		// No archive to put back, and the tree is read back anyway. A backup taken with
		// backup.lfs off, or where git-lfs was not installed, holds no LFS objects, and its
		// restore used to pass with a pointer in place of every LFS file. Nothing else looked:
		// the check above only ran when there was an archive to extract.
		//
		// With a key, "no archive" is the signed manifest's word, since a stored archive it does
		// not record was refused above. Without one it is the listing's, and a listing that
		// failed has not said there is none.
		pointers, err := lfsPointers(ctx, req.OutDir)
		if err != nil {
			return nil, fmt.Errorf("lfs verify: %w", err)
		}
		if len(pointers) > 0 {
			if listErr != nil && checks == nil {
				return nil, fmt.Errorf("%d file(s) came back as git-lfs pointers, first is %q, and looking for this backup's LFS archive (%s) failed: %w",
					len(pointers), pointers[0], lfsKey, listErr)
			}
			return nil, fmt.Errorf("%d file(s) came back as git-lfs pointers, first is %q, and this backup holds no LFS archive (%s): the content of those files was not backed up. gitdr stores LFS objects only when backup.lfs is on and git-lfs is installed where the backup runs",
				len(pointers), pointers[0], lfsKey)
		}
	}

	res := &RestoreResult{BundleKey: bundleKey, SHA256: gotSHA, OutDir: req.OutDir, Verified: true, Refs: refs}
	// Never claim more verification than happened. The restore that could not check
	// against the signed manifest says so, in the result and in the log.
	var integrity string
	switch {
	case checks != nil && lfsStored:
		integrity = "bundle and lfs tar verified against the signed manifest"
	case checks != nil:
		integrity = "bundle verified against the signed manifest"
	case lfsStored:
		integrity = "bundle checked against its unsigned sha256 sidecar, lfs tar not checked; no public key configured, so the signed manifest was not used"
	default:
		integrity = "bundle checked against its unsigned sha256 sidecar; no public key configured, so the signed manifest was not used"
	}
	// Two different claims, kept apart on purpose. The first is about the bytes: whether
	// the stored bundle is the one the signed manifest records. The second is about the
	// history: whether the repository on disk carries every ref that bundle declares. The
	// ref comparison runs either way, so with no key it is still a real proof — of the
	// restore against an unauthenticated bundle, which is what its wording says.
	res.Verification = integrity + "; " + refs.Summary(checks != nil)
	if checks == nil {
		log.Warn("restore was not verified against the signed manifest; set manifest.publicKeyPath to verify restores",
			"checked", res.Verification)
	}
	log.Info("restored", "bundle", bundleKey, "out", req.OutDir,
		"refsDeclared", refs.Declared, "refsMatched", refs.Matched, "verification", res.Verification)
	return res, nil
}

// restoreChecks are the checksums the signed manifest records for one restore.
type restoreChecks struct {
	manifestKey string
	bundleSHA   string
	lfsSHA      string            // empty when the manifest records no LFS tar for this repo
	pub         ed25519.PublicKey // the key the manifest was verified with
}

// recordedChecks is what a verified manifest's entry records for the artifacts at these keys:
// the bundle's checksum, empty when it records no such bundle, and the LFS archive's, empty when
// it records none.
func recordedChecks(manifestKey string, pub ed25519.PublicKey, entry RepoEntry, bundleKey, lfsKey string) restoreChecks {
	c := restoreChecks{manifestKey: manifestKey, pub: pub}
	for _, a := range entry.Artifacts {
		switch a.Key {
		case bundleKey:
			c.bundleSHA = a.SHA256
		case lfsKey:
			c.lfsSHA = a.SHA256
		}
	}
	return c
}

// checksFromManifest is `restore -manifest`. The named manifest, read through the one loader,
// says where the repository's artifacts are and what they hash to, and the request comes back
// with the host, owner, name and date of the bundle it records.
//
// The entry is the one whose slug is the request's, exactly, and it has to be a copy that run
// made. A repository a run skipped or failed has no copy in that run, and restoring one that some
// other run wrote, under this run's name, would put a verified label on the wrong copy.
//
// From here the restore takes the path a drill's does: the manifest has been verified once, and
// every artifact is held to what it records.
func checksFromManifest(ctx context.Context, d RestoreDeps, req RestoreRequest) (RestoreRequest, *restoreChecks, error) {
	key := req.ManifestKey
	if d.PublicKey == nil {
		return req, nil, fmt.Errorf("restore from %s needs the public key: a manifest is read only once its signature holds; set manifest.publicKeyPath", key)
	}
	if req.Host != "" || req.Date != "" {
		return req, nil, fmt.Errorf("restore from %s: the host and date come from the manifest, and this request gives its own", key)
	}
	m, err := loadManifest(ctx, d.Dest, d.PublicKey, key)
	if err != nil {
		return req, nil, err
	}

	slug := req.Owner + "/" + req.Name
	i := slices.IndexFunc(m.Repos, func(e RepoEntry) bool { return e.Slug == slug })
	if i < 0 {
		return req, nil, fmt.Errorf("the manifest %s records no repository %q", key, slug)
	}
	entry := m.Repos[i]
	if entry.Status != StatusSuccess {
		why := entry.Reason
		if why == "" {
			why = entry.Error
		}
		if why != "" {
			why = " (" + why + ")"
		}
		return req, nil, fmt.Errorf("the manifest %s records %q as %s%s, so this run holds no copy of it to restore: a copy is recorded by the manifest of the run that made it",
			key, slug, entry.Status, why)
	}
	i = slices.IndexFunc(entry.Artifacts, func(a ArtifactInfo) bool { return a.Kind == "bundle" })
	if i < 0 {
		return req, nil, fmt.Errorf("the manifest %s records %q as copied and names no bundle for it, so there is nothing to restore", key, slug)
	}
	recorded := entry.Artifacts[i].Key

	// Parsed from the end, as locate does, and then held to the one layout backup writes. A key
	// that does not come back out of artifactKeys unchanged is not one gitdr wrote.
	host, owner, name, date, ok := splitArtifactKey(recorded)
	bundleKey, _, lfsKey := artifactKeys(host, owner, name, date)
	if !ok || bundleKey != recorded {
		return req, nil, fmt.Errorf("the manifest %s records the bundle of %q at %s, which is not a key gitdr writes: refusing it", key, slug, recorded)
	}
	c := recordedChecks(key, d.PublicKey, entry, bundleKey, lfsKey)
	req.Host, req.Owner, req.Name, req.Date = host, owner, name, date
	return req, &c, nil
}

// findRestoreChecks is restore without -manifest. It finds the signed manifest of the run that
// wrote this bundle and returns the checksums it records.
//
// It looks where backup files one. A run files its manifest under the deepest namespace holding
// every repository it copied, which is this repository's own namespace or one above it, and names
// it for the moment it finished, which is the day of the copy or, for a run that crossed midnight
// UTC, the day after. So the search goes through the repository's namespace, each one above it and
// then the host, and in each through the manifests of the date and of the next day. That finds
// every run from v0.1.20 on that finished within a day of its copy. A manifest an older engine
// filed under another namespace, or a longer run's, takes -manifest.
//
// Artifact keys are create-only, so exactly one run wrote this bundle and only that run's
// manifest records it. Newest first only makes the common case cheap. The search stops at the
// first manifest that records the exact bundle key.
//
// A candidate that cannot be read, or that the loader refuses, is skipped with a warning and
// counted rather than failing the restore: the signing key may have been rotated since, a bucket
// shared by several backup jobs holds manifests signed by other keys, and a copy stored under
// another name is somebody's doing and not this backup's. Skipping trusts nothing, and if nothing
// records the bundle the restore fails and says how many were passed over.
func findRestoreChecks(ctx context.Context, d dest.Destination, pub ed25519.PublicKey, log *slog.Logger, req RestoreRequest) (*restoreChecks, error) {
	day, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		return nil, fmt.Errorf("restore: the date %q is not in the form YYYY-MM-DD", req.Date)
	}
	next := day.AddDate(0, 0, 1)
	bundleKey, _, lfsKey := artifactKeys(req.Host, req.Owner, req.Name, req.Date)
	dirs := manifestSearchDirs(req.Host, req.Owner)

	seen, passed := 0, 0
	for _, dir := range dirs {
		for _, on := range []time.Time{day, next} {
			prefix := path.Join(dir, on.Format("20060102"))
			objs, err := d.List(ctx, prefix)
			if err != nil {
				return nil, fmt.Errorf("list manifests under %s: %w", prefix, err)
			}
			for _, key := range filedManifests(objs, dir) {
				seen++
				m, err := loadManifest(ctx, d, pub, key)
				if err != nil {
					passed++
					log.Warn("skipping a manifest this restore cannot rely on", "manifest", key, "err", err)
					continue
				}
				for _, entry := range m.Repos {
					if c := recordedChecks(key, pub, entry, bundleKey, lfsKey); c.bundleSHA != "" {
						return &c, nil
					}
				}
			}
		}
	}

	searched := fmt.Sprintf("manifests that finished on %s or %s, under %s/",
		day.Format("2006-01-02"), next.Format("2006-01-02"), strings.Join(dirs, "/, "))
	if passed > 0 {
		return nil, fmt.Errorf("no signed manifest records %s: %d of the %d %s did not verify with the configured public key or could not be used, as the warnings above say. If the signing key was rotated, point manifest.publicKeyPath at the key that signed this backup; otherwise pass -manifest <key>, the manifestKey the backup printed",
			bundleKey, passed, seen, searched)
	}
	return nil, fmt.Errorf("no signed manifest records %s: looked through %d %s. The manifest of a run that finished more than a day after the copy, or one an engine older than v0.1.20 filed under another namespace, is reached only by its key: pass -manifest <key>, the manifestKey the backup printed",
		bundleKey, seen, searched)
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
