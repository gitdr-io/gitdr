package pipeline_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

// How large a manifest every reader takes, and what a reader says about one it will not.

// initManyRefsRepo is the fixture repository with refs extra branches beside main, all at main's
// commit, each with a name of about 600 characters in parts short enough for any filesystem. Long
// names make a large manifest out of few refs, and git's time goes by the ref. Written straight
// into packed-refs.
func initManyRefsRepo(t *testing.T, refs int) string {
	t.Helper()
	dir := initFixtureRepo(t)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	oid := strings.TrimSpace(string(out))
	tail := strings.Repeat("x", 190)
	names := make([]string, refs)
	for i := range names {
		names[i] = fmt.Sprintf("refs/heads/scale/%06d/%s/%s/%s", i, tail, tail, tail)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for _, n := range names {
		b.WriteString(oid + " " + n + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "packed-refs"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A manifest past the old 32 MiB cap is read by the next run, by restore -manifest and by a drill.
//
// The cap was 32 MiB, about 390,000 refs, and `--mirror` brings every refs/pull/*. Five
// repositories, four of them with 16,000 long refs, make a manifest of about 40 MiB. Past the cap
// the next run read nothing and copied all five again, and restore -manifest and drill refused the
// very manifest that recorded the copies. The one they restore here is the small one: what is
// under test is that they read the manifest, and a checkout of 16,000 branches is not.
func TestAManifestPastTheOldCapIsRead(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	repos := append(slugRepos("github.com", initFixtureRepo(t), "octo/r1"),
		slugRepos("github.com", initManyRefsRepo(t, 16000), "octo/r2", "octo/r3", "octo/r4", "octo/r5")...)
	day1 := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)

	first := backupAt(t, md, signer, day1, repos)
	md.mu.Lock()
	size := len(md.objs[first.ManifestKey])
	md.mu.Unlock()
	if size <= 32<<20 || size > 128<<20 {
		t.Fatalf("the manifest is %d bytes; this test needs one between 32 and 128 MiB", size)
	}

	next := backupAt(t, md, signer, day1.AddDate(0, 0, 1), repos)
	for _, e := range next.Manifest.Repos {
		if e.Status != pipeline.StatusSkipped || e.Reason != pipeline.ReasonUnchanged+" 2026-06-13" {
			t.Errorf("%s: %s %q, want skipped as unchanged: the next run did not read a %d-byte manifest", e.Slug, e.Status, e.Reason, size)
		}
	}

	if _, err := pipeline.Restore(ctx, restoreDeps(md, pub), pipeline.RestoreRequest{
		ManifestKey: first.ManifestKey, Owner: "octo", Name: "r1", OutDir: filepath.Join(t.TempDir(), "r1"),
	}); err != nil {
		t.Errorf("restore -manifest: %v", err)
	}

	drilled, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: md, Git: gitexec.New(nil), PublicKey: pub, ToolVersion: "test",
		Now: func() time.Time { return day1.Add(time.Hour) },
	}, pipeline.DrillRequest{ManifestKey: first.ManifestKey, Sample: 1, WorkDir: t.TempDir()})
	if err != nil || drilled.Report.Status != pipeline.StatusSuccess || drilled.Report.Drilled != 1 {
		t.Errorf("drill of a %d-byte manifest: %v", size, err)
	}
}

// largeListing is a memDest whose listing says each manifest is size bytes, so a reader that
// believes the listing meets a manifest past the cap without one being stored.
type largeListing struct {
	*memDest
	size int64
}

func (l *largeListing) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	objs, err := l.memDest.List(ctx, prefix)
	for i := range objs {
		if strings.HasSuffix(objs[i].Key, ".manifest.json") {
			objs[i].Size = l.size
		}
	}
	return objs, err
}

// A manifest past the cap is refused before a byte of it is fetched, and every reader that meets
// one says how large it is. The next run warns and copies what only it recorded. A restore by
// date says the manifest is too large, where it used to suggest a rotated signing key.
func TestAManifestPastTheCapIsRefusedByItsSize(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	md := newMemDest(true)
	pub, signer := drillKeys(t)
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/hello")
	day1 := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	first := backupAt(t, md, signer, day1, repos)
	large := &largeListing{memDest: md, size: 200 << 20}
	const says = "is 200 MiB, larger than the 128 MiB this engine reads"

	t.Run("the next run", func(t *testing.T) {
		var logged bytes.Buffer
		md.storeAt(day1.AddDate(0, 0, 1))
		res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
			Config: testConfig(), Source: &fixtureSource{repos: repos}, Dest: large, Git: gitexec.New(nil),
			SigningKey: signer, ToolVersion: "test", Logger: slog.New(slog.NewTextHandler(&logged, nil)),
			Now: func() time.Time { return day1.AddDate(0, 0, 1) },
		})
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		if e := res.Manifest.Repos[0]; e.Status != pipeline.StatusSuccess {
			t.Errorf("octo/hello = %s %q; with its only manifest refused, want it copied", e.Status, e.Reason)
		}
		var warned bool
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, first.ManifestKey) && strings.Contains(line, says) {
				warned = true
			}
		}
		if !warned {
			t.Errorf("no warning names %s and its size:\n%s", first.ManifestKey, logged.String())
		}
	})

	t.Run("a restore by date", func(t *testing.T) {
		_, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: large, Git: gitexec.New(nil), PublicKey: pub}, pipeline.RestoreRequest{
			Host: "github.com", Owner: "octo", Name: "hello", Date: "2026-06-13", OutDir: filepath.Join(t.TempDir(), "r"),
		})
		if err == nil || !strings.Contains(err.Error(), says) || strings.Contains(err.Error(), "rotated") {
			t.Errorf("err = %v\nwant it to say the manifest %s, and nothing about a rotated key", err, says)
		}
	})
}
