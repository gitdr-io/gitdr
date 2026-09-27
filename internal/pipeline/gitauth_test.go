package pipeline_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
	ghsrc "gitdr.io/gitdr/internal/source/github"
)

// The pipeline asks for a git credential immediately before every git command, so a token file
// replaced during a run reaches the next command. These run a real backup against a real git
// server and look at the header on the wire.

// gitHTTPServer serves bare repositories over git's dumb HTTP protocol, which is nothing but
// files: `git update-server-info` writes the indexes a client needs and a file server does the
// rest, so this needs no git-http-backend. It also answers the git-lfs batch API from a
// directory of LFS objects.
//
// Every request must carry the Authorization header it currently expects, or it is answered 401.
type gitHTTPServer struct {
	*httptest.Server
	lfsObjects string

	mu   sync.Mutex
	want string
	seen []seenGitRequest
	// expire is answered 401 once, as GitHub answers a token that has run out, and then
	// onExpire runs, before the answer is sent.
	expire   string
	onExpire func()
}

type seenGitRequest struct {
	method, path, auth string
	status             int
}

func newGitHTTPServer(t *testing.T, root, lfsObjects string) *gitHTTPServer {
	t.Helper()
	s := &gitHTTPServer{lfsObjects: lfsObjects}
	files := http.FileServer(http.Dir(root))
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		status, then := http.StatusOK, func() {}
		switch {
		case s.expire != "" && auth == s.expire:
			status, s.expire = http.StatusUnauthorized, ""
			then = s.onExpire
		case auth != s.want:
			status = http.StatusUnauthorized
		}
		s.seen = append(s.seen, seenGitRequest{r.Method, r.URL.Path, auth, status})
		s.mu.Unlock()
		then()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch"):
			s.lfsBatch(w, r)
		case strings.HasPrefix(r.URL.Path, "/lfs-objects/"):
			oid := path.Base(r.URL.Path)
			http.ServeFile(w, r, filepath.Join(s.lfsObjects, oid[0:2], oid[2:4], oid))
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// lfsBatch answers a download batch with a URL on this server for each object.
func (s *gitHTTPServer) lfsBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Objects []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type object struct {
		OID           string                       `json:"oid"`
		Size          int64                        `json:"size"`
		Authenticated bool                         `json:"authenticated"`
		Actions       map[string]map[string]string `json:"actions"`
	}
	resp := struct {
		Transfer string   `json:"transfer"`
		Objects  []object `json:"objects"`
	}{Transfer: "basic"}
	for _, o := range req.Objects {
		resp.Objects = append(resp.Objects, object{
			OID: o.OID, Size: o.Size, Authenticated: true,
			Actions: map[string]map[string]string{"download": {"href": s.URL + "/lfs-objects/" + o.OID}},
		})
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *gitHTTPServer) expect(auth string) {
	s.mu.Lock()
	s.want = auth
	s.mu.Unlock()
}

func (s *gitHTTPServer) requests() []seenGitRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenGitRequest(nil), s.seen...)
}

// serveBare puts a bare copy of the repository at workdir under root as owner/name.git, ready to
// be cloned over dumb HTTP.
func serveBare(t *testing.T, root, workdir, owner, name string) string {
	t.Helper()
	bare := filepath.Join(root, owner, name+".git")
	for _, args := range [][]string{
		{"clone", "--quiet", "--bare", "--", workdir, bare},
		{"-C", bare, "update-server-info"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return owner + "/" + name + ".git"
}

// isolateGit keeps the machine's own git setup out of the run: no credential helper to answer a
// 401, no proxy, nothing a developer's ~/.gitconfig could add. And no askpass program: these
// tests answer 401 on purpose, and GIT_TERMINAL_PROMPT=0 stops git prompting on a terminal but
// not running GIT_ASKPASS, which an editor's terminal sets and which then waits for a person. An
// empty GIT_ASKPASS also stops git falling back to core.askPass and SSH_ASKPASS.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
}

func newToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "ghs_" + hex.EncodeToString(b)
}

// basicAuth is the Authorization header git sends for a GitHub token.
func basicAuth(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
}

