// Package pipeline orchestrates a backup run: WORM check -> enumerate -> per repo
// (clone --mirror, bundle, checksum, immutable upload) -> signed run-manifest. A repo
// failure makes the run fail and the manifest records it.
package pipeline

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
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

	// The work stops at the deadline, or at a stop signal, and the manifest does not. It is
	// written after the work on a context of its own, so a run that ran out of time or was told
	// to stop still records what it did and the repositories it did not finish. A deadline on
	// everything would have cut the manifest too, and a run that writes no manifest is the
	// outcome the deadline exists to prevent.
	work := ctx
	if !r.deadline.IsZero() {
		var cancel context.CancelFunc
		work, cancel = context.WithDeadlineCause(ctx, r.deadline,
			fmt.Errorf("the run's deadline %s passed", r.deadline.UTC().Format(time.RFC3339)))
		defer cancel()
	}
	final, stopFinal := manifestContext(ctx, manifestGrace)
	defer stopFinal()

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

	// The first of the run's events (SPEC §11): how many repositories it will report on. From
	// here every one of them gets its repo finished, however the run ends.
	r.log.Info(EventReposSelected, "count", len(repos))
	entries := r.fanOut(work, repos, ret)
	allOK := true
	for _, e := range entries {
		if e.Status == StatusFailed {
			allOK = false
		}
	}
	if work.Err() != nil {
		r.log.Warn("the run was stopped before it finished; filing what it did", "cause", context.Cause(work))
	}

	// What actually landed on one object, before the manifest is composed and signed.
	observed, verdict := r.observeRetention(final, entries)
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

		Destination: r.destInfo(ret, verdict, observed),

		StartedAt:  started,
		FinishedAt: r.now().UTC(),
		Status:     statusString(allOK),
		Repos:      entries,
	}

	key, err := r.uploadManifest(final, m, manifestDir(repos), ret)
	res := &BackupResult{Manifest: m, ManifestKey: key}
	if err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}
	r.log.Info(EventManifestWritten, "key", key, "status", m.Status)
	if !allOK {
		return res, errors.New("backup completed with failures")
	}
	return res, nil
}

// destInfo is the manifest's destination block.
//
// bucket names what the copies were written to: the S3 or GCS bucket, or the Azure container.
// wormMode is the mode gitdr set on each object, and it sets one on S3 alone. On GCS and Azure the
// bucket's or the container's own policy locks every copy, so there is no mode to record. Up to
// v0.1.20 the block was S3's wherever a run wrote: off S3 the bucket was empty, and a locked GCS
// bucket signed the configured COMPLIANCE, a mode Google was never sent.
func (r *backupRun) destInfo(ret dest.Retention, verdict dest.WormVerdict, observed dest.RetentionObservation) DestInfo {
	d := DestInfo{
		Type:              r.cfg.Destination.Type,
		WormImmutable:     verdict.Immutable(),
		WormVerdict:       verdict.Wire(),
		WormDetails:       r.wormStatus.Details,
		RetentionObserved: string(observed),
	}
	switch r.cfg.Destination.Type {
	case "s3":
		d.Bucket = r.cfg.Destination.S3.Bucket
		d.WormMode = string(ret.Mode)
	case "gcs":
		d.Bucket = r.cfg.Destination.GCS.Bucket
	case "azure":
		d.Bucket = r.cfg.Destination.Azure.Container
	}
	return d
}

// wormCheck verifies destination immutability. WORM is recommended, not required:
// configuring it is the operator's responsibility. If the destination is not immutable
// gitdr warns loudly and proceeds, unless requireWORM is set, in which case it fails
// closed. A bucket the store says does not exist fails the run either way.
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
	// A bucket that is not there is not a WORM question: there is nothing to write into. So the run
	// stops here, before it lists or clones anything, worm.require or not. It used to warn that it
	// could not read the bucket's immutability, clone every repository, and fail each one at its
	// first upload.
	if errors.Is(st.Refusal, dest.ErrNoSuchBucket) {
		return fmt.Errorf("destination: %w; nothing was copied", st.Refusal)
	}
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

