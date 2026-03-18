// Package pipeline orchestrates a backup run: WORM check -> enumerate -> per repo
// (clone --mirror, bundle, checksum, immutable upload) -> signed run-manifest. A repo
// failure makes the run fail and the manifest records it.
package pipeline

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/source"
)

// BackupDeps are the inputs to a backup run.
type BackupDeps struct {
	Config        *config.Config
	Source        source.Source
	Dest          dest.Destination
	Git           *gitexec.Git
	SigningKey    ed25519.PrivateKey
	EncryptionKey []byte // optional client-side envelope key; nil = off
	ToolVersion   string
	Logger        *slog.Logger
	Now           func() time.Time
	RequireWORM   bool // --require-worm / worm.require: fail closed if not immutable
}

// BackupResult carries the run-manifest and where it was stored. It is returned even
// when the run fails, so callers can surface the recorded failure.
type BackupResult struct {
	Manifest    *Manifest
	ManifestKey string
}

// Backup runs one backup. It returns a non-nil error on any failure (fail-closed); a
// BackupResult may still be returned to report what happened.
func Backup(ctx context.Context, d BackupDeps) (*BackupResult, error) {
	if d.SigningKey == nil {
		return nil, errors.New("backup: manifest signing key is required")
	}
	r := &backupRun{
		cfg: d.Config, src: d.Source, dst: d.Dest, git: d.Git,
		signer: d.SigningKey, encKey: d.EncryptionKey, toolVersion: d.ToolVersion,
		log: orDefault(d.Logger), now: orNow(d.Now), requireWORM: d.RequireWORM,
	}
	return r.run(ctx)
}

type backupRun struct {
	cfg         *config.Config
	src         source.Source
	dst         dest.Destination
	git         *gitexec.Git
	signer      ed25519.PrivateKey
	encKey      []byte
	toolVersion string
	log         *slog.Logger
	now         func() time.Time
	requireWORM bool
	wormStatus  dest.WormStatus // captured by wormCheck, recorded in the manifest
}

func (r *backupRun) run(ctx context.Context) (*BackupResult, error) {
	started := r.now().UTC()

	if err := r.wormCheck(ctx); err != nil {
		return nil, err
	}

	repos, err := r.selectRepos(ctx)
	if err != nil {
		return nil, err
	}

	authHeader, err := gitAuthHeader(ctx, r.src)
	if err != nil {
		return nil, fmt.Errorf("source auth: %w", err)
	}
	// Object Lock retention is only meaningful on an immutable destination. On the
	// adoption path (a non-WORM bucket that we warned about and proceeded past), send no
	// retention so S3 does not reject the write for a bucket without Object Lock enabled.
	ret := r.retention()
	if !r.wormStatus.Locked {
		ret = dest.Retention{}
	}
	if r.cfg.Backup.LFS && !gitexec.LFSAvailable() {
		r.log.Warn("git-lfs not installed; LFS objects will not be backed up")
	}

	entries := r.fanOut(ctx, repos, authHeader, ret)
	allOK := true
	for _, e := range entries {
		if e.Status == StatusFailed {
			allOK = false
		}
	}

	m := &Manifest{
		Schema: ManifestSchema,
		RunID:  newRunID(started),
		Tool:   ToolInfo{Name: "gitdr", Version: r.toolVersion},
		Source: SourceInfo{Type: r.cfg.Source.Type, Host: repos[0].Host},
		Destination: DestInfo{
			Type: r.cfg.Destination.Type, Bucket: r.cfg.Destination.S3.Bucket, WormMode: string(ret.Mode),
			WormImmutable: r.wormStatus.Locked, WormDetails: r.wormStatus.Details,
		},
		StartedAt:  started,
		FinishedAt: r.now().UTC(),
		Status:     statusString(allOK),
		Repos:      entries,
	}

	key, err := r.uploadManifest(ctx, m, repos[0], ret)
	res := &BackupResult{Manifest: m, ManifestKey: key}
	if err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}
	r.log.Info("manifest written", "key", key, "status", m.Status)
	if !allOK {
		return res, errors.New("backup completed with failures")
	}
	return res, nil
}

// wormCheck verifies destination immutability. WORM is recommended, not required:
// configuring it is the operator's responsibility. If the destination is not immutable
// gitdr warns loudly and proceeds, unless requireWORM is set, in which case it fails
// closed.
func (r *backupRun) wormCheck(ctx context.Context) error {
	st, err := r.dst.VerifyWorm(ctx)
	if err != nil {
		r.wormStatus = dest.WormStatus{Details: "could not verify immutability"}
		if r.requireWORM {
			return fmt.Errorf("worm preflight: %w", err)
		}
		r.log.Warn("could not verify destination immutability; proceeding (WORM is recommended)", "err", err)
		return nil
	}
	r.wormStatus = st
	if st.Locked {
		r.log.Info("destination is WORM-immutable", "mode", st.Mode, "details", st.Details)
		return nil
	}
	if r.requireWORM {
		return fmt.Errorf("destination is not WORM-immutable (%s): refusing because worm.require is set", st.Details)
	}
	r.log.Warn("destination is NOT WORM-immutable, backups here can be deleted or overwritten. "+
		"Strongly recommended: enable object-lock/retention on the bucket. Proceeding anyway.", "details", st.Details)
	return nil
}

