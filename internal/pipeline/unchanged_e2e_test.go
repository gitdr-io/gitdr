package pipeline_test

import (
	"context"
	"crypto/ed25519"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// Two runs, a real repository, a real destination.
//
// The unit tests cover the decision. This covers the thing the decision was written for: that
// running gitdr twice against an unchanged repository writes the history once, and that a
// commit in between makes it write again. Nothing short of running it twice proves that,
// because the failure mode is a wiring mistake — a ref map recorded in the wrong place, a
// previous manifest that is never read, a skip that forgets to carry the refs forward — and
// every one of those passes a unit test.
func TestASecondRunWritesNothingWhenNothingChanged(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	signer := testSigner(t)

	day := 24 * time.Hour
	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) func() time.Time {
		return func() time.Time { return start.Add(d) }
	}
	run := func(clock func() time.Time) *pipeline.BackupResult {
		t.Helper()
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test", Now: clock,
		})
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		return res
	}

	first := run(at(0))
	if got := first.Manifest.Repos[0].Status; got != pipeline.StatusSuccess {
		t.Fatalf("first run status = %s, want success", got)
	}
	if len(first.Manifest.Repos[0].Refs) == 0 {
		t.Fatal("the first run recorded no refs, so the second has nothing to compare against")
	}
	wroteFirst := bundleCount(md)
	if wroteFirst == 0 {
		t.Fatal("the first run wrote no bundle")
	}

	// A day later, nothing has changed.
	second := run(at(day))
	entry := second.Manifest.Repos[0]
	if entry.Status != pipeline.StatusSkipped {
		t.Errorf("second run status = %s, want skipped (reason %q)", entry.Status, entry.Reason)
	}
	if !strings.Contains(entry.Reason, "unchanged") {
		t.Errorf("reason = %q, want it to say the repository was unchanged", entry.Reason)
	}
	if got := bundleCount(md); got != wroteFirst {
		t.Errorf("the second run wrote %d bundles, want 0 more than the first's %d", got-wroteFirst, wroteFirst)
	}

	// And the skip carried the refs forward, or the third run would copy in full.
	if len(entry.Refs) == 0 {
		t.Error("a skipped repository dropped its refs; the next run has nothing to compare against")
	}

	// Two days later, still nothing changed. This is the one that catches a skip that forgets
	// to carry its refs: without that, run three copies in full and the saving is every other
	// day rather than every day.
	third := run(at(2 * day))
	if got := third.Manifest.Repos[0].Status; got != pipeline.StatusSkipped {
		t.Errorf("third run status = %s, want skipped", got)
	}
	if got := bundleCount(md); got != wroteFirst {
		t.Errorf("the third run wrote again: %d bundles, want %d", got, wroteFirst)
	}
}

func TestACommitMakesItCopyAgain(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	signer := testSigner(t)

	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	run := func(d time.Duration) *pipeline.BackupResult {
		t.Helper()
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test",
			Now: func() time.Time { return start.Add(d) },
		})
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		return res
	}

	run(0)
	before := bundleCount(md)

	commit(t, repoDir, "second.txt", "more")

	after := run(24 * time.Hour)
	if got := after.Manifest.Repos[0].Status; got != pipeline.StatusSuccess {
		t.Fatalf("status = %s, want success: a commit landed and it was not copied", got)
	}
	if got := bundleCount(md); got <= before {
		t.Errorf("bundles = %d, want more than %d: the new commit was not written", got, before)
	}
	// And the new refs replaced the old, not the other way round.
	if len(after.Manifest.Repos[0].Refs) == 0 {
		t.Error("the copy recorded no refs")
	}
}

func bundleCount(md *memDest) int {
	md.mu.Lock()
	defer md.mu.Unlock()
	n := 0
	for key := range md.objs {
		if strings.HasSuffix(key, ".bundle") {
			n++
		}
	}
	return n
}

