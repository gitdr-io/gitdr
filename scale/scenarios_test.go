//go:build scale

package scale

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/pipeline"
)

// The scenarios. Each is a pass/fail test, and on an engine with a known defect the check that
// demonstrates it fails and names it. Numbered as in README.md; 5, 7 and 9 are not built yet.

// simBase is simulated day 1: the UTC date the run started on, fixed once so that a run crossing
// midnight does not move its own days.
var simBase = time.Now().UTC().Truncate(24 * time.Hour)

// simDay is hour h of simulated day d. Retention dates are computed from it, so the days run
// forward from today and every lock lands in the future.
func simDay(d, hour int) time.Time {
	return simBase.Add(time.Duration(d-1)*24*time.Hour + time.Duration(hour)*time.Hour)
}

func newBucket(t *testing.T, parts ...string) string {
	t.Helper()
	name := h.bucketName(parts...)
	if err := h.needStack(t).newBucket(context.Background(), name); err != nil {
		t.Fatalf("harness: %v", err)
	}
	return name
}

func manifestDirOf(o *org) string { return path.Join(h.forge.host(), o.owner, "manifests") }

// copies tracks which repositories have a recorded copy that nothing has changed since, so a
// run that copies one of them again can be caught doing it.
type copies struct {
	o       *org
	current map[string]bool
}

func newCopies(o *org) *copies { return &copies{o: o, current: map[string]bool{}} }

func (c *copies) record(r *runResult) {
	for _, e := range r.entries() {
		if e.Status == pipeline.StatusSuccess {
			c.current[e.Slug] = true
		}
	}
}

func (c *copies) changed(names []string) {
	for _, n := range names {
		c.current[c.o.slug(n)] = false
	}
}

// recopies is the repositories r copied in full although their recorded copy was still current,
// and the bytes it sent again for them.
func (c *copies) recopies(r *runResult) (slugs []string, bytes int64) {
	for _, e := range r.entries() {
		if e.Status == pipeline.StatusSuccess && c.current[e.Slug] {
			slugs = append(slugs, e.Slug)
			for _, a := range e.Artifacts {
				bytes += a.Size
			}
		}
	}
	return slugs, bytes
}

func failures(r *runResult) []string {
	var out []string
	for _, e := range r.entries() {
		if e.Status == pipeline.StatusFailed {
			out = append(out, e.Slug+": "+e.Error)
		}
	}
	if len(out) == 0 && r.err != nil {
		out = append(out, r.err.Error())
	}
	return out
}

func sample(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, "; ")
	}
	return strings.Join(list[:n], "; ") + fmt.Sprintf("; and %d more", len(list)-n)
}

func copiedAll(sc *scenario, t *testing.T, r *runResult, want int) {
	t.Helper()
	sc.check(t, r.phase.Name+" copies every repository", r.err == nil && r.phase.Copied == want,
		fmt.Sprintf("copied %d of %d; %s", r.phase.Copied, want, sample(failures(r), 3)), "")
}