// manifestGrace is how long a stopped run has to file its manifest, from the moment it was
// stopped. The caller that stops it waits longer than this before it kills it.
const manifestGrace = 45 * time.Second

// inFlightGrace is how much of manifestGrace a stopped run gives the repositories it has in
// flight. The other 15 s are the manifest's own.
//
// A stop ends most of what a repository does at once: its git commands are killed, its reads and
// writes are on the stopped context, and its archive, encryption and checksums stop at their next
// read. A step that does none of that, a whole-file checksum a store computes before it sends a
// byte say, used to hold the run until it was done, and the grace ran out under the manifest's own
// upload. A variable, so a test can shorten it (export_test.go).
var inFlightGrace = manifestGrace - 15*time.Second

// manifestContext is the context the manifest is written on. Stopping ctx does not cancel it,
// because a stopped run still has to file what it did: the copies it finished are in the bucket,
// and without a manifest nothing can verify, restore or drill them, and the next run cannot skip
// them. Once ctx is stopped it lasts grace longer, so a stop still ends the process.
//
// Up to v0.1.20 the manifest was written on the context a SIGTERM cancels, so a stopped run
// filed nothing at all.
func manifestContext(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	final, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	return final, func() { stop(); cancel() }
}

// stoppedBefore is the error of a repository the run was stopped before finishing.
const stoppedBefore = "stopped before it finished"

// fanOut backs up repos with bounded concurrency, preserving input order.
//
// Once ctx is done, at the deadline or on a stop signal, nothing more is started. A repository
// that was never started, or that failed after the stop, is recorded as failed and stopped before
// it finished, so the manifest names every repository the run selected. The ones in flight are
// waited for, their git commands killed with the context, for inFlightGrace at most. One still
// running then is recorded as stopped before it finished, with what it had written, and the run
// goes on to its manifest without waiting for it any longer.
func (r *backupRun) fanOut(ctx context.Context, repos []source.Repo, ret dest.Retention) []RepoEntry {
	limit := r.cfg.Backup.Concurrency
	if limit < 1 {
		limit = 1
	}
	entries := make([]RepoEntry, len(repos))
	progress := make([]*copyProgress, len(repos))
	// Each entry is settled once, by its repository or by the run that stopped waiting for it,
	// whichever comes first, and its repo finished event is written then. What a repository
	// returns after the run stopped waiting for it is dropped.
	var mu sync.Mutex
	settled := make([]bool, len(repos))
	settle := func(i int, e RepoEntry) {
		mu.Lock()
		defer mu.Unlock()
		if settled[i] {
			return
		}
		entries[i], settled[i] = e, true
		r.finished(e)
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range repos {
		acquired := false
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			if acquired {
				<-sem
			}
			cause := context.Cause(ctx)
			r.log.Warn("repositories not started; the run was stopped", "count", len(repos)-i, "cause", cause)
			for j := i; j < len(repos); j++ {
				settle(j, failedEntry(repos[j].Slug(), fmt.Errorf("%s: %w", stoppedBefore, cause)))
			}
			break
		}
		progress[i] = &copyProgress{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			e := r.backupOne(ctx, repos[i], ret, progress[i])
			if e.Status == StatusFailed && ctx.Err() != nil {
				e.Error = stoppedBefore + ": " + e.Error
			}
			settle(i, e)
		}(i)
	}

	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	select {
	case <-all:
		return entries
	case <-ctx.Done():
	}
	wait := time.NewTimer(inFlightGrace)
	defer wait.Stop()
	select {
	case <-all:
		return entries
	case <-wait.C:
	}

	mu.Lock()
	defer mu.Unlock()
	cause := context.Cause(ctx)
	for i := range repos {
		if settled[i] {
			continue
		}
		r.log.Warn("a repository was still running when the run could wait no longer; recording it as stopped",
			"repo", repos[i].Slug(), "waited", inFlightGrace)
		e := failedEntry(repos[i].Slug(), fmt.Errorf("%s: %w; still running %s after the stop", stoppedBefore, cause, inFlightGrace))
		e.Artifacts = progress[i].written()
		entries[i], settled[i] = e, true
		r.finished(e)
	}
	return entries
}

