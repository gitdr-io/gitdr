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
	"sort"
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
	// Deadline, when set, is when the run's work stops: --deadline or GITDR_DEADLINE. The manifest
	// is written after it all the same, recording what the run did and did not finish.
	Deadline time.Time
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
	pub, ok := d.SigningKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("backup: the manifest signing key has no Ed25519 public half")
	}
	r := &backupRun{
		cfg: d.Config, src: d.Source, dst: d.Dest, git: d.Git,
		signer: d.SigningKey, pub: pub, encKey: d.EncryptionKey, toolVersion: d.ToolVersion,
		log: orDefault(d.Logger), now: orNow(d.Now), requireWORM: d.RequireWORM,
		deadline: d.Deadline,
	}
	return r.run(ctx)
}

type backupRun struct {
	cfg    *config.Config
	src    source.Source
	dst    dest.Destination
	git    *gitexec.Git
	signer ed25519.PrivateKey
	// pub is the signer's public half. Every previous manifest a skip relies on has to verify
	// with it: the next run's comparison and a same-day rerun's search alike.
	pub         ed25519.PublicKey
	encKey      []byte
	toolVersion string
	log         *slog.Logger
	now         func() time.Time
	requireWORM bool
	deadline    time.Time       // when the work stops; zero for none
	wormStatus  dest.WormStatus // captured by wormCheck, recorded in the manifest
	// date is the UTC date the run started, YYYY-MM-DD, and every copy the run makes is filed
	// under it. Read once: read again for each repository, a run that crossed midnight filed the
	// repositories it reached before midnight under one date and the rest under the next.
	date string
	// What the last successful run recorded, by repository slug. Read once per run; empty
	// when there was no previous run or its manifest could not be read, in which case every
	// repository is copied in full. See previous.go.
	previous map[string]previousCopy
	// copies finds the manifest that records a copy under this run's date, for a repository a
	// same-day rerun finds objects for. See recorded.go.
	copies *copySearch
}