func commit(t *testing.T, dir, name, body string) {
	t.Helper()
	for _, args := range [][]string{
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		_ = exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
	}
	if err := exec.Command("sh", "-c",
		"cd "+dir+" && echo "+body+" > "+name+" && git add . && git commit -q -m more").Run(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func testSigner(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// The hazard, end to end: a repository that never changes must not be skipped until its only
// copy has aged out of its object lock.
//
// Object lock protects an object until its retain-until date and not one second longer. A
// repository that is skipped forever ends up with a copy that expires and nothing to replace
// it, which turns a saving into data loss — the exact outcome this product exists to prevent.
//
// The unit test covers the arithmetic. This covers the wiring, and the wiring is where it went
// wrong: an earlier version recorded the *skip's* time rather than the copy's, so every skip
// restarted the clock and the refresh could never fire. It passed every unit test.
func TestAnUnchangingRepositoryIsCopiedAgainBeforeItsLockExpires(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	signer := testSigner(t)

	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	run := func(d time.Duration) *pipeline.BackupResult {
		t.Helper()
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test",
			Now: func() time.Time { return start.Add(d) },
		})
		if err != nil {
			t.Fatalf("backup at day %d: %v", int(d.Hours()/24), err)
		}
		return res
	}

	run(0)
	if bundleCount(md) != 1 {
		t.Fatalf("first run wrote %d bundles, want 1", bundleCount(md))
	}

	// Nothing ever changes. Run it every day for two months.
	statuses := map[string]int{}
	for d := 1; d <= 60; d++ {
		statuses[run(time.Duration(d) * day).Manifest.Repos[0].Status]++
	}

	if statuses[pipeline.StatusSkipped] == 0 {
		t.Error("nothing was ever skipped, so none of this is doing anything")
	}
	// The cap is thirty days, so across sixty it must have refreshed at least once.
	if statuses[pipeline.StatusSuccess] == 0 {
		t.Error("an unchanging repository was never copied again in sixty days; its lock will expire and leave nothing")
	}

	// Refreshed on a schedule tied to the retention, not on a number picked here. The default
	// retention is 30 days and a copy is refreshed after a third of it, so over sixty days
	// that is about six times — and the assertion is written from that arithmetic rather than
	// from whatever the first run happened to produce, which would be a test that passes
	// because it was told what the answer was.
	const retentionDays = 30 // config.Default()
	want := 60 / (retentionDays / refreshFloorForTest)
	if got := statuses[pipeline.StatusSuccess]; got < want-1 || got > want+1 {
		t.Errorf("copied %d times in sixty unchanged days, want about %d (retention %dd, refreshed after a third)",
			got, want, retentionDays)
	}
	t.Logf("over 60 unchanged days: %d skipped, %d refreshed", statuses[pipeline.StatusSkipped], statuses[pipeline.StatusSuccess])
}

// Mirrors refreshFloor in unchanged.go. Duplicated rather than exported: this test asserts the
// behaviour an operator gets, and reading the constant out of the code under test would make
// it agree with any value that constant ever takes.
const refreshFloorForTest = 3

// A failed copy's refs are not evidence, and trusting them would skip the retry.
//
// The mutation that exposed this gap: relaxing loadPrevious to accept any entry with refs,
// including failed ones. Nothing failed, because nothing had ever exercised a run that failed
// and then ran again.
func TestAFailedCopyIsRetriedRatherThanSkipped(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()

	repoDir := initFixtureRepo(t)
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main",
	}}}
	md := newMemDest(true)
	signer := testSigner(t)

	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	run := func(d time.Duration) (*pipeline.BackupResult, error) {
		return pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test",
			Now: func() time.Time { return start.Add(d) },
		})
	}

	// A first run that fails: the source cannot be cloned.
	src.repos[0].CloneURL = repoDir + "-does-not-exist"
	failed, err := run(0)
	if err == nil {
		t.Fatal("expected the first run to fail")
	}
	if failed == nil || failed.Manifest == nil {
		t.Fatal("a failed run wrote no manifest, so there is nothing for the next one to read")
	}
	if got := failed.Manifest.Repos[0].Status; got != pipeline.StatusFailed {
		t.Fatalf("status = %s, want failed", got)
	}

	// The source comes back. The next run must copy it, not skip it.
	src.repos[0].CloneURL = repoDir
	second, err := run(24 * time.Hour)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := second.Manifest.Repos[0].Status; got != pipeline.StatusSuccess {
		t.Errorf("status = %s, want success: the retry was skipped and the repository has no copy at all", got)
	}
	if bundleCount(md) == 0 {
		t.Error("nothing was ever written")
	}
}