// putToken writes tok beside path and renames it over path, the way a caller replaces the file.
func putToken(t *testing.T, path, tok string) {
	t.Helper()
	f, err := os.CreateTemp(filepath.Dir(path), ".github-token-*")
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.WriteString(tok)
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Fatalf("write token: %v %v", werr, cerr)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// tokenFileSource lists one repository and fetches canned metadata. Its git credential is the
// real GitHub source's, reading a real token file; that half is what is under test.
type tokenFileSource struct {
	github *ghsrc.Source
	repo   source.Repo

	mu            sync.Mutex
	cloneURLCalls int
	// onCloneURL runs on every CloneURL with the call's number. The pipeline resolves the clone
	// URL once before ls-remote and once before the clone, so call 2 falls between them.
	onCloneURL func(call int)
	// onMetadata runs when metadata is fetched, which is after the clone and before LFS.
	onMetadata func()
}

func (s *tokenFileSource) ListRepos(context.Context, source.Filter) ([]source.Repo, error) {
	return []source.Repo{s.repo}, nil
}

func (s *tokenFileSource) CloneURL(_ context.Context, r source.Repo) (string, error) {
	s.mu.Lock()
	s.cloneURLCalls++
	call := s.cloneURLCalls
	s.mu.Unlock()
	if s.onCloneURL != nil {
		s.onCloneURL(call)
	}
	return r.CloneURL, nil
}

func (s *tokenFileSource) FetchMetadata(context.Context, source.Repo) ([]byte, error) {
	if s.onMetadata != nil {
		s.onMetadata()
	}
	return []byte(`{"schema":"gitdr.meta/v0"}`), nil
}

func (s *tokenFileSource) GitAuthHeader(ctx context.Context) (string, error) {
	return s.github.GitAuthHeader(ctx)
}

// authRun is one backup of one repository served over HTTP, with its credential in a token file.
type authRun struct {
	server    *gitHTTPServer
	src       *tokenFileSource
	tokenPath string
	logs      *bytes.Buffer
}

func newAuthRun(t *testing.T, workdir, lfsObjects, name, firstToken string) *authRun {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	repoPath := serveBare(t, root, workdir, "octo", name)
	server := newGitHTTPServer(t, root, lfsObjects)
	server.expect(basicAuth(firstToken))

	tokenPath := filepath.Join(t.TempDir(), "github-token")
	putToken(t, tokenPath, firstToken)
	github, err := ghsrc.New(ghsrc.Options{TokenPath: tokenPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &authRun{
		server: server,
		src: &tokenFileSource{github: github, repo: source.Repo{
			Host: "github.com", Owner: "octo", Name: name, CloneURL: server.URL + "/" + repoPath, DefaultBranch: "main",
		}},
		tokenPath: tokenPath,
		logs:      &bytes.Buffer{},
	}
}

func (a *authRun) backup(t *testing.T, lfs bool) (*pipeline.BackupResult, error) {
	t.Helper()
	_, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Source.Repo = "octo/" + a.src.repo.Name
	cfg.Backup.LFS = lfs
	// Debug, so every git command's arguments are in the log too.
	log := slog.New(slog.NewJSONHandler(a.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// A git command that waits on something, a prompt or a stalled server, fails the test in
	// minutes rather than holding it until go test gives up.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: a.src, Dest: newMemDest(true), Git: gitexec.New(log),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(), Logger: log,
	})
}

// assertTokensStayOnTheWire checks that no token reached the log, which holds every git
// command's arguments, or the manifest.
func (a *authRun) assertTokensStayOnTheWire(t *testing.T, res *pipeline.BackupResult, tokens ...string) {
	t.Helper()
	var manifest []byte
	if res != nil && res.Manifest != nil {
		var err error
		if manifest, err = json.Marshal(res.Manifest); err != nil {
			t.Fatal(err)
		}
	}
	for _, tok := range tokens {
		cred := strings.TrimPrefix(basicAuth(tok), "Basic ")
		for where, text := range map[string]string{"the log": a.logs.String(), "the manifest": string(manifest)} {
			if strings.Contains(text, tok[4:]) || strings.Contains(text, cred) {
				t.Errorf("a token reached %s:\n%s", where, text)
			}
		}
	}
}

// The token file is read again before each git command: ls-remote, every clone attempt and the
// LFS fetch each carry whatever the file held when that command started.
func TestTheTokenFileIsReadBeforeEveryGitCommand(t *testing.T) {
	t.Run("a file replaced between ls-remote and clone", func(t *testing.T) {
		first, second := newToken(t), newToken(t)
		a := newAuthRun(t, initFixtureRepo(t), "", "hello", first)
		boundary := -1
		a.src.onCloneURL = func(call int) {
			if call == 2 { // ls-remote is done; the clone has not asked for its header yet
				boundary = len(a.server.requests())
				putToken(t, a.tokenPath, second)
				a.server.expect(basicAuth(second))
			}
		}

		res, err := a.backup(t, false)
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		reqs := a.server.requests()
		if boundary < 1 {
			t.Fatalf("ls-remote sent nothing before the file was replaced (%d requests)", boundary)
		}
		assertCarried(t, "ls-remote", reqs[:boundary], basicAuth(first))
		assertCarried(t, "clone", reqs[boundary:], basicAuth(second))
		a.assertTokensStayOnTheWire(t, res, first, second)
	})

	t.Run("a file replaced after a clone attempt failed", func(t *testing.T) {
		// The retry asks again, so an attempt made after the token was replaced uses the
		// replacement rather than the credential that was just refused.
		first, second, third := newToken(t), newToken(t), newToken(t)
		a := newAuthRun(t, initFixtureRepo(t), "", "hello", first)
		a.src.onCloneURL = func(call int) {
			if call == 2 {
				putToken(t, a.tokenPath, second)
				a.server.mu.Lock()
				a.server.want, a.server.expire = basicAuth(second), basicAuth(second)
				a.server.onExpire = func() {
					putToken(t, a.tokenPath, third)
					a.server.expect(basicAuth(third))
				}
				a.server.mu.Unlock()
			}
		}

		res, err := a.backup(t, false)
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		reqs := a.server.requests()
		refused := -1
		for i, r := range reqs {
			if r.auth == basicAuth(second) {
				if refused != -1 || r.status != http.StatusUnauthorized {
					t.Fatalf("the second token was sent more than once, or was not the one refused: %+v", reqs)
				}
				refused = i
			}
		}
		if refused == -1 {
			t.Fatalf("no clone attempt carried the second token: %+v", reqs)
		}
		assertCarried(t, "the retried clone", reqs[refused+1:], basicAuth(third))
		a.assertTokensStayOnTheWire(t, res, first, second, third)
	})

	t.Run("a file replaced between clone and LFS fetch", func(t *testing.T) {
		if !gitexec.LFSAvailable() {
			if os.Getenv("CI") != "" {
				t.Fatal("git-lfs is not installed; in CI the LFS fetch must be exercised, not skipped")
			}
			t.Skip("git-lfs not installed")
		}
		first, second, third := newToken(t), newToken(t), newToken(t)
		isolateGit(t) // before the fixture, which runs git-lfs too
		workdir, _ := initLFSFixture(t)
		a := newAuthRun(t, workdir, filepath.Join(workdir, ".git", "lfs", "objects"), "lfsrepo", first)
		a.src.onCloneURL = func(call int) {
			if call == 2 {
				putToken(t, a.tokenPath, second)
				a.server.expect(basicAuth(second))
			}
		}
		boundary := -1
		a.src.onMetadata = func() { // the clone is done; LFS has not asked for its header yet
			boundary = len(a.server.requests())
			putToken(t, a.tokenPath, third)
			a.server.expect(basicAuth(third))
		}

		res, err := a.backup(t, true)
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		reqs := a.server.requests()
		if boundary < 0 {
			t.Fatal("metadata was never fetched")
		}
		var batch, download bool
		for _, r := range reqs[boundary:] {
			batch = batch || strings.HasSuffix(r.path, "/info/lfs/objects/batch")
			download = download || strings.HasPrefix(r.path, "/lfs-objects/")
		}
		if !batch || !download {
			t.Fatalf("the LFS fetch did not reach the server (batch %v, download %v): %+v", batch, download, reqs[boundary:])
		}
		assertCarried(t, "lfs fetch", reqs[boundary:], basicAuth(third))
		kinds := map[string]bool{}
		for _, art := range res.Manifest.Repos[0].Artifacts {
			kinds[art.Kind] = true
		}
		if !kinds["lfs"] {
			t.Errorf("no lfs artifact: %v", kinds)
		}
		a.assertTokensStayOnTheWire(t, res, first, second, third)
	})
}

func assertCarried(t *testing.T, what string, reqs []seenGitRequest, auth string) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatalf("%s sent no requests", what)
	}
	for _, r := range reqs {
		if r.auth != auth || r.status != http.StatusOK {
			t.Errorf("%s: %s %s carried the wrong token or was refused (%d)", what, r.method, r.path, r.status)
		}
	}
}

// A token file that goes missing or empty during a run fails the repository that needed it:
// the manifest records the failure, the run returns an error, and so exits 1. Nothing falls back
// to an unauthenticated request, and no error or log line shows the token that was there.
func TestATokenFileThatGoesWrongMidRunFailsTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		spoil      func(t *testing.T, path string)
	}{
		{name: "missing", want: "no such file or directory", spoil: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "empty", want: "is empty", spoil: func(t *testing.T, path string) { putToken(t, path, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := newToken(t)
			a := newAuthRun(t, initFixtureRepo(t), "", "hello", tok)
			a.src.onCloneURL = func(call int) {
				if call == 1 { // after the preflight, before ls-remote
					tc.spoil(t, a.tokenPath)
				}
			}

			res, err := a.backup(t, false)
			if err == nil {
				t.Fatal("the run succeeded without a credential")
			}
			if res == nil || res.Manifest == nil || res.Manifest.Status != pipeline.StatusFailed {
				t.Fatalf("result = %+v, want a failed manifest", res)
			}
			entry := res.Manifest.Repos[0]
			if entry.Status != pipeline.StatusFailed {
				t.Errorf("repository status = %s, want failed", entry.Status)
			}
			for _, want := range []string{"source.github.tokenPath", a.tokenPath, tc.want} {
				if !strings.Contains(entry.Error, want) {
					t.Errorf("the recorded error does not say %q: %s", want, entry.Error)
				}
			}
			if reqs := a.server.requests(); len(reqs) != 0 {
				t.Errorf("git went ahead without the credential: %+v", reqs)
			}
			a.assertTokensStayOnTheWire(t, res, tok)
		})
	}
}

