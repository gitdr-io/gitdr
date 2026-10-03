package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/logging"
	"gitdr.io/gitdr/internal/pipeline"
)

// eventLines returns the lines of a JSON log whose msg is msg, with their time taken out, which is
// the one field that changes from run to run.
func eventLines(t *testing.T, log string, msg string) []string {
	t.Helper()
	stamp := regexp.MustCompile(`^\{"time":"[^"]*",`)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		if head.Msg == msg {
			out = append(out, stamp.ReplaceAllString(line, "{"))
		}
	}
	return out
}

// A backup reports each repository as it finishes, in a line a caller can read without waiting for
// the run to end, and the lines say what the manifest then records.
//
// Nothing was said before the engine exited: a caller had the manifest or nothing, so a run of a
// few thousand repositories looked stalled until it ended, and one that was stopped left no trace
// of the copies it had finished.
func TestBackupReportsEachRepositoryAsItFinishes(t *testing.T) {
	t.Chdir(t.TempDir())
	var logged bytes.Buffer
	repos := append(slugRepos("github.com", initFixtureRepo(t), "octo/copied", "octo/broken"),
		slugRepos("github.com", initEmptyRepo(t), "octo/empty")...)
	src := &failingMetadata{
		fixtureSource: &fixtureSource{repos: repos},
		fails:         "broken",
		err:           errors.New("github: issues: GET .../issues: 403 Resource not accessible by integration"),
	}
	cfg := testConfig()
	cfg.Source.Repo = ""
	res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: newMemDest(true), Git: gitexec.New(nil),
		SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
		// The logger the CLI builds, so the lines are the ones on gitdr's stderr.
		Logger: logging.New("info", "json", &logged),
	})
	if err == nil || res == nil {
		t.Fatalf("the run with a failed repository: %v", err)
	}

	if got := eventLines(t, logged.String(), "repos selected"); !slices.Equal(got, []string{`{"level":"INFO","msg":"repos selected","count":3}`}) {
		t.Errorf("repos selected: %q", got)
	}

	finished := eventLines(t, logged.String(), "repo finished")
	if len(finished) != len(res.Manifest.Repos) {
		t.Fatalf("%d repo finished lines for %d repositories:\n%s", len(finished), len(res.Manifest.Repos), strings.Join(finished, "\n"))
	}
	for _, e := range res.Manifest.Repos {
		var line string
		for _, l := range finished {
			if strings.Contains(l, `"slug":"`+e.Slug+`"`) {
				line = l
			}
		}
		var got struct {
			Slug, Status, Reason, Error string
			Artifacts                   []struct {
				Kind, Key string
				Size      int64
			}
			CopiedAt *time.Time
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("%s: no readable repo finished line: %v", e.Slug, err)
		}
		if got.Status != e.Status || got.Reason != e.Reason || got.Error != e.Error {
			t.Errorf("%s: the line says %s %q %q, the manifest %s %q %q", e.Slug, got.Status, got.Reason, got.Error, e.Status, e.Reason, e.Error)
		}
		if len(got.Artifacts) != len(e.Artifacts) {
			t.Fatalf("%s: the line names %d artifacts, the manifest %d", e.Slug, len(got.Artifacts), len(e.Artifacts))
		}
		for i, a := range e.Artifacts {
			if g := got.Artifacts[i]; g.Kind != a.Kind || g.Key != a.Key || g.Size != a.Size {
				t.Errorf("%s: artifact %d is %+v in the line, %s %s %d in the manifest", e.Slug, i, g, a.Kind, a.Key, a.Size)
			}
		}
		if (got.CopiedAt == nil) != (e.CopiedAt == nil) || (got.CopiedAt != nil && !got.CopiedAt.Equal(*e.CopiedAt)) {
			t.Errorf("%s: copiedAt %v in the line, %v in the manifest", e.Slug, got.CopiedAt, e.CopiedAt)
		}
	}

	// The shape, pinned: what a caller parses, field for field and in this order. reason, error and
	// copiedAt are there only when the manifest's entry has them, as in the manifest.
	const golden = `{"level":"INFO","msg":"repo finished","slug":"octo/empty","status":"skipped","reason":"repository has no commits","artifacts":[{"kind":"meta","key":"github.com/octo/empty/2026-06-13/empty.meta.json","size":41}]}`
	if !slices.Contains(finished, golden) {
		t.Errorf("no line is\n%s\nthe lines are\n%s", golden, strings.Join(finished, "\n"))
	}
	for _, l := range finished {
		keys := regexp.MustCompile(`"(\w+)":`).FindAllStringSubmatch(l, -1)
		var top []string
		for _, k := range keys {
			if k[1] != "kind" && k[1] != "key" && k[1] != "size" {
				top = append(top, k[1])
			}
		}
		if !slices.ContainsFunc([][]string{
			{"level", "msg", "slug", "status", "artifacts", "copiedAt"},
			{"level", "msg", "slug", "status", "reason", "artifacts"},
			{"level", "msg", "slug", "status", "error", "artifacts"},
		}, func(shape []string) bool { return slices.Equal(shape, top) }) {
			t.Errorf("a line with the fields %v: %s", top, l)
		}
	}
}

// A stopped run reports every repository it selected, the one it stopped in the middle of and the
// ones it never started among them, so a caller counting lines against "repos selected" sees the
// run end rather than stall.
func TestAStoppedRunReportsEveryRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	var logged bytes.Buffer
	repos := slugRepos("github.com", initFixtureRepo(t), "octo/r1", "octo/r2", "octo/r3")
	cfg := testConfig()
	cfg.Source.Repo = ""
	cfg.Backup.Concurrency = 1
	res, _ := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: &stoppingSource{fixtureSource: &fixtureSource{repos: repos}, at: "r2", stop: stop},
		Dest: newMemDest(true), Git: gitexec.New(nil), SigningKey: testSigner(t), ToolVersion: "test",
		Now: fixedClock(), Logger: logging.New("info", "json", &logged),
	})
	if res == nil {
		t.Fatal("the stopped run returned nothing")
	}
	finished := eventLines(t, logged.String(), "repo finished")
	if len(finished) != 3 {
		t.Fatalf("%d repo finished lines for 3 repositories:\n%s", len(finished), strings.Join(finished, "\n"))
	}
	const neverStarted = `{"level":"INFO","msg":"repo finished","slug":"octo/r3","status":"failed","error":"stopped before it finished: terminated signal received","artifacts":[]}`
	if !slices.Contains(finished, neverStarted) {
		t.Errorf("no line is\n%s\nthe lines are\n%s", neverStarted, strings.Join(finished, "\n"))
	}
}