// selectRepos enumerates and guards against unbounded fan-out (single-repo milestone).
func (r *backupRun) selectRepos(ctx context.Context) ([]source.Repo, error) {
	filter := source.Filter{Include: r.cfg.Source.Include, Exclude: r.cfg.Source.Exclude}
	selector := strings.TrimSpace(r.cfg.Source.Repo)
	if selector != "" {
		filter.Include = []string{selector}
	}
	repos, err := r.src.ListRepos(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("enumerate repos: %w", err)
	}
	if len(repos) == 0 {
		return nil, errors.New("no repositories matched the filter")
	}
	return repos, nil
}

// fanOut backs up repos with bounded concurrency, preserving input order. Each
// goroutine writes a distinct entries[i], so no lock is needed.
func (r *backupRun) fanOut(ctx context.Context, repos []source.Repo, authHeader string, ret dest.Retention) []RepoEntry {
	limit := r.cfg.Backup.Concurrency
	if limit < 1 {
		limit = 1
	}
	entries := make([]RepoEntry, len(repos))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			entries[i] = r.backupOne(ctx, repos[i], authHeader, ret)
		}(i)
	}
	wg.Wait()
	return entries
}

// backupOne adds resume-skip and logging around backupRepo.
func (r *backupRun) backupOne(ctx context.Context, repo source.Repo, authHeader string, ret dest.Retention) RepoEntry {
	if r.cfg.Backup.Resume && r.alreadyBackedUp(ctx, repo) {
		r.log.Info("repo skipped (already backed up)", "repo", repo.Slug())
		return RepoEntry{Slug: repo.Slug(), Status: StatusSkipped}
	}
	entry := r.backupRepo(ctx, repo, authHeader, ret)
	if entry.Status == StatusFailed {
		r.log.Error("repo backup failed", "repo", repo.Slug(), "err", entry.Error)
	} else {
		r.log.Info("repo backup ok", "repo", repo.Slug(), "artifacts", len(entry.Artifacts))
	}
	return entry
}

// alreadyBackedUp reports whether this repo's bundle for the run date already exists.
func (r *backupRun) alreadyBackedUp(ctx context.Context, repo source.Repo) bool {
	date := r.now().UTC().Format("2006-01-02")
	key := path.Join(repo.Host, repo.Owner, repo.Name, date, repo.Name+".bundle")
	objs, err := r.dst.List(ctx, key)
	return err == nil && len(objs) > 0
}

func (r *backupRun) retention() dest.Retention {
	days := r.cfg.Destination.Retention.Days
	mode := dest.RetentionMode(strings.ToUpper(strings.TrimSpace(r.cfg.Destination.Retention.Mode)))
	return dest.Retention{Mode: mode, Until: r.now().UTC().Add(time.Duration(days) * 24 * time.Hour)}
}