// The preflight: a credential that cannot produce a header at all stops the run before any
// repository is touched, with the error it has always had.
func TestNoCredentialAtTheStartStopsTheRun(t *testing.T) {
	tok := newToken(t)
	a := newAuthRun(t, initFixtureRepo(t), "", "hello", tok)
	if err := os.Remove(a.tokenPath); err != nil {
		t.Fatal(err)
	}

	res, err := a.backup(t, false)
	if err == nil || !strings.HasPrefix(err.Error(), "source auth: source.github.tokenPath:") {
		t.Fatalf("err = %v, want the source auth preflight to fail on the token file", err)
	}
	if res != nil {
		t.Errorf("a result was returned for a run that never started: %+v", res)
	}
	if reqs := a.server.requests(); len(reqs) != 0 {
		t.Errorf("git ran: %+v", reqs)
	}
}

// A token GitHub refuses, as it refuses one that has expired, fails the repository the same way
// a missing file does. The clone is tried three times, each with the token, and nothing is sent
// without it.
func TestATokenTheSourceRefusesFailsTheRepository(t *testing.T) {
	tok := newToken(t)
	a := newAuthRun(t, initFixtureRepo(t), "", "hello", tok)
	a.server.expect("a header nobody sends") // every request is answered 401

	res, err := a.backup(t, false)
	if err == nil {
		t.Fatal("the run succeeded with a token the source refused")
	}
	if res == nil || res.Manifest == nil || res.Manifest.Repos[0].Status != pipeline.StatusFailed {
		t.Fatalf("result = %+v, want the repository recorded as failed", res)
	}
	reqs := a.server.requests()
	// One ls-remote and three clone attempts, and git gives up on each at its first 401.
	if len(reqs) != 4 {
		t.Errorf("%d requests, want 4 (ls-remote and three clone attempts): %+v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.auth != basicAuth(tok) {
			t.Errorf("%s %s went out with %q, not the token", r.method, r.path, r.auth)
		}
	}
	a.assertTokensStayOnTheWire(t, res, tok)
}