func (r *backupRun) run(ctx context.Context) (*BackupResult, error) {
	started := r.now().UTC()
	r.date = started.Format("2006-01-02")

	// The work stops at the deadline, and the manifest does not. It is written after the work
	// with the run's own context, so a run that ran out of time still records what it did and the
	// repositories it did not finish. A deadline on everything would have cut the manifest too,
	// and a run that writes no manifest is the outcome the deadline exists to prevent.
	work := ctx
	if !r.deadline.IsZero() {
		var cancel context.CancelFunc
		work, cancel = context.WithDeadline(ctx, r.deadline)
		defer cancel()
	}

	if err := r.wormCheck(work); err != nil {
		return nil, err
	}

	repos, err := r.selectRepos(work)
	if err != nil {
		return nil, err
	}

	// A preflight, and its header is thrown away. Every git command below asks for its own
	// immediately before it starts, because a credential can change during a run: a token file
	// is replaced, an App token expires after an hour. This stays so that a credential that
	// cannot produce a header at all fails the run here, before any repository is touched.
	if _, err := gitAuthHeader(work, r.src); err != nil {
		return nil, fmt.Errorf("source auth: %w", err)
	}
	// Object Lock retention is only meaningful on an immutable destination. On the
	// adoption path (a non-WORM bucket that we warned about and proceeded past), send no
	// retention so S3 does not reject the write for a bucket without Object Lock enabled.
	ret := r.retention()
	if !r.wormStatus.Verdict.Immutable() {
		ret = dest.Retention{}
	}
	if r.cfg.Backup.LFS && !gitexec.LFSAvailable() {
		r.log.Warn("git-lfs not installed; LFS objects will not be backed up")
	}

	// One List and a few Gets for the whole run, before any repository is touched. What they
	// return decides which repositories can be left alone; see unchanged.go for the rules,
	// including the one that refreshes a copy before its object lock expires.
	selected := make(map[string]bool, len(repos))
	for _, repo := range repos {
		selected[repo.Slug()] = true
	}
	r.previous = r.loadPrevious(work, manifestDir(repos), selected)
	r.copies = newCopySearch(r.dst, r.pub, r.log, func(slug string) bool { return selected[slug] })

	entries := r.fanOut(work, repos, ret)
	allOK := true
	for _, e := range entries {
		if e.Status == StatusFailed {
			allOK = false
		}
	}

	// What actually landed on one object, before the manifest is composed and signed.
	observed, verdict := r.observeRetention(ctx, entries)
	// The case the flag exists for. The objects are already written and cannot be unwritten, so
	// failing closed here can only mean refusing to report a protection that is not there.
	//
	// Only on the earned negative. A store that would not answer has told us nothing, and
	// failing the run on silence would fail it for every operator who took this product's own
	// advice and scoped the destination credential create/put-only.
	if r.requireWORM && observed == dest.RetentionAbsent {
		return nil, fmt.Errorf("worm: the destination accepted this run and applied no retention to it: %s", r.wormStatus.Details)
	}

	m := &Manifest{
		Schema: ManifestSchema,
		RunID:  newRunID(started),
		Tool:   ToolInfo{Name: "gitdr", Version: r.toolVersion},
		Source: SourceInfo{Type: r.cfg.Source.Type, Host: repos[0].Host},
		Destination: DestInfo{
			Type: r.cfg.Destination.Type, Bucket: r.cfg.Destination.S3.Bucket, WormMode: string(ret.Mode),
			WormImmutable: verdict.Immutable(),
			WormVerdict:   verdict.Wire(),
			WormDetails:   r.wormStatus.Details,

			RetentionObserved: string(observed),
		},
		StartedAt:  started,
		FinishedAt: r.now().UTC(),
		Status:     statusString(allOK),
		Repos:      entries,
	}

	key, err := r.uploadManifest(ctx, m, manifestDir(repos), ret)
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
		// The wording is load-bearing. The platform matches "could not verify" to render a
		// manifest written by an engine too old to carry a verdict, and there will be such
		// manifests for years, because customers pin the image.
		r.wormStatus = dest.WormStatus{Verdict: dest.VerdictUnknown, Details: "could not verify immutability"}
		if r.requireWORM {
			return fmt.Errorf("worm preflight: %w", err)
		}
		r.log.Warn("could not verify destination immutability; proceeding (WORM is recommended)", "err", err)
		return nil
	}
	r.wormStatus = st
	if st.Verdict.Immutable() {
		r.log.Info("destination is WORM-immutable", "mode", st.Mode, "details", st.Details)
		return nil
	}
	// --require-worm passes only on a confirmed lock. Invariant 4 already says the gate fires
	// when immutability "cannot be confirmed", and unknown is the definition of that.
	if r.requireWORM {
		return fmt.Errorf("destination is not WORM-immutable (%s): refusing because worm.require is set", st.Details)
	}
	/*
	 * Two warnings, because they are two different facts and they send an operator to two
	 * different places.
	 *
	 * `not-immutable` is a claim about the bucket, and what to do is local: turn object lock
	 * on. `unknown` is a claim about gitdr's own visibility, and what to do is elsewhere: ask
	 * the provider, because gitdr cannot see it from here. Both stay at WARN. Unknown is not
	 * the quieter problem — for an operator it is arguably worse, since nothing they read
	 * here tells them which way it went.
	 */
	if st.Verdict == dest.VerdictUnknown {
		r.log.Warn("could not read this destination's immutability, so backups here may or may not be protected. "+
			"Check the retention settings with the provider; silence here is not evidence against them. Proceeding anyway.",
			"details", st.Details)
		return nil
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
func (r *backupRun) fanOut(ctx context.Context, repos []source.Repo, ret dest.Retention) []RepoEntry {
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
			entries[i] = r.backupOne(ctx, repos[i], ret)
		}(i)
	}
	wg.Wait()
	return entries
}