// backupRepo clones, bundles, checksums, and uploads one repo's artifacts immutably.
func (r *backupRun) backupRepo(ctx context.Context, repo source.Repo, authHeader string, ret dest.Retention) RepoEntry {
	entry := RepoEntry{Slug: repo.Slug(), Status: StatusSuccess}
	fail := func(err error) RepoEntry {
		entry.Status = StatusFailed
		entry.Error = err.Error()
		return entry
	}

	tmp, err := os.MkdirTemp("", "gitdr-backup-")
	if err != nil {
		return fail(fmt.Errorf("tempdir: %w", err))
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	mirror := filepath.Join(tmp, repo.Name+".git")
	bundlePath := filepath.Join(tmp, repo.Name+".bundle")

	cloneURL, err := r.src.CloneURL(ctx, repo)
	if err != nil {
		return fail(fmt.Errorf("clone url: %w", err))
	}
	if err := retry(ctx, 3, time.Second, func() error {
		_ = os.RemoveAll(mirror) // clear any partial clone before retrying
		return r.git.CloneMirror(ctx, cloneURL, mirror, gitexec.Options{AuthHeader: authHeader})
	}); err != nil {
		return fail(err)
	}
	if err := r.git.BundleAll(ctx, mirror, bundlePath); err != nil {
		return fail(err)
	}

	date := r.now().UTC().Format("2006-01-02")
	prefix := path.Join(repo.Host, repo.Owner, repo.Name, date)

	// bundle (git data); the stored SHA covers the on-disk object (ciphertext if encrypted).
	bres, bundleSHA, err := r.putFile(ctx, path.Join(prefix, repo.Name+".bundle"), bundlePath, ret)
	if err != nil {
		return fail(err)
	}
	entry.Artifacts = append(entry.Artifacts, artifact("bundle", bres, bundleSHA))

	// per-resource metadata
	meta, err := r.src.FetchMetadata(ctx, repo)
	if err != nil {
		return fail(fmt.Errorf("metadata: %w", err))
	}
	mres, metaSHA, err := r.putBytes(ctx, path.Join(prefix, repo.Name+".meta.json"), meta, ret)
	if err != nil {
		return fail(err)
	}
	entry.Artifacts = append(entry.Artifacts, artifact("meta", mres, metaSHA))

	// sha256 sidecar (sha256sum format) over the stored bundle object
	shaLine := fmt.Sprintf("%s  %s\n", bundleSHA, repo.Name+".bundle")
	sres, shaSHA, err := r.putBytes(ctx, path.Join(prefix, repo.Name+".sha256"), []byte(shaLine), ret)
	if err != nil {
		return fail(err)
	}
	entry.Artifacts = append(entry.Artifacts, artifact("sha256", sres, shaSHA))

	// LFS objects (optional): fetch and store as a separate immutable tar artifact.
	if r.cfg.Backup.LFS && gitexec.LFSAvailable() {
		if err := r.git.LFSFetchAll(ctx, mirror, cloneURL, gitexec.Options{AuthHeader: authHeader}); err != nil {
			return fail(fmt.Errorf("lfs fetch: %w", err))
		}
		lfsDir := filepath.Join(mirror, "lfs")
		if dirHasFiles(lfsDir) {
			lfsTar := filepath.Join(tmp, repo.Name+".lfs.tar")
			if err := writeTarFile(lfsDir, lfsTar); err != nil {
				return fail(err)
			}
			lres, lfsSHA, err := r.putFile(ctx, path.Join(prefix, repo.Name+".lfs.tar"), lfsTar, ret)
			if err != nil {
				return fail(err)
			}
			entry.Artifacts = append(entry.Artifacts, artifact("lfs", lres, lfsSHA))
		}
	}

	return entry
}

// uploadManifest signs the canonical manifest and stores it with a detached .sig.
func (r *backupRun) uploadManifest(ctx context.Context, m *Manifest, anchor source.Repo, ret dest.Retention) (string, error) {
	canon, err := m.Canonical()
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	sig := crypto.Sign(r.signer, canon)
	ts := m.FinishedAt.UTC().Format("20060102T150405Z")
	key := path.Join(anchor.Host, anchor.Owner, "manifests", ts+".manifest.json")

	if _, err := r.dst.PutImmutable(ctx, key, bytes.NewReader(canon), int64(len(canon)), ret); err != nil {
		return "", fmt.Errorf("upload manifest: %w", err)
	}
	sigB64 := []byte(base64.StdEncoding.EncodeToString(sig))
	if _, err := r.dst.PutImmutable(ctx, key+".sig", bytes.NewReader(sigB64), int64(len(sigB64)), ret); err != nil {
		return "", fmt.Errorf("upload signature: %w", err)
	}
	return key, nil
}

// retry runs fn up to attempts times with exponential backoff, honoring ctx. A coarse
// safety net for transient clone/network and rate-limit blips.
func retry(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i < attempts-1 {
			select {
			case <-time.After(base << i):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

// putFile encrypts plainPath (when enabled), then SHAs and uploads the stored object,
// returning the put result and the SHA-256 of the stored bytes.
func (r *backupRun) putFile(ctx context.Context, key, plainPath string, ret dest.Retention) (dest.PutResult, string, error) {
	storedPath := plainPath
	if r.encKey != nil {
		storedPath = plainPath + ".enc"
		if err := crypto.EncryptFile(plainPath, storedPath, r.encKey); err != nil {
			return dest.PutResult{}, "", fmt.Errorf("encrypt: %w", err)
		}
		defer func() { _ = os.Remove(storedPath) }()
	}
	sha, size, err := crypto.SHA256File(storedPath)
	if err != nil {
		return dest.PutResult{}, "", err
	}
	f, err := os.Open(storedPath)
	if err != nil {
		return dest.PutResult{}, "", err
	}
	res, err := r.dst.PutImmutable(ctx, key, f, size, ret)
	_ = f.Close()
	return res, sha, err
}

// putBytes encrypts plain (when enabled), then SHAs and uploads the stored object.
func (r *backupRun) putBytes(ctx context.Context, key string, plain []byte, ret dest.Retention) (dest.PutResult, string, error) {
	stored := plain
	if r.encKey != nil {
		var buf bytes.Buffer
		if err := crypto.Encrypt(&buf, bytes.NewReader(plain), r.encKey); err != nil {
			return dest.PutResult{}, "", fmt.Errorf("encrypt: %w", err)
		}
		stored = buf.Bytes()
	}
	res, err := r.dst.PutImmutable(ctx, key, bytes.NewReader(stored), int64(len(stored)), ret)
	return res, crypto.SHA256Bytes(stored), err
}

func artifact(kind string, res dest.PutResult, sha string) ArtifactInfo {
	return ArtifactInfo{Kind: kind, Key: res.Key, Size: res.Size, SHA256: sha, RetainUntil: res.RetainUntil}
}

func gitAuthHeader(ctx context.Context, src source.Source) (string, error) {
	if ga, ok := src.(source.GitAuther); ok {
		return ga.GitAuthHeader(ctx)
	}
	return "", nil
}

func orDefault(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

func orNow(f func() time.Time) func() time.Time {
	if f == nil {
		return time.Now
	}
	return f
}
