package pipeline_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/pipeline"
)

// GitHub refuses an installation token once its hour is up, and `git lfs fetch` sends the header
// it was started with until it ends, so a fetch that took longer than its token failed part way
// with a 401. The clone does not have the problem, because it authenticates before its pack
// streams. These run a real fetch against a server whose token runs out once the first of two LFS
// objects has been sent.

// initTwoObjectLFSFixture is initLFSFixture with a second LFS object, so a fetch can be cut between
// the two. It returns the repository and the content of each object.
func initTwoObjectLFSFixture(t *testing.T) (dir string, contents [][]byte) {
	t.Helper()
	dir, first := initLFSFixture(t)
	second := bytes.Repeat([]byte("SECOND-LFS-PAYLOAD-"), 2048)
	if err := os.WriteFile(filepath.Join(dir, "second.bin"), second, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "second.bin"}, {"commit", "-q", "-m", "a second lfs file"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir, [][]byte{first, second}
}

// newLFSTokenRun is an authRun over the two-object fixture, whose token runs out once the first
// object has been sent: the request after it that carries the token is refused, and then
// whenTokenRunsOut runs.
func newLFSTokenRun(t *testing.T, firstToken string, whenTokenRunsOut func(a *authRun)) (*authRun, [][]byte) {
	t.Helper()
	requireLFS(t)
	isolateGit(t) // before the fixture, which runs git-lfs too
	workdir, contents := initTwoObjectLFSFixture(t)
	a := newAuthRun(t, workdir, filepath.Join(workdir, ".git", "lfs", "objects"), "lfsrepo", firstToken)
	a.server.mu.Lock()
	a.server.expire, a.server.expireAfter = basicAuth(firstToken), 1
	a.server.onExpire = func() { whenTokenRunsOut(a) }
	a.server.mu.Unlock()
	return a, contents
}

// lfsFetches counts the `git lfs fetch` commands the run started. The log has every git command's
// arguments.
func (a *authRun) lfsFetches(t *testing.T) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(a.logs.String()), "\n") {
		var l struct {
			Msg  string   `json:"msg"`
			Args []string `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		if l.Msg == "git" && slices.Equal(l.Args, []string{"lfs", "fetch", "--all"}) {
			n++
		}
	}
	return n
}

// archivedLFS lists, sorted, the LFS objects in the archive stored at key, and every other entry in
// it that is not a directory of lfs/objects.
func archivedLFS(t *testing.T, md *memDest, key string) (oids, others []string) {
	t.Helper()
	md.mu.Lock()
	b, ok := md.objs[key]
	md.mu.Unlock()
	if !ok {
		t.Fatalf("no LFS archive stored at %s", key)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the LFS archive: %v", err)
		}
		switch {
		case !strings.HasPrefix(hdr.Name, "objects/"):
			others = append(others, hdr.Name)
		case hdr.Typeflag == tar.TypeReg:
			oids = append(oids, path.Base(hdr.Name))
		}
	}
	slices.Sort(oids)
	return oids, others
}

// A fetch that outlives its token completes. The first attempt fetches one object before the token
// runs out, and the fetch runs again with the token the file holds by then, for what is still
// missing.
func TestALongLFSFetchSurvivesItsToken(t *testing.T) {
	first, second := newToken(t), newToken(t)
	// By the time the token runs out the caller has replaced the file, as SPEC §3 asks, and GitHub
	// takes only the new token.
	a, contents := newLFSTokenRun(t, first, func(a *authRun) {
		putToken(t, a.tokenPath, second)
		a.server.expect(basicAuth(second))
	})

	res, err := a.backup(t, true)
	if res == nil || res.Manifest == nil {
		t.Fatalf("no manifest: %v", err)
	}
	if e := res.Manifest.Repos[0]; err != nil || e.Status != pipeline.StatusSuccess {
		t.Fatalf("octo/lfsrepo = %s %q (%v), want copied", e.Status, e.Error, err)
	}

	var ranOut bool
	var after []seenGitRequest
	for _, r := range a.server.requests() {
		switch r.auth {
		case basicAuth(first):
			ranOut = ranOut || (r.status == http.StatusUnauthorized && strings.HasPrefix(r.path, "/lfs-objects/"))
		case basicAuth(second):
			after = append(after, r)
		}
	}
	if !ranOut {
		t.Fatalf("the token never ran out during the fetch: %+v", a.server.requests())
	}
	assertCarried(t, "the fetch run again", after, basicAuth(second))
	if n := a.lfsFetches(t); n != 2 {
		t.Errorf("%d LFS fetches, want 2: the one the token ran out in, and the one that finished", n)
	}

	var want []string
	for _, c := range contents {
		sum := sha256.Sum256(c)
		want = append(want, hex.EncodeToString(sum[:]))
	}
	slices.Sort(want)
	got, others := archivedLFS(t, a.dest, "github.com/octo/lfsrepo/2026-06-13/lfsrepo.lfs.tar")
	if !slices.Equal(got, want) || len(others) != 0 {
		t.Errorf("the LFS archive holds %v and %v, want both objects %v and nothing else", got, others, want)
	}
	a.assertTokensStayOnTheWire(t, res, first, second)
}

// git-lfs writes each fatal error, a server error on a batch say, to a log in the mirror's lfs
// directory, and the log lists every GIT_* variable git-lfs was started with, the credential's
// header among them. An attempt that failed that way after fetching an object is run again, and
// when the next one completes the log is still beside the objects. Only the objects are archived,
// so no stored object holds a token.
func TestAnLFSFetchRunAgainStoresNoGitLFSLog(t *testing.T) {
	first, second := newToken(t), newToken(t)
	// The token runs out after the first object, and the batch git-lfs then asks for again is
	// answered with a server error rather than a 401.
	a, _ := newLFSTokenRun(t, first, func(a *authRun) {
		putToken(t, a.tokenPath, second)
		a.server.mu.Lock()
		a.server.want, a.server.failBatch = basicAuth(second), http.StatusBadGateway
		a.server.mu.Unlock()
	})

	res, err := a.backup(t, true)
	if res == nil || res.Manifest == nil {
		t.Fatalf("no manifest: %v", err)
	}
	if e := res.Manifest.Repos[0]; err != nil || e.Status != pipeline.StatusSuccess {
		t.Fatalf("octo/lfsrepo = %s %q (%v), want copied", e.Status, e.Error, err)
	}
	// git-lfs says where it logged the error, and the failed attempt's error is in the run's log.
	// Without that file there is nothing to keep out of the archive.
	if !strings.Contains(a.logs.String(), "Errors logged to") {
		t.Fatalf("git-lfs logged no error to a file, so nothing was tested:\n%s", a.logs)
	}

	if oids, others := archivedLFS(t, a.dest, "github.com/octo/lfsrepo/2026-06-13/lfsrepo.lfs.tar"); len(oids) != 2 || len(others) != 0 {
		t.Errorf("the LFS archive holds %v and %v, want two objects and nothing else", oids, others)
	}
	a.dest.mu.Lock()
	for key, b := range a.dest.objs {
		for _, tok := range []string{first, second} {
			if bytes.Contains(b, []byte(tok[4:])) || bytes.Contains(b, []byte(strings.TrimPrefix(basicAuth(tok), "Basic "))) {
				t.Errorf("a token is in the stored %s", key)
			}
		}
	}
	a.dest.mu.Unlock()
	a.assertTokensStayOnTheWire(t, res, first, second)
}

// An attempt that adds no object fails the repository with its error, and the fetch is not run
// again: the first attempt, as it always did, or one run again, which would otherwise run for as
// long as the run does. Nothing is stored, so the same day's rerun copies the repository.
func TestAnLFSFetchThatAddsNothingFailsTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name    string
		run     func(t *testing.T, tok string) *authRun
		fetches int
		want    string // how the recorded error starts
	}{
		{
			name: "the first attempt, from a source that has none of the objects",
			run: func(t *testing.T, tok string) *authRun {
				requireLFS(t)
				isolateGit(t)
				workdir, _ := initTwoObjectLFSFixture(t)
				return newAuthRun(t, workdir, t.TempDir(), "lfsrepo", tok)
			},
			fetches: 1,
			want:    "lfs fetch: git lfs: ",
		},
		{
			// The token runs out after the first object, and the file is never replaced, so the
			// second attempt is refused before it fetches anything.
			name: "an attempt run again, with a token that ran out and was not replaced",
			run: func(t *testing.T, tok string) *authRun {
				a, _ := newLFSTokenRun(t, tok, func(a *authRun) { a.server.expect("a token the file never holds") })
				return a
			},
			fetches: 2,
			want:    "lfs fetch: attempt 2 fetched no object, so it is not run again (1 fetched before it): git lfs: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := newToken(t)
			a := tc.run(t, tok)

			res, err := a.backup(t, true)
			if err == nil {
				t.Fatal("the run succeeded without the repository's LFS objects")
			}
			if res == nil || res.Manifest == nil {
				t.Fatalf("no manifest: %v", err)
			}
			if e := res.Manifest.Repos[0]; e.Status != pipeline.StatusFailed || !strings.HasPrefix(e.Error, tc.want) {
				t.Errorf("octo/lfsrepo = %s %q, want failed with an error that starts %q", e.Status, e.Error, tc.want)
			}
			if n := a.lfsFetches(t); n != tc.fetches {
				t.Errorf("%d LFS fetches, want %d", n, tc.fetches)
			}
			if got := storedNames(a.dest, "github.com/octo/lfsrepo/"); len(got) != 0 {
				t.Errorf("stored %v for a repository whose LFS objects were not all fetched, want nothing", got)
			}
			a.assertTokensStayOnTheWire(t, res, tok)
		})
	}
}