// backupOne adds resume-skip and logging around backupRepo.
func (r *backupRun) backupOne(ctx context.Context, repo source.Repo, ret dest.Retention) RepoEntry {
	if r.cfg.Backup.Resume {
		if entry, ok := r.resumed(ctx, repo); ok {
			if entry.Status == StatusFailed {
				r.log.Error("repo backup failed", "repo", repo.Slug(), "err", entry.Error)
			} else {
				r.log.Info("repo skipped (already backed up)", "repo", repo.Slug())
			}
			return entry
		}
	}

	// Ask the source what it has, and skip a repository whose refs have not moved since the
	// copy that is still being retained. One network round trip against a clone of the whole
	// history, and on a typical organisation seven repositories in ten do not move on a given
	// day.
	//
	// The refs are carried into backupRepo rather than fetched twice: what gets recorded in
	// the manifest must be the state that was compared, or a repository that changed between
	// the two calls would record refs for a copy that does not contain them.
	current := r.currentRefs(ctx, repo)
	if prev, ok := r.previous[repo.Slug()]; ok {
		d := decideUnchanged(prev.refs, current, prev.copiedAt, r.now(), r.retentionWindow())
		if d.skip {
			copiedAt := prev.copiedAt
			r.log.Info("repo skipped (unchanged)", "repo", repo.Slug(), "reason", d.reason)
			return RepoEntry{
				Slug:   repo.Slug(),
				Status: StatusSkipped,
				Reason: d.reason,
				// Carried forward so the *next* run can compare against this one. Without
				// this a skipped repository loses its ref map and is copied in full the day
				// after, which would make the whole thing an every-other-day saving.
				Refs: refsToEntries(prev.refs),
				// And the age of the copy travels with it. Recording this run's time here
				// would restart the clock on every skip, so the refresh bound would never
				// fire and an unchanging repository would be skipped past its lock's expiry.
				CopiedAt: &copiedAt,
			}
		}
		if d.reason != "" {
			r.log.Info("repo unchanged but the copy is ageing; copying again", "repo", repo.Slug(), "reason", d.reason)
		}
	}

	entry := r.backupRepo(ctx, repo, ret)
	// Recorded only on a copy that succeeded. A failed run's refs describe a repository
	// nothing was written for, and trusting them next time would skip the retry.
	if entry.Status == StatusSuccess && len(current) > 0 {
		entry.Refs = refsToEntries(current)
		copiedAt := r.now().UTC()
		entry.CopiedAt = &copiedAt
	}
	if entry.Status == StatusFailed {
		r.log.Error("repo backup failed", "repo", repo.Slug(), "err", entry.Error)
	} else {
		r.log.Info("repo backup ok", "repo", repo.Slug(), "artifacts", len(entry.Artifacts))
	}
	return entry
}