// copyProgress is what one repository has written so far. The run reads it when it stops waiting
// for a repository that may still be writing, so it is locked.
type copyProgress struct {
	mu        sync.Mutex
	artifacts map[string]ArtifactInfo // by kind
}

func (p *copyProgress) wrote(a ArtifactInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.artifacts == nil {
		p.artifacts = map[string]ArtifactInfo{}
	}
	p.artifacts[a.Kind] = a
}

// written is what has been written so far, in the order a manifest lists a repository's artifacts
// whatever order they were written in.
func (p *copyProgress) written() []ArtifactInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ArtifactInfo
	for _, kind := range []string{"bundle", "meta", "sha256", "lfs"} {
		if a, ok := p.artifacts[kind]; ok {
			out = append(out, a)
		}
	}
	return out
}

// The names of the events a backup writes to stderr, part of the output contract (SPEC §11): a
// caller reads them as they come to show progress, and matches on these strings.
const (
	// EventReposSelected comes once, with "count", the repositories the run will report on.
	EventReposSelected = "repos selected"
	// EventRepoFinished comes once per repository, after its last write.
	EventRepoFinished = "repo finished"
	// EventManifestWritten comes once, with "key" and "status", after the manifest is stored.
	EventManifestWritten = "manifest written"
)

// finishedArtifact is an artifact as the repo finished event names it.
type finishedArtifact struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// finished writes the repo finished event for e, which is final: what the manifest will record for
// the repository. reason, error and copiedAt are there when the entry has them, copiedAt in the
// manifest's own format.
func (r *backupRun) finished(e RepoEntry) {
	attrs := []any{"slug", e.Slug, "status", e.Status}
	if e.Reason != "" {
		attrs = append(attrs, "reason", e.Reason)
	}
	if e.Error != "" {
		attrs = append(attrs, "error", e.Error)
	}
	artifacts := make([]finishedArtifact, 0, len(e.Artifacts))
	for _, a := range e.Artifacts {
		artifacts = append(artifacts, finishedArtifact{Kind: a.Kind, Key: a.Key, Size: a.Size})
	}
	attrs = append(attrs, "artifacts", artifacts)
	if e.CopiedAt != nil {
		attrs = append(attrs, "copiedAt", e.CopiedAt.UTC().Format(time.RFC3339Nano))
	}
	r.log.Info(EventRepoFinished, attrs...)
}