// A source that stops sending no longer holds the run: git gives up on each transfer, the clone
// is retried, and the repository fails, where before the run waited for good.
//
// git's own GIT_HTTP_LOW_SPEED_TIME shortens the window to a second; the limit is gitdr's.
func TestAStalledSourceFailsTheRepository(t *testing.T) {
	isolateGit(t)
	t.Setenv("GIT_HTTP_LOW_SPEED_TIME", "1")
	var mu sync.Mutex
	requests := 0
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Ends by itself after a while, so a missing limit fails the test at its deadline rather
		// than hanging it on a git-remote-http that outlived the git that started it.
		select {
		case <-r.Context().Done():
		case <-done:
		case <-time.After(25 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })

	_, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Backup.LFS = false
	src := &fixtureSource{repos: []source.Repo{{Host: "github.com", Owner: "octo", Name: "hello", CloneURL: srv.URL + "/octo/hello.git"}}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: newMemDest(true), Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "test", Now: fixedClock(),
	})
	if ctx.Err() != nil {
		t.Fatal("the run was still waiting on the stalled source a minute later")
	}
	if err == nil {
		t.Fatal("the run succeeded against a source that sent nothing")
	}
	entry := res.Manifest.Repos[0]
	if entry.Status != pipeline.StatusFailed || !strings.Contains(entry.Error, "1000 bytes") {
		t.Errorf("repository %s: %q, want it failed by the low-speed limit", entry.Status, entry.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 4 {
		t.Errorf("%d transfers, want 4 (ls-remote and three clone attempts)", requests)
	}
}