// resumed decides what a same-day rerun does with a repository that already has objects under
// this run's date. It returns the entry to record and true, or false when nothing is under the
// date and the repository is copied as usual.
//
// A repository is skipped as already backed up only when a manifest records the copy under the
// date (recorded.go), and every object under the date is one that manifest lists for it. The skip
// carries the copy's refs and copiedAt, as an unchanged skip does, so the next day can still tell
// whether anything moved since.
//
// Anything else under the date fails the repository by name, and it is never skipped. Up to
// v0.1.20 any bundle, or for a repository with no commits any metadata, counted as a finished
// copy, so a copy whose checksum or LFS archive never landed was reported as backed up and
// counted as protected. The date cannot be finished either: its keys are create-only, so the
// next copy is made on the next UTC date.
func (r *backupRun) resumed(ctx context.Context, repo source.Repo) (RepoEntry, bool) {
	slug := repo.Slug()
	dir := path.Join(repo.Host, repo.Owner, repo.Name, r.date)
	objs, err := r.dst.List(ctx, dir+"/")
	if err != nil {
		// Not knowing is no reason to skip. The copy goes ahead, and if anything is under the
		// date already the destination refuses the first key that exists.
		r.log.Warn("could not list what is stored under this run's date; copying the repository", "repo", slug, "err", err)
		return RepoEntry{}, false
	}
	// Only what is filed directly in the date's folder. A listing is by prefix and reaches into
	// any deeper folder, such as a GitLab subgroup named like the repository. In key order, so
	// an error names the same object whatever order the store lists them in.
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	stored := map[string]bool{}
	var under []string
	var foreign []dest.Object
	for _, o := range objs {
		if path.Dir(o.Key) != dir {
			continue
		}
		stored[o.Key] = true
		under = append(under, o.Key)
		if o.LastModified.IsZero() || o.LastModified.UTC().Format("2006-01-02") != r.date {
			foreign = append(foreign, o)
		}
	}
	if len(under) == 0 {
		return RepoEntry{}, false
	}

	day, err := time.Parse("2006-01-02", r.date)
	if err != nil {
		return failedEntry(slug, fmt.Errorf("the run's date %q: %w", r.date, err)), true
	}
	next := day.AddDate(0, 0, 1).Format("2006-01-02")
	// Every refusal below leaves the date spent, and says so the same way.
	spent := func(what, detail string) (RepoEntry, bool) {
		return failedEntry(slug, fmt.Errorf("%s (%s); its keys are create-only, so the next copy is on %s", what, detail, next)), true
	}
	incomplete := func(detail string) (RepoEntry, bool) {
		return spent("an incomplete copy for "+r.date+" exists", detail)
	}

	// An object counts only if the store wrote it on the date its key names. A run writes a
	// date's objects that day, so one written earlier was put there ahead of the date, by
	// something other than a run, and a copy planted for a future date must fail that day's run
	// rather than be skipped by it. A store that does not say when it wrote an object has not
	// shown that either. A run that crosses midnight writes its last repositories the next day,
	// and only a second run of the same date that reached them after that is refused here.
	if len(foreign) > 0 {
		o := foreign[0]
		when := "and the destination does not say when it was written"
		if !o.LastModified.IsZero() {
			when = "and the destination wrote it at " + o.LastModified.UTC().Format(time.RFC3339)
		}
		return spent("an object under "+r.date+" was not written that day", fmt.Sprintf("%s is filed under the date %s", o.Key, when))
	}

	bundleKey, _, _ := artifactKeys(repo.Host, repo.Owner, repo.Name, r.date)
	metaKey := path.Join(dir, repo.Name+".meta.json")
	found, rep, err := r.copies.find(ctx, manifestSearchDirs(repo.Host, repo.Owner), day, slug, []string{bundleKey, metaKey}, r.now())
	if err != nil {
		return failedEntry(slug, fmt.Errorf("a copy for %s exists, and finding the manifest that records it failed: %w", r.date, err)), true
	}
	if found == nil {
		detail := fmt.Sprintf("no manifest records %s as a copy", strings.Join(baseNames(under), ", "))
		if rep.named != nil {
			detail = fmt.Sprintf("%s records it as %s", rep.namedIn, rep.named.Status)
		}
		return incomplete(detail)
	}

	listed := map[string]bool{}
	for _, a := range found.entry.Artifacts {
		listed[a.Key] = true
		if !stored[a.Key] {
			return incomplete(fmt.Sprintf("%s records %s, and the destination does not have it", found.manifestKey, a.Key))
		}
	}
	for _, key := range under {
		if !listed[key] {
			return incomplete(fmt.Sprintf("%s is under the date, and %s does not record it", key, found.manifestKey))
		}
	}

	copiedAt := copiedAtOf(found.entry, found.finishedAt)
	if copiedAt != nil {
		if err := plausibleCopiedAt(*copiedAt, found.finishedAt, r.now()); err != nil {
			return spent("the manifest that records the copy for "+r.date+" cannot be believed", fmt.Sprintf("%s: %v", found.manifestKey, err))
		}
	}

	return RepoEntry{
		Slug:     slug,
		Status:   StatusSkipped,
		Reason:   ReasonResume,
		Refs:     found.entry.Refs,
		CopiedAt: copiedAt,
	}, true
}