// backupOne adds resume-skip and logging around backupRepo, which records each artifact it writes in
// progress.
func (r *backupRun) backupOne(ctx context.Context, repo source.Repo, ret dest.Retention, progress *copyProgress) RepoEntry {
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

	entry := r.backupRepo(ctx, repo, ret, progress)
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

// retentionWindow is how long a copy is kept, as a duration: the configured days, or the lock the
// destination reported when that is shorter.
//
// The skip decision needs it to know when a copy is close enough to expiring that it must be
// rewritten even though nothing changed. Object lock protects an object until its retain-until
// and not one second longer.
//
// The configured days are what gitdr locks each object for on S3, where the store reports no period
// of its own. On GCS and Azure gitdr locks nothing: the bucket's retention policy or the container's
// immutability policy holds every copy for its own period, whatever the configuration says. Up to
// v0.1.20 the window was the configured days everywhere, so in a bucket that locks for one day a copy
// was relied on for ten.
func (r *backupRun) retentionWindow() time.Duration {
	var window time.Duration
	if r.cfg != nil {
		window = time.Duration(r.cfg.Destination.Retention.Days) * 24 * time.Hour
	}
	if held := r.wormStatus.Period; held > 0 && (window <= 0 || held < window) {
		window = held
	}
	return window
}

// refreshBound is how old a copy may get before this run writes it again; see unchanged.go.
func (r *backupRun) refreshBound() time.Duration {
	return refreshBound(r.retentionWindow())
}

func (r *backupRun) retention() dest.Retention {
	days := r.cfg.Destination.Retention.Days
	mode := dest.RetentionMode(strings.ToUpper(strings.TrimSpace(r.cfg.Destination.Retention.Mode)))
	return dest.Retention{Mode: mode, Until: r.now().UTC().Add(time.Duration(days) * 24 * time.Hour)}
}

// backupRepo clones, bundles, checksums, and uploads one repo's artifacts immutably.
//
// In two steps, and the order is the point. First everything that reads the source or can fail
// locally: the metadata, the clone, the LFS objects, the archive, the bundle, the encryption and
// the checksums. Nothing is written to the destination until all of that has succeeded, so a
// failure in it leaves nothing under the date, and the same day's rerun copies the repository
// cleanly. Then the uploads, largest first, and the checksum sidecar last, so a restore that goes
// by the sidecar never finds one beside a partial copy.
//
// A failure among the uploads spends the date: the keys already written are create-only, and the
// same day's rerun fails the repository by name (resumed). Up to v0.1.20 the bundle, the metadata
// and the checksum were stored before the LFS objects were even fetched, so an LFS failure always
// left a partial copy.
//
// Each artifact goes into progress as it lands, so a repository the run stops waiting for is still
// recorded with what it wrote (fanOut).
func (r *backupRun) backupRepo(ctx context.Context, repo source.Repo, ret dest.Retention, progress *copyProgress) RepoEntry {
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
	prefix := path.Join(repo.Host, repo.Owner, repo.Name, r.date)
	key := func(suffix string) string { return path.Join(prefix, repo.Name+suffix) }

	cloneURL, err := r.src.CloneURL(ctx, repo)
	if err != nil {
		return fail(fmt.Errorf("clone url: %w", err))
	}

	// The metadata first, before the clone, so a wait for a rate limit holds no scratch space.
	// What a failure costs depends on whether it is likely to pass. A transient one, a rate limit
	// that could not be waited out or a server error that outlasted its retries, fails the
	// repository here with nothing stored: the next run will probably get through. Any other
	// failure, a permission missing say, would fail every run the same way, so the code is stored
	// regardless and the repository fails on its metadata after the uploads.
	meta, metaErr := r.src.FetchMetadata(ctx, repo)
	if metaErr != nil && (errors.Is(metaErr, source.ErrTransient) || ctx.Err() != nil) {
		return fail(fmt.Errorf("metadata: %w", metaErr))
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
	// repository was seen, and the run is auditable about what it did with it. A repository with
	// no code can still carry issues, labels and milestones, so its metadata is still stored.
	hasRefs, err := r.git.HasRefs(ctx, mirror)
	if err != nil {
		return fail(fmt.Errorf("check refs: %w", err))
	}
	if !hasRefs {
		r.log.Info("repo has no commits; storing metadata only", "repo", repo.Slug())
		entry.Status = StatusSkipped
		entry.Reason = ReasonEmpty
	}

	var files []*stagedArtifact
	// LFS objects, fetched while the source is being read and archived before anything is
	// written. Nothing to fetch without refs: LFS objects are pointed at by commits.
	if hasRefs && r.cfg.Backup.LFS && gitexec.LFSAvailable() {
		auth, err := gitAuthHeader(ctx, r.src)
		if err != nil {
			return fail(fmt.Errorf("lfs fetch: source auth: %w", err))
		}
		if err := r.git.LFSFetchAll(ctx, mirror, cloneURL, gitexec.Options{AuthHeader: auth}); err != nil {
			return fail(fmt.Errorf("lfs fetch: %w", err))
		}
		if lfsDir := filepath.Join(mirror, "lfs"); dirHasFiles(lfsDir) {
			lfsTar := filepath.Join(tmp, repo.Name+".lfs.tar")
			if err := archiveLFS(ctx, lfsDir, lfsTar); err != nil {
				return fail(err)
			}
			files = append(files, &stagedArtifact{kind: "lfs", key: key(".lfs.tar"), path: lfsTar})
		}
	}
	if hasRefs {
		if err := r.git.BundleAll(ctx, mirror, bundlePath); err != nil {
			return fail(err)
		}
		files = append(files, &stagedArtifact{kind: "bundle", key: key(".bundle"), path: bundlePath})
	}
	// The bundle holds everything the mirror did, and the scratch space is needed for what follows.
	if err := os.RemoveAll(mirror); err != nil {
		return fail(fmt.Errorf("remove the mirror: %w", err))
	}

	// Encrypted, when that is on, and checksummed: the stored checksum covers the stored object,
	// ciphertext when encrypted.
	for _, f := range files {
		if err := r.stageFile(ctx, f); err != nil {
			return fail(err)
		}
	}
	staged := files
	if metaErr == nil {
		m, err := r.stageBytes("meta", key(".meta.json"), meta)
		if err != nil {
			return fail(err)
		}
		staged = append(staged, m)
	}
	// Largest first. The longest upload is the likeliest to fail, and it then fails with the least
	// written beside it.
	slices.SortStableFunc(staged, func(a, b *stagedArtifact) int { return cmp.Compare(b.size, a.size) })
	// The sha256 sidecar (sha256sum format) over the stored bundle object, and only for a bundle.
	if hasRefs {
		var bundleSHA string
		for _, a := range files {
			if a.kind == "bundle" {
				bundleSHA = a.sha
			}
		}
		line := fmt.Sprintf("%s  %s\n", bundleSHA, repo.Name+".bundle")
		s, err := r.stageBytes("sha256", key(".sha256"), []byte(line))
		if err != nil {
			return fail(err)
		}
		staged = append(staged, s)
	}

	// Every upload in that order, and the sidecar last.
	for _, a := range staged {
		res, err := r.putStaged(ctx, a, ret)
		if err != nil {
			entry.Artifacts = progress.written() // a failed entry still lists what it wrote
			return fail(err)
		}
		progress.wrote(artifact(a.kind, res, a.sha))
	}
	entry.Artifacts = progress.written()

	// The code is stored, and the repository fails on the metadata it could not have.
	if metaErr != nil {
		return fail(fmt.Errorf("metadata: %w", metaErr))
	}
	return entry
}

// stagedArtifact is one artifact ready to upload: its stored bytes, on disk or in memory, with
// their size and SHA-256.
type stagedArtifact struct {
	kind, key string
	path      string // the stored file; empty for one held in data
	data      []byte
	size      int64
	sha       string
}

// stageFile encrypts a's file when encryption is on, and checksums what will be stored. The
// plaintext is removed once the ciphertext exists, as the scratch disk holds one of them at a
// time. Both stop with ctx.
func (r *backupRun) stageFile(ctx context.Context, a *stagedArtifact) error {
	if r.encKey != nil {
		enc := a.path + ".enc"
		if err := crypto.EncryptFile(ctx, a.path, enc, r.encKey); err != nil {
			return fmt.Errorf("encrypt: %w", err)
		}
		_ = os.Remove(a.path)
		a.path = enc
	}
	sha, size, err := crypto.SHA256File(ctx, a.path)
	if err != nil {
		return err
	}
	a.sha, a.size = sha, size
	return nil
}

// stageBytes is stageFile for an artifact held in memory.
func (r *backupRun) stageBytes(kind, key string, plain []byte) (*stagedArtifact, error) {
	stored := plain
	if r.encKey != nil {
		var buf bytes.Buffer
		if err := crypto.Encrypt(&buf, bytes.NewReader(plain), r.encKey); err != nil {
			return nil, fmt.Errorf("encrypt: %w", err)
		}
		stored = buf.Bytes()
	}
	return &stagedArtifact{kind: kind, key: key, data: stored, size: int64(len(stored)), sha: crypto.SHA256Bytes(stored)}, nil
}

// putStaged uploads one staged artifact, create-only.
func (r *backupRun) putStaged(ctx context.Context, a *stagedArtifact, ret dest.Retention) (dest.PutResult, error) {
	if a.path == "" {
		return r.dst.PutImmutable(ctx, a.key, bytes.NewReader(a.data), a.size, ret)
	}
	f, err := os.Open(a.path)
	if err != nil {
		return dest.PutResult{}, err
	}
	defer func() { _ = f.Close() }()
	return r.dst.PutImmutable(ctx, a.key, f, a.size, ret)
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
 * Ask the destination what retention is on the objects this run wrote, one from each way it
 * writes them.
 *
 * One object per write path, not every object. The check is a strong falsifier and a weak
 * confirmer, and the design leans on exactly that: a store applies object lock in its write path,
 * so one that drops the header for one object drops it for every object written the same way, and
 * a negative generalises from a single sample. A positive does not - it proves this object is
 * retained and that the store implements the headers, and nothing about the rest. Per-object
 * checks would be three or four thousand extra requests on a large estate buying detection of an
 * anomaly the protocol does not produce.
 *
 * But the generalisation stops at the path. An object over the multipart threshold is written in
 * parts, with the lock headers on CreateMultipartUpload rather than on PutObject, and a store can
 * honour them on one and not the other. Backblaze documents no lock headers on its multipart call
 * at all. The run used to ask about the first object it wrote, a single PUT whenever the first
 * repository was small, so a store that dropped the lock on every object written in parts still
 * read present. The run now asks about the smallest object it wrote, which went in one PUT unless
 * every object went in parts, and the largest, which went in parts if any object did:
 *
 *   - absent when either is absent, and that lowers the verdict;
 *   - present when both are present;
 *   - not-checked otherwise, since an answer for one path says nothing about the other.
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
	keys := retentionSamples(entries)
	if len(keys) == 0 {
		return dest.RetentionNotChecked, verdict
	}

	observed := dest.RetentionPresent
	for _, key := range keys {
		got, until, err := observer.ObserveRetention(ctx, key)
		switch got {
		case dest.RetentionPresent:
			r.log.Info("retention observed", "key", key, "until", until.Format(time.RFC3339))
		case dest.RetentionAbsent:
			// The earned negative, and the reason all of this exists. The bucket said it locks,
			// the write was accepted, and the object holds nothing. The objects cannot be
			// unwritten - the destination is create-only - so failing closed here can only mean
			// refusing to report a protection that is not there.
			r.log.Warn("the destination accepted this run and applied no retention to it",
				"key", key, "bucket_said", r.wormStatus.Details)
			observed = dest.RetentionAbsent
		default:
			// A refusal is not a no. On S3 this is the common case rather than the exotic one:
			// reading an object's retention needs s3:GetObjectRetention, and this product tells
			// operators to scope destination credentials create/put-only.
			r.log.Info("could not confirm the retention on this run's objects", "key", key, "err", err)
			if observed == dest.RetentionPresent {
				observed = dest.RetentionNotChecked
			}
		}
	}
	if observed == dest.RetentionAbsent {
		verdict = dest.VerdictNotImmutable
	}
	return observed, verdict
}

// retentionSamples is the keys observeRetention asks about: the smallest artifact the run wrote,
// which went in one PutObject unless every artifact went in parts, and the largest, which went in
// parts if any artifact did. One key when they are the same object, and none when the run wrote
// nothing.
func retentionSamples(entries []RepoEntry) []string {
	var smallest, largest *ArtifactInfo
	for i := range entries {
		for j := range entries[i].Artifacts {
			a := &entries[i].Artifacts[j]
			if smallest == nil || a.Size < smallest.Size {
				smallest = a
			}
			if largest == nil || a.Size > largest.Size {
				largest = a
			}
		}
	}
	switch {
	case smallest == nil:
		return nil
	case smallest.Key == largest.Key:
		return []string{smallest.Key}
	}
	return []string{smallest.Key, largest.Key}
}