// 0. One object over 16 MiB over TLS.
//
// The floor under the other scenarios: an artifact or a manifest over 16 MiB, written the way the
// engine writes one. Once through the proxy, which splits the SDK's single chunk as AWS would take
// it, and once passed to MinIO as it came.
func TestScale0LargeObjectOverTLS(t *testing.T) {
	sc := h.begin(t, "s0", "0. one object over 16 MiB over TLS, written as the engine writes it")
	st := h.needStack(t)
	bucket := newBucket(t, "s0")
	ctx := context.Background()
	d, err := st.dest(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	payload := detBytes("s0", 20<<20)
	ret := dest.Retention{Mode: dest.RetentionCompliance, Until: time.Now().Add(24 * time.Hour)}

	_, err = d.PutImmutable(ctx, "objects/through-the-proxy.bin", bytes.NewReader(payload), int64(len(payload)), ret)
	sc.check(t, "a 20 MiB object is stored through the proxy, as AWS would store it", err == nil, fmt.Sprint(err), "")

	was, err := st.setRechunk(0)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer func() { _, _ = st.setRechunk(was) }()
	_, err = d.PutImmutable(ctx, "objects/straight-to-minio.bin", bytes.NewReader(payload), int64(len(payload)), ret)
	finding := ""
	if err != nil && strings.Contains(err.Error(), "chunk too big") {
		finding = "minio-tls-chunk"
	}
	sc.check(t, "a 20 MiB object is stored by MinIO over TLS", err == nil, fmt.Sprint(err), finding)
}

// 1. Day, same-day rerun, next day.
//
// A same-day rerun has nothing to copy, and the day after copies only what changed. Today the
// rerun records its skips without the refs the next day compares against, so the next day copies
// every repository in full.
func TestScale1DayCycle(t *testing.T) {
	sc := h.begin(t, "s1", "1. day, same-day rerun, next day: zero full re-copies")
	o := h.needOrg(t, &smallOrg, h.seedSmall)
	bucket := newBucket(t, "s1")
	track := newCopies(o)

	a := h.backup(t, sc, runSpec{name: "day 1", org: o, bucket: bucket, at: simDay(1, 1)})
	copiedAll(sc, t, a, len(o.names)-1) // all but the repository with no commits
	track.record(a)

	b := h.backup(t, sc, runSpec{name: "day 1 rerun", org: o, bucket: bucket, at: simDay(1, 13)})
	sc.check(t, "the same-day rerun copies nothing again", b.phase.Copied == 0,
		fmt.Sprintf("%d repositories copied again", b.phase.Copied), "")
	finding := ""
	if e := b.entry(o.slug(o.empty)); e != nil && e.Status == pipeline.StatusFailed {
		finding = "empty-repo-rerun"
	}
	sc.check(t, "the same-day rerun succeeds", b.err == nil && b.phase.Failed == 0, sample(failures(b), 3), finding)
	// Not the repository with no commits: it has no refs to record, and its day-1 entry, a skip as
	// empty, has no copiedAt to carry.
	var bare []string
	for _, e := range b.entries() {
		if e.Slug == o.slug(o.empty) {
			continue
		}
		if e.Status == pipeline.StatusSkipped && e.Reason == pipeline.ReasonResume && (len(e.Refs) == 0 || e.CopiedAt == nil) {
			bare = append(bare, e.Slug)
		}
	}
	sc.check(t, "a same-day skip records the refs and copiedAt of the copy it relies on", len(bare) == 0,
		fmt.Sprintf("%d skips record neither, so the next run has nothing to compare: %s", len(bare), sample(bare, 3)), "resume-trusts-objects")

	changed := h.changeDay(t, o, 2)
	track.changed(changed)
	c := h.backup(t, sc, runSpec{name: "day 2", org: o, bucket: bucket, at: simDay(2, 1)})
	again, sent := track.recopies(c)
	c.phase.Recopied, c.phase.BytesSentTwice = len(again), c.phase.BytesSentTwice+sent
	sc.check(t, "day 2 copies only the repositories that changed", len(again) == 0,
		fmt.Sprintf("%d of %d unchanged repositories copied in full again (%s sent twice), %d changed",
			len(again), len(o.names)-1-len(changed), size(sent), len(changed)), "resume-trusts-objects")
	missed := missing(c, o, changed)
	sc.check(t, "day 2 copies every repository that changed", len(missed) == 0, sample(missed, 3), "")
	track.record(c)

	// The control. Nothing ran twice on day 2, so day 3 reads a manifest written by copies, and
	// it should skip the unchanged. If this fails as well, the harness is measuring something else.
	changed = h.changeDay(t, o, 3)
	track.changed(changed)
	d := h.backup(t, sc, runSpec{name: "day 3", org: o, bucket: bucket, at: simDay(3, 1)})
	again, sent = track.recopies(d)
	d.phase.Recopied, d.phase.BytesSentTwice = len(again), d.phase.BytesSentTwice+sent
	sc.check(t, "control: day 3, after a day without a rerun, copies only what changed", len(again) == 0 && len(missing(d, o, changed)) == 0,
		fmt.Sprintf("%d unchanged repositories copied again: %s", len(again), sample(again, 3)), "")
}

func missing(r *runResult, o *org, names []string) []string {
	var out []string
	for _, n := range names {
		if e := r.entry(o.slug(n)); e == nil || e.Status != pipeline.StatusSuccess {
			out = append(out, o.slug(n))
		}
	}
	return out
}

// 2. The kill matrix.
//
// gitdr is stopped while a clone is in flight, while an upload is in flight, and just after a
// write landed, with SIGTERM and with SIGKILL, and then run again the same day. No repository may
// be counted as backed up unless a manifest records its copy.
func TestScale2KillMatrix(t *testing.T) {
	sc := h.begin(t, "s2", "2. kill matrix: stopped mid-clone, mid-upload and after a write, then a rerun")
	o := h.needOrg(t, &killOrg, h.seedKill)
	st, f := h.needStack(t), h.needForge(t)
	waitPastMidnight(t)
	// The sixth repository: with four at a time, some are finished by the time it starts.
	target := o.names[5]

	for _, kc := range []struct {
		name string
		sig  syscall.Signal
		at   string
	}{
		{"sigterm mid-clone", syscall.SIGTERM, "clone"},
		{"sigkill mid-clone", syscall.SIGKILL, "clone"},
		{"sigterm mid-upload", syscall.SIGTERM, "upload"},
		{"sigkill mid-upload", syscall.SIGKILL, "upload"},
		{"sigkill after the bundle landed", syscall.SIGKILL, "landed"},
	} {
		t.Run(slugify(kc.name), func(t *testing.T) {
			bucket := newBucket(t, "s2", slugify(kc.name))
			reached := make(chan struct{})
			var release func()
			switch kc.at {
			case "clone":
				hd := f.holdFetch(o.slug(target))
				reached, release = hd.reached, hd.Release
			default:
				action := "hold-before"
				if kc.at == "landed" {
					action = "hold-after"
				}
				id, err := st.addFault(s3Fault{Op: "PutObject", Bucket: bucket, KeyRegex: "/" + target + `\.bundle$`, Action: action})
				if err != nil {
					t.Fatalf("harness: %v", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				go func() {
					defer cancel()
					if st.waitHeld(ctx, id) == nil {
						close(reached)
					}
				}()
				release = func() { _ = st.clearFaults() }
			}

			p := h.startProcess(t, sc, runSpec{name: kc.name, org: o, bucket: bucket, process: true})
			select {
			case <-reached:
			case <-p.done:
				release()
				p.wait(t, time.Minute)
				t.Fatalf("harness: gitdr finished before it reached %s", target)
			case <-time.After(10 * time.Minute):
				release()
				p.stop(syscall.SIGKILL)
				p.wait(t, time.Minute)
				t.Fatalf("harness: gitdr never reached %s", target)
			}
			p.stop(kc.sig)
			stoppedInTime := p.exited(time.Minute)
			release()
			p.wait(t, time.Minute)

			dir := manifestDirOf(o)
			if kc.sig == syscall.SIGTERM {
				sc.check(t, kc.name+": gitdr exits within a minute of SIGTERM", stoppedInTime,
					"it was still running a minute later, waiting on a git process the signal did not reach", "stop-waits-for-git")
				filed := h.bucketManifests(t, bucket, dir)
				sc.check(t, kc.name+": the stopped run filed a manifest", len(filed) > 0,
					"no manifest under "+dir+" after SIGTERM, so the copies it finished are recorded nowhere", "stopped-run-no-manifest")
			}

			rerun := h.backup(t, sc, runSpec{name: kc.name + ", rerun", org: o, bucket: bucket, process: true})
			recorded := map[string]bool{}
			for _, m := range h.bucketManifests(t, bucket, dir) {
				for _, e := range m.Repos {
					if e.Status == pipeline.StatusSuccess && slices.ContainsFunc(e.Artifacts, func(a pipeline.ArtifactInfo) bool { return a.Kind == "bundle" }) {
						recorded[e.Slug] = true
					}
				}
			}
			date := time.Now().UTC().Format("2006-01-02")
			var unrecorded, partial []string
			for _, e := range rerun.entries() {
				if e.Status != pipeline.StatusSkipped || e.Reason != pipeline.ReasonResume {
					continue
				}
				if !recorded[e.Slug] {
					unrecorded = append(unrecorded, e.Slug)
				}
				name := path.Base(e.Slug)
				keys := h.keysUnder(t, bucket, path.Join(h.forge.host(), e.Slug, date)+"/")
				for _, suffix := range []string{".bundle", ".meta.json", ".sha256"} {
					if !slices.ContainsFunc(keys, func(k string) bool { return path.Base(k) == name+suffix }) {
						partial = append(partial, e.Slug+" has no "+name+suffix)
					}
				}
			}
			sc.check(t, kc.name+": the rerun counts no repository without a recorded copy", len(unrecorded) == 0,
				fmt.Sprintf("%d repositories reported %q with no manifest recording a copy: %s", len(unrecorded), pipeline.ReasonResume, sample(unrecorded, 4)),
				"resume-trusts-objects")
			sc.check(t, kc.name+": the rerun counts no partial copy as backed up", len(partial) == 0,
				sample(partial, 4), "resume-trusts-objects")
		})
	}
}

// waitPastMidnight keeps a same-day scenario from straddling a UTC date.
func waitPastMidnight(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	next := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	if left := next.Sub(now); left < 10*time.Minute {
		t.Logf("waiting %s for the UTC date to turn, so the scenario stays inside one day", left.Round(time.Second))
		time.Sleep(left + 30*time.Second)
	}
}

// 3. Refs.
//
// Repositories with a hundred thousand refs, half of them pull requests, which `git clone
// --mirror` brings. Unchanged the next day, so the next day copies none of them.
func TestScale3Refs(t *testing.T) {
	sc := h.begin(t, "s3", "3. refs: manifest size and the next day's skips with 100k-ref repositories")
	o := h.needOrg(t, &refsOrg, h.seedRefs)
	bucket := newBucket(t, "s3")
	track := newCopies(o)

	a := h.backup(t, sc, runSpec{name: "day 1", org: o, bucket: bucket, at: simDay(1, 1)})
	copiedAll(sc, t, a, len(o.names))
	track.record(a)
	sc.note(t, "the day-1 manifest is %s for %d repositories of %d refs", size(a.phase.ManifestBytes), len(o.names), h.prof.RefsPerRepo)

	b := h.backup(t, sc, runSpec{name: "day 2", org: o, bucket: bucket, at: simDay(2, 1)})
	again, sent := track.recopies(b)
	b.phase.Recopied, b.phase.BytesSentTwice = len(again), b.phase.BytesSentTwice+sent
	finding := ""
	if a.phase.ManifestBytes > 32<<20 {
		finding = "manifest-read-cap"
	}
	sc.check(t, "day 2 copies none of the unchanged repositories", len(again) == 0,
		fmt.Sprintf("%d of %d unchanged repositories copied again (%s sent twice); the manifest they would be compared with is %s",
			len(again), len(o.names), size(sent), size(a.phase.ManifestBytes)), finding)
}

// 4. Big.
//
// A pack past 5 GiB and an LFS archive past 5 GiB, through a store that refuses a single PUT that
// large, as AWS does. Behind SCALE_BIG=1.
func TestScale4Big(t *testing.T) {
	sc := h.begin(t, "s4", "4. big: a 6 GiB pack and 8 GiB of LFS through AWS's single-PUT limit")
	if !h.prof.Big {
		t.Skip("set SCALE_BIG=1 for the 6 GiB pack and the 8 GiB of LFS; it needs about 45 GiB of free disk")
	}
	o := h.needOrg(t, &bigOrg, h.seedBig)
	bucket := newBucket(t, "s4")
	r := h.backup(t, sc, runSpec{name: "one run", org: o, bucket: bucket, process: true, timeout: 4 * time.Hour})
	var problems []string
	finding := ""
	for _, n := range o.names {
		e := r.entry(o.slug(n))
		switch {
		case e == nil:
			problems = append(problems, n+" is not in the manifest")
		case e.Status != pipeline.StatusSuccess:
			problems = append(problems, n+": "+e.Error)
			switch {
			case strings.Contains(e.Error, "EntityTooLarge"):
				finding = "single-put-limit"
			case r.phase.OOMKills > 0 || r.phase.OOMKilled:
				finding = "git-memory-unbounded"
			}
		}
	}
	if r.manifest == nil {
		problems = append(problems, "no manifest: "+fmt.Sprint(r.err))
	}
	sc.check(t, "every repository is stored, the ones past 5 GiB included", len(problems) == 0, sample(problems, 2), finding)
	sc.note(t, "peak scratch %s, peak resident set %s", size(r.phase.PeakScratchBytes), size(r.phase.MaxRSSBytes))
	if h.prof.Image != "" {
		p := r.phase
		sc.note(t, "%s", memoryNote(p))
		sc.check(t, "nothing is killed for memory under a "+h.prof.MemoryLimit+" limit",
			p.MemoryLimitBytes > 0 && p.OOMKills == 0 && !p.OOMKilled,
			fmt.Sprintf("the cgroup's OOM killer killed %d processes; the container itself OOMKilled: %v", p.OOMKills, p.OOMKilled),
			"git-memory-unbounded")
		// Not memory.peak: it counts the page cache, which a run writing this much fills to the
		// limit whatever git holds. What the processes hold is their anonymous memory and the file
		// pages they map, and the bound is 3.5 GiB of a 4 GiB limit.
		ceiling := p.MemoryLimitBytes / 8 * 7
		sc.check(t, "the processes hold at most "+size(ceiling)+" at once",
			p.MemoryLimitBytes > 0 && p.MaxResidentBytes <= ceiling,
			fmt.Sprintf("anonymous memory and mapped file pages reached %s (anon %s, mapped %s)",
				size(p.MaxResidentBytes), size(p.MaxAnonBytes), size(p.MaxFileMappedBytes)),
			"git-memory-unbounded")
	}
}

// 6. A rate limit.
//
// The installation's budget runs out partway through the run, and GitHub answers 403 until its
// window resets. The run waits and finishes.
func TestScale6RateLimit(t *testing.T) {
	sc := h.begin(t, "s6", "6. a GitHub rate limit spent mid-run is waited out")
	o := h.needOrg(t, &rateOrg, h.seedRate)
	f := h.needForge(t)
	f.setRateLimit(o.install, h.prof.RateLimit, h.prof.rateWindow)
	defer f.setRateLimit(o.install, 0, 0)
	bucket := newBucket(t, "s6")
	sc.note(t, "budget %d REST requests per %s for %d repositories", h.prof.RateLimit, h.prof.rateWindow, len(o.names))

	r := h.backup(t, sc, runSpec{name: "one run", org: o, bucket: bucket, timeout: 30 * time.Minute})
	sc.check(t, "the budget ran out during the run", r.phase.APIBudgetSpent > 0,
		"the budget never ran out, so this run proved nothing about rate limits", "")
	sc.note(t, "the budget ran out %d times and the forge refused %d requests", r.phase.APIBudgetSpent, r.phase.APIRateLimited)
	var limited []string
	for _, e := range r.entries() {
		if e.Status == pipeline.StatusFailed && strings.Contains(strings.ToLower(e.Error), "rate limit") {
			limited = append(limited, e.Slug+": "+e.Error)
		}
	}
	finding := ""
	if len(limited) > 0 {
		finding = "rate-limit-not-waited"
	}
	sc.check(t, "every repository succeeds", r.err == nil && r.phase.Failed == 0,
		fmt.Sprintf("%d of %d failed on the limit: %s", len(limited), len(o.names), sample(limited, 2)), finding)
}

// 8. Organisation run, single-repository run, organisation run.
//
// The single run files its manifest beside the organisation's. Nothing changes in between, so
// the third run copies nothing.
func TestScale8OrgSingleOrg(t *testing.T) {
	sc := h.begin(t, "s8", "8. organisation run, single-repository run, organisation run")
	o := h.needOrg(t, &smallOrg, h.seedSmall)
	bucket := newBucket(t, "s8")
	track := newCopies(o)

	a := h.backup(t, sc, runSpec{name: "day 1 organisation", org: o, bucket: bucket, at: simDay(1, 1)})
	copiedAll(sc, t, a, len(o.names)-1)
	track.record(a)

	single := o.slug(o.names[0])
	b := h.backup(t, sc, runSpec{name: "day 2 one repository", org: o, bucket: bucket, repo: single, at: simDay(2, 1)})
	e := b.entry(single)
	sc.check(t, "the single-repository run skips its unchanged repository",
		e != nil && e.Status == pipeline.StatusSkipped && strings.HasPrefix(e.Reason, pipeline.ReasonUnchanged),
		fmt.Sprintf("entry %+v", e), "")

	c := h.backup(t, sc, runSpec{name: "day 3 organisation", org: o, bucket: bucket, at: simDay(3, 1)})
	again, sent := track.recopies(c)
	c.phase.Recopied, c.phase.BytesSentTwice = len(again), c.phase.BytesSentTwice+sent
	sc.check(t, "day 3 copies no unchanged repository", len(again) == 0,
		fmt.Sprintf("%d of %d unchanged repositories copied in full again (%s sent twice) after one single-repository run",
			len(again), len(o.names)-1, size(sent)), "newest-manifest-only")
}