// plausibleCopiedAt refuses a copy's copiedAt that no run could have recorded: one later than the
// finish of the manifest that holds it, or later than now. A skip relies on copiedAt for the age
// of the copy, and an age that never grows never reaches the refresh, so one manifest naming a
// copy made in the future would have skipped the repository for good while every run stayed
// green.
func plausibleCopiedAt(copiedAt, finished, now time.Time) error {
	switch {
	case copiedAt.After(finished):
		return fmt.Errorf("it says the copy was made at %s, after the manifest itself finished at %s",
			copiedAt.UTC().Format(time.RFC3339), finished.UTC().Format(time.RFC3339))
	case copiedAt.After(now):
		return fmt.Errorf("it says the copy was made at %s, which is later than now", copiedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// failedEntry is a repository this run failed with err before writing anything of it.
func failedEntry(slug string, err error) RepoEntry {
	return RepoEntry{Slug: slug, Status: StatusFailed, Error: err.Error()}
}

// baseNames is each key's last path segment, for an error that names objects of one folder.
func baseNames(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = path.Base(k)
	}
	return out
}

// retentionWindow is how long a copy is kept, as a duration.
//
// The skip decision needs it to know when a copy is close enough to expiring that it must be
// rewritten even though nothing changed. Object lock protects an object until its retain-until
// and not one second longer.
func (r *backupRun) retentionWindow() time.Duration {
	return time.Duration(r.cfg.Destination.Retention.Days) * 24 * time.Hour
}

// refreshBound is how old a copy may get before this run writes it again; see unchanged.go.
func (r *backupRun) refreshBound() time.Duration {
	var retention time.Duration
	if r.cfg != nil {
		retention = r.retentionWindow()
	}
	return refreshBound(retention)
}

func (r *backupRun) retention() dest.Retention {
	days := r.cfg.Destination.Retention.Days
	mode := dest.RetentionMode(strings.ToUpper(strings.TrimSpace(r.cfg.Destination.Retention.Mode)))
	return dest.Retention{Mode: mode, Until: r.now().UTC().Add(time.Duration(days) * 24 * time.Hour)}
}

// backupRepo clones, bundles, checksums, and uploads one repo's artifacts immutably.
func (r *backupRun) backupRepo(ctx context.Context, repo source.Repo, ret dest.Retention) RepoEntry {
	entry := RepoEntry{Slug: repo.Slug(), Status: StatusSuccess}
	fail := func(err error) RepoEntry {
		entry.Status = StatusFailed
		entry.Error = err.Error()
		entry.Reason = "" // a reason belongs to a skip, and an empty repository can still fail
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
		// Inside the retry, so an attempt made after the token was replaced uses the
		// replacement rather than the credential that just failed.
		auth, err := gitAuthHeader(ctx, r.src)
		if err != nil {
			return fmt.Errorf("source auth: %w", err)
		}
		return r.git.CloneMirror(ctx, cloneURL, mirror, gitexec.Options{AuthHeader: auth})
	}); err != nil {
		return fail(err)
	}
	// A repository that was created and never pushed to has no refs, and `git bundle create`
	// refuses to write an empty bundle. Treating that refusal as a failed repository would
	// mean one unused project makes every backup of the organisation fail, for ever, while
	// nothing has actually been lost: there are no commits to lose.
	//
	// Recorded in the manifest as skipped-with-a-reason rather than passed over silently. The
	// repository was seen, and the run is auditable about what it did with it.
	hasRefs, err := r.git.HasRefs(ctx, mirror)
	if err != nil {
		return fail(fmt.Errorf("check refs: %w", err))
	}

	prefix := path.Join(repo.Host, repo.Owner, repo.Name, r.date)

	// The metadata is fetched before anything is written, and what a failure costs depends on
	// whether it is likely to pass. A transient one, a rate limit that could not be waited out or
	// a server error that outlasted its retries, fails the repository here with nothing stored:
	// the next run will probably get through, and nothing written to the destination can be taken
	// back. Any other failure, a permission missing say, would fail every run the same way, so the
	// code is stored regardless, bundle and checksum, and the repository fails on its metadata
	// after that, as it did before v0.1.21.
	meta, metaErr := r.src.FetchMetadata(ctx, repo)
	if metaErr != nil && (errors.Is(metaErr, source.ErrTransient) || ctx.Err() != nil) {
		return fail(fmt.Errorf("metadata: %w", metaErr))
	}

	// No commits, so no bundle — but the metadata is still stored below. A repository with no
	// code can still carry issues, labels and milestones, and dropping those because nobody
	// pushed a commit would be a silent loss of exactly the kind this tool exists to prevent.
	if !hasRefs {
		r.log.Info("repo has no commits; storing metadata only", "repo", repo.Slug())
		entry.Status = StatusSkipped
		entry.Reason = ReasonEmpty
	} else {
		if err := r.git.BundleAll(ctx, mirror, bundlePath); err != nil {
			return fail(err)
		}
	}

	// bundle (git data); the stored SHA covers the on-disk object (ciphertext if encrypted).
	// Skipped entirely for a repository with no commits: there is no bundle, and so no sha256
	// sidecar either, since that file describes the bundle.
	var bundleSHA string
	if hasRefs {
		bres, sha, err := r.putFile(ctx, path.Join(prefix, repo.Name+".bundle"), bundlePath, ret)
		if err != nil {
			return fail(err)
		}
		bundleSHA = sha
		entry.Artifacts = append(entry.Artifacts, artifact("bundle", bres, bundleSHA))
	}

	// per-resource metadata, fetched above
	if metaErr == nil {
		mres, metaSHA, err := r.putBytes(ctx, path.Join(prefix, repo.Name+".meta.json"), meta, ret)
		if err != nil {
			return fail(err)
		}
		entry.Artifacts = append(entry.Artifacts, artifact("meta", mres, metaSHA))
	}

	// sha256 sidecar (sha256sum format) over the stored bundle object
	if hasRefs {
		shaLine := fmt.Sprintf("%s  %s\n", bundleSHA, repo.Name+".bundle")
		sres, shaSHA, err := r.putBytes(ctx, path.Join(prefix, repo.Name+".sha256"), []byte(shaLine), ret)
		if err != nil {
			return fail(err)
		}
		entry.Artifacts = append(entry.Artifacts, artifact("sha256", sres, shaSHA))
	}

	// The code is stored, and the repository fails on the metadata it could not have.
	if metaErr != nil {
		return fail(fmt.Errorf("metadata: %w", metaErr))
	}

	// LFS objects (optional): fetch and store as a separate immutable tar artifact.
	// Nothing to fetch without refs: LFS objects are pointed at by commits.
	if hasRefs && r.cfg.Backup.LFS && gitexec.LFSAvailable() {
		auth, err := gitAuthHeader(ctx, r.src)
		if err != nil {
			return fail(fmt.Errorf("lfs fetch: source auth: %w", err))
		}
		if err := r.git.LFSFetchAll(ctx, mirror, cloneURL, gitexec.Options{AuthHeader: auth}); err != nil {
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

// uploadManifest signs the canonical manifest and stores it with a detached .sig in dir, named
// for the moment the run finished. See manifestDir for which directory, and loadManifest for why
// the name has to be that moment.
func (r *backupRun) uploadManifest(ctx context.Context, m *Manifest, dir string, ret dest.Retention) (string, error) {
	canon, err := m.Canonical()
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	sig := crypto.Sign(r.signer, canon)
	key := path.Join(dir, m.FinishedAt.UTC().Format(manifestStamp)+manifestSuffix)

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

// gitAuthHeader asks the source for the header one git command sends, or "" for a source that
// has no credential. It is called immediately before each git network command, never once for
// the run; see source.GitAuther.
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

/*
 * Ask the destination what retention is on the first object this run wrote.
 *
 * One object, not every object. The check is a strong falsifier and a weak confirmer, and the
 * design leans on exactly that: a store applies object lock in the PUT path, so one that drops
 * the header for the first object drops it for all of them, and a negative therefore generalises
 * from a single sample. A positive does not - it proves this object is retained and that the
 * store implements the headers, and nothing about the rest. Per-object checks would be three or
 * four thousand extra requests on a large estate buying detection of an anomaly the protocol does
 * not produce.
 *
 * It may only ever lower the verdict. There is no path here that turns "not-immutable" into
 * "immutable", no badge and no green: the whole purpose is to remove a claim gitdr could not
 * back, and a confirmation from one object is not entitled to add one.
 *
 * Only on the immutable path, because that is the only path where retention was requested at all.
 * A run whose repositories were all skipped as unchanged has written nothing to look at, and
 * records not-checked rather than inventing an answer.
 */
func (r *backupRun) observeRetention(ctx context.Context, entries []RepoEntry) (dest.RetentionObservation, dest.WormVerdict) {
	verdict := r.wormStatus.Verdict
	if !verdict.Immutable() {
		return dest.RetentionNotChecked, verdict
	}
	observer, ok := r.dst.(dest.RetentionObserver)
	if !ok {
		return dest.RetentionNotChecked, verdict
	}
	var key string
	for _, e := range entries {
		if len(e.Artifacts) > 0 {
			key = e.Artifacts[0].Key
			break
		}
	}
	if key == "" {
		return dest.RetentionNotChecked, verdict
	}

	got, until, err := observer.ObserveRetention(ctx, key)
	switch got {
	case dest.RetentionPresent:
		r.log.Info("retention observed", "key", key, "until", until.Format(time.RFC3339))
	case dest.RetentionAbsent:
		// The earned negative, and the reason all of this exists. The bucket said it locks, the
		// write was accepted, and the object holds nothing. The objects cannot be unwritten -
		// the destination is create-only - so failing closed here can only mean refusing to
		// report a protection that is not there.
		r.log.Warn("the destination accepted this run and applied no retention to it",
			"key", key, "bucket_said", r.wormStatus.Details)
		verdict = dest.VerdictNotImmutable
	default:
		// A refusal is not a no. On S3 this is the common case rather than the exotic one:
		// reading an object's retention needs s3:GetObjectRetention, and this product tells
		// operators to scope destination credentials create/put-only.
		r.log.Info("could not confirm the retention on this run's objects", "key", key, "err", err)
	}
	return got, verdict
}
