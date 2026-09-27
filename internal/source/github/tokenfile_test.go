package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/source"
)

// canaryToken is a token-shaped value that must turn up nowhere but where it is sent.
func canaryToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "ghs_canary" + hex.EncodeToString(b)
}

// replaceTokenFile writes content beside path and renames it over path, the way a caller
// replaces the file during a run.
func replaceTokenFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.WriteString(content)
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Fatalf("write %s: %v %v", f.Name(), werr, cerr)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// Every verb, the mismatched ones included: a mismatched verb is how a secret leaked before.
// %p is absent because fmt handles it before any method can; see redact.Secret.Format.
var everyVerb = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%b", "%o", "%U", "%e", "%10.3s"}

func renderEveryWay(v any) string {
	var b strings.Builder
	for _, verb := range everyVerb {
		fmt.Fprintf(&b, verb+"\n", v)
	}
	if err, ok := v.(error); ok {
		b.WriteString(err.Error() + "\n")
		b.WriteString(fmt.Errorf("wrapped: %w", err).Error() + "\n")
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Error("failed", "err", v, "value", v)
	slog.New(slog.NewTextHandler(&logged, nil)).Error("failed", "err", v, "value", v)
	b.Write(logged.Bytes())
	return b.String()
}

// assertNoCanary fails when any 8 bytes of the canary's random part, the bytes that cannot occur
// by accident, show up in out, in the clear or as base64 of the Basic credential git sends.
func assertNoCanary(t *testing.T, what, out, canary string) {
	t.Helper()
	random := strings.TrimPrefix(canary, "ghs_canary")
	for i := 0; i+8 <= len(random); i++ {
		if strings.Contains(out, random[i:i+8]) {
			t.Fatalf("%s shows part of the token (%q):\n%s", what, random[i:i+8], out)
		}
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + canary)); strings.Contains(out, b64[20:]) {
		t.Fatalf("%s shows the token as a Basic credential:\n%s", what, out)
	}
}

func assertTokenFileError(t *testing.T, err error, path, want, canary string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one saying %q", want)
	}
	msg := err.Error()
	if !strings.Contains(msg, "source.github.tokenPath") {
		t.Errorf("error does not name source.github.tokenPath: %v", err)
	}
	if !strings.Contains(msg, path) {
		t.Errorf("error does not name the file %s: %v", path, err)
	}
	if !strings.Contains(msg, want) {
		t.Errorf("error does not say %q: %v", want, err)
	}
	assertNoCanary(t, "the error", renderEveryWay(err), canary)
}

// What the reader accepts: one token, with whitespace around it ignored.
func TestTokenFileReadsOneToken(t *testing.T) {
	tok := canaryToken(t)
	atLimit := strings.Repeat("x", maxTokenFileBytes)
	for _, tc := range []struct{ name, content, want string }{
		{name: "the token alone", content: tok, want: tok},
		{name: "a newline after it, as echo writes", content: tok + "\n", want: tok},
		{name: "whitespace around it", content: " \t" + tok + "\r\n\n", want: tok},
		{name: "exactly the size limit", content: atLimit, want: atLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "github-token")
			replaceTokenFile(t, path, tc.content)
			got, err := tokenFile{path: path}.read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Reveal() != tc.want {
				t.Errorf("read %d bytes, want %d", len(got.Reveal()), len(tc.want))
			}
			// Held as a Secret, so whatever formats it prints the placeholder.
			assertNoCanary(t, "the token formatted", renderEveryWay(got), tok)
		})
	}
}

// Each thing that can be wrong with the file fails the read, and the error names the file and
// the problem and never what the file held.
//
// Every case starts from a good file that the reader accepts, and then breaks it. A reader that
// kept what it read last time would still hand out the good token, so each case is also the
// proof that nothing is cached.
func TestTokenFileRefusesWhatIsNotAToken(t *testing.T) {
	tok := canaryToken(t)
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, path string)
		want  string
	}{
		{name: "missing", want: "no such file or directory", spoil: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory", want: "is not a regular file", spoil: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "empty", want: "is empty", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, "") }},
		{name: "only whitespace", want: "is empty", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, " \n\t\r\n") }},
		{name: "oversized", want: "larger than 4096 bytes", spoil: func(t *testing.T, path string) {
			replaceTokenFile(t, path, tok+strings.Repeat("A", maxTokenFileBytes))
		}},
		{name: "a token at the limit and a newline", want: "larger than 4096 bytes", spoil: func(t *testing.T, path string) {
			replaceTokenFile(t, path, tok+strings.Repeat("x", maxTokenFileBytes-len(tok))+"\n")
		}},
		{name: "a space inside", want: "offset 12", spoil: func(t *testing.T, path string) {
			replaceTokenFile(t, path, tok[:12]+" "+tok[12:])
		}},
		{name: "a second line", want: fmt.Sprintf("offset %d", len(tok)), spoil: func(t *testing.T, path string) {
			replaceTokenFile(t, path, tok+"\n"+tok+"\n")
		}},
		{name: "a NUL", want: "offset", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, tok+"\x00") }},
		{name: "DEL", want: "offset", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, tok+"\x7f") }},
		{name: "a non-ASCII letter", want: "offset", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, "\xc3\xa9"+tok) }},
		{name: "a byte-order mark", want: "offset 0", spoil: func(t *testing.T, path string) { replaceTokenFile(t, path, "\xef\xbb\xbf"+tok) }},
		{name: "an Authorization header instead of a token", want: "offset 6", spoil: func(t *testing.T, path string) {
			replaceTokenFile(t, path, "Bearer "+tok)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "github-token")
			replaceTokenFile(t, path, tok)
			f := tokenFile{path: path}
			if got, err := f.read(); err != nil || got.Reveal() != tok {
				t.Fatalf("the good file did not read back: %v", err)
			}

			tc.spoil(t, path)
			got, err := f.read()
			if got != "" {
				t.Error("a refused file still produced a token")
			}
			assertTokenFileError(t, err, path, tc.want, tok)
		})
	}

	t.Run("a missing file is fs.ErrNotExist", func(t *testing.T) {
		_, err := tokenFile{path: filepath.Join(t.TempDir(), "never-written")}.read()
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("errors.Is(%v, fs.ErrNotExist) = false", err)
		}
	})
}

// recordingTransport stands in for the network: it keeps each request it is handed and answers
// 200 with an empty body.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// The token goes on a request only over https to the API host, and to nothing else.
func TestTokenTransportAuthenticatesOnlyTheAPIHost(t *testing.T) {
	tok := canaryToken(t)
	path := filepath.Join(t.TempDir(), "github-token")
	replaceTokenFile(t, path, tok)

	for _, tc := range []struct {
		name, apiHost, url string
		// A header already on the request, which must not survive a request that goes out bare.
		preset string
		want   string
	}{
		{name: "github.com over https", apiHost: "api.github.com", url: "https://api.github.com/installation/repositories", want: "Bearer " + tok},
		{name: "github.com over plain http", apiHost: "api.github.com", url: "http://api.github.com/installation/repositories"},
		{name: "another github.com host", apiHost: "api.github.com", url: "https://uploads.github.com/repos/o/r/releases"},
		{name: "a host that starts with the API host", apiHost: "api.github.com", url: "https://api.github.com.example.net/installation/repositories"},
		{name: "the API host on another port", apiHost: "api.github.com", url: "https://api.github.com:8443/installation/repositories"},
		{name: "the API host spelled in capitals", apiHost: "api.github.com", url: "https://API.GITHUB.COM/installation/repositories"},
		{name: "another host, carrying a header already", apiHost: "api.github.com", url: "https://example.net/", preset: "Bearer stale"},
		{name: "Enterprise Server over https", apiHost: "ghe.example.com", url: "https://ghe.example.com/api/v3/installation/repositories", want: "Bearer " + tok},
		{name: "github.com, from an Enterprise Server source", apiHost: "ghe.example.com", url: "https://api.github.com/installation/repositories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &recordingTransport{}
			tr := &tokenTransport{base: base, file: tokenFile{path: path}, host: tc.apiHost}
			req, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.preset != "" {
				req.Header.Set("Authorization", tc.preset)
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if len(base.reqs) != 1 {
				t.Fatalf("%d requests went out, want 1", len(base.reqs))
			}
			if got := base.reqs[0].Header.Get("Authorization"); got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
			// The caller's request is its own: a RoundTripper must not modify it.
			if got := req.Header.Get("Authorization"); got != tc.preset {
				t.Errorf("the caller's request was changed: Authorization = %q", got)
			}
		})
	}

	t.Run("a file that cannot be read fails the request before it is sent", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		base := &recordingTransport{}
		tr := &tokenTransport{base: base, file: tokenFile{path: missing}, host: "api.github.com"}
		body := &closeRecorder{Reader: strings.NewReader("{}")}
		req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("the request went ahead without its token")
		}
		if len(base.reqs) != 0 {
			t.Errorf("%d requests went out", len(base.reqs))
		}
		if !body.closed {
			t.Error("the request body was not closed")
		}
	})
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// A redirect off the API host goes out bare, and a redirect that stays on it keeps the token.
// Two real servers, and Go's own client following the redirect, because the client's rule for
// dropping a header on a redirect only covers headers it was handed, and this one is not.
func TestARedirectOffTheAPIHostGoesOutBare(t *testing.T) {
	tok := canaryToken(t)
	path := filepath.Join(t.TempDir(), "github-token")
	replaceTokenFile(t, path, tok)

	var mu sync.Mutex
	seen := map[string]string{}
	record := func(name string, r *http.Request) {
		mu.Lock()
		seen[name] = r.Header.Get("Authorization")
		mu.Unlock()
	}
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("elsewhere", r)
	}))
	defer elsewhere.Close()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r.URL.Path, r)
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/landed", http.StatusFound)
		case "/moved":
			http.Redirect(w, r, "/here", http.StatusMovedPermanently)
		}
	}))
	defer api.Close()

	u, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &tokenTransport{base: api.Client().Transport, file: tokenFile{path: path}, host: u.Host}}
	for _, p := range []string{"/away", "/moved"} {
		resp, err := client.Get(api.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	bearer := "Bearer " + tok
	for name, want := range map[string]string{"/away": bearer, "elsewhere": "", "/moved": bearer, "/here": bearer} {
		if got, ok := seen[name]; !ok || got != want {
			t.Errorf("%s: Authorization = %q (seen %v), want %q", name, got, ok, want)
		}
	}
}

// fakeGitHubAPI answers the repository list for an installation and records the Authorization
// header of every request.
type fakeGitHubAPI struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seenRequest
	// status, when set, is the answer to every request instead of the list.
	status int
}

type seenRequest struct{ method, path, query, auth string }

func newFakeGitHubAPI(t *testing.T) *fakeGitHubAPI {
	t.Helper()
	f := &fakeGitHubAPI{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, seenRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")})
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"Bad credentials"}`, status)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/installation/repositories") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"repositories": []map[string]any{{
				"name": "hello", "clone_url": "https://github.com/octo/hello.git", "owner": map[string]any{"login": "octo"},
			}},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHubAPI) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.reqs...)
}

// tokenFileSource builds the source the way the CLI does in token mode, over a transport that
// trusts the fake API's certificate.
func tokenFileSource(t *testing.T, api *fakeGitHubAPI, path string) *Source {
	t.Helper()
	s, err := newSource(Options{BaseURL: api.URL, TokenPath: path}, nil, api.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The file is read again for every request, so a replacement written between two requests is
// what the second one carries. Both the API and git see it.
func TestATokenFileReplacedBetweenRequestsIsUsedByTheNextOne(t *testing.T) {
	api := newFakeGitHubAPI(t)
	path := filepath.Join(t.TempDir(), "github-token")
	first, second := canaryToken(t), canaryToken(t)
	replaceTokenFile(t, path, first+"\n")
	s := tokenFileSource(t, api, path)
	ctx := context.Background()

	if _, err := s.ListRepos(ctx, source.Filter{}); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	h1, err := s.GitAuthHeader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replaceTokenFile(t, path, second+"\n")
	if _, err := s.ListRepos(ctx, source.Filter{}); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	h2, err := s.GitAuthHeader(ctx)
	if err != nil {
		t.Fatal(err)
	}

	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if reqs[0].auth != "Bearer "+first || reqs[1].auth != "Bearer "+second {
		t.Errorf("the API saw %q then %q, want the first token then the second", reqs[0].auth, reqs[1].auth)
	}
	for i, tc := range []struct{ header, tok string }{{h1, first}, {h2, second}} {
		want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tc.tok))
		if tc.header != want {
			t.Errorf("git header %d does not carry token %d", i+1, i+1)
		}
	}
}

// Enumeration is the installation's own list, which only an installation token can read.
func TestTokenModeListsTheInstallationsRepositories(t *testing.T) {
	api := newFakeGitHubAPI(t)
	path := filepath.Join(t.TempDir(), "github-token")
	tok := canaryToken(t)
	replaceTokenFile(t, path, tok)
	s := tokenFileSource(t, api, path)

	repos, err := s.ListRepos(context.Background(), source.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Slug() != "octo/hello" {
		t.Fatalf("repos = %+v", repos)
	}
	reqs := api.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/api/v3/installation/repositories" {
		t.Fatalf("requests = %+v, want one GET of /api/v3/installation/repositories", reqs)
	}
	if !s.UsesTokenFile() {
		t.Error("UsesTokenFile = false in token mode")
	}
	if got := s.client.Client().Timeout; got != 60*time.Second {
		t.Errorf("client timeout = %s, want 60s", got)
	}
}

// doctor's check: exactly one request, for one repository, with the token.
func TestCheckTokenMakesOneRequest(t *testing.T) {
	api := newFakeGitHubAPI(t)
	path := filepath.Join(t.TempDir(), "github-token")
	tok := canaryToken(t)
	replaceTokenFile(t, path, tok)
	s := tokenFileSource(t, api, path)

	if err := s.CheckToken(context.Background()); err != nil {
		t.Fatalf("CheckToken: %v", err)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want exactly 1: %+v", len(reqs), reqs)
	}
	want := seenRequest{http.MethodGet, "/api/v3/installation/repositories", "per_page=1", "Bearer " + tok}
	if reqs[0] != want {
		t.Errorf("request = %+v, want %+v", reqs[0], want)
	}

	t.Run("a token GitHub refuses fails the check", func(t *testing.T) {
		api.mu.Lock()
		api.status = http.StatusUnauthorized
		api.mu.Unlock()
		err := s.CheckToken(context.Background())
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("CheckToken = %v, want GitHub's 401", err)
		}
		if !strings.Contains(err.Error(), "source.github.tokenPath") {
			t.Errorf("the error does not say which setting: %v", err)
		}
		assertNoCanary(t, "the error", renderEveryWay(err), tok)
	})
}

// New reads the file once, so a run with a bad file fails before any work. It keeps nothing:
// the file going away afterwards fails the next request.
func TestNewFailsFastOnABadTokenFile(t *testing.T) {
	tok := canaryToken(t)
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing")
	_, err := New(Options{TokenPath: missing}, nil)
	assertTokenFileError(t, err, missing, "no such file", tok)

	bad := filepath.Join(dir, "bad")
	replaceTokenFile(t, bad, tok+" "+tok)
	_, err = New(Options{TokenPath: bad}, nil)
	assertTokenFileError(t, err, bad, "offset", tok)

	if _, err := New(Options{TokenPath: bad, PrivateKeyPEM: []byte("pem")}, nil); err == nil ||
		!strings.Contains(err.Error(), "both a token file and an App private key") {
		t.Errorf("New with a token file and a key = %v, want a refusal", err)
	}

	good := filepath.Join(dir, "good")
	replaceTokenFile(t, good, tok)
	s, err := New(Options{TokenPath: good, AppID: 1, InstallationID: 2}, nil)
	if err != nil {
		t.Fatalf("New with a good file, appID and installationID set: %v", err)
	}
	if err := os.Remove(good); err != nil {
		t.Fatal(err)
	}
	_, err = s.GitAuthHeader(context.Background())
	assertTokenFileError(t, err, good, "no such file", tok)
}

// A file that goes wrong during a run fails the request that read it, and that request only:
// nothing is sent, nothing falls back, and no error shows the token that was there before.
func TestATokenFileThatGoesWrongFailsTheRequestThatReadIt(t *testing.T) {
	tok := canaryToken(t)
	for _, tc := range []struct {
		name, content, want string
		remove              bool
	}{
		{name: "missing", remove: true, want: "no such file"},
		{name: "empty", content: "", want: "is empty"},
		{name: "oversized", content: tok + strings.Repeat("A", maxTokenFileBytes), want: "larger than 4096 bytes"},
		{name: "an invalid character", content: tok + "\x00" + tok, want: "offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeGitHubAPI(t)
			path := filepath.Join(t.TempDir(), "github-token")
			replaceTokenFile(t, path, tok)
			s := tokenFileSource(t, api, path)
			if tc.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				replaceTokenFile(t, path, tc.content)
			}
			ctx := context.Background()

			_, err := s.ListRepos(ctx, source.Filter{})
			assertTokenFileError(t, err, path, tc.want, tok)
			_, err = s.FetchMetadata(ctx, source.Repo{Host: "github.com", Owner: "octo", Name: "hello"})
			assertTokenFileError(t, err, path, tc.want, tok)
			err = s.CheckToken(ctx)
			assertTokenFileError(t, err, path, tc.want, tok)
			header, err := s.GitAuthHeader(ctx)
			assertTokenFileError(t, err, path, tc.want, tok)
			if header != "" {
				t.Error("a header was produced from a file that was refused")
			}
			if reqs := api.requests(); len(reqs) != 0 {
				t.Errorf("%d requests reached GitHub without a token: %+v", len(reqs), reqs)
			}
		})
	}
}

// answersEmpty answers every request with an empty repository list, counts the requests that
// carried a token, and keeps nothing else. The source below is built on it rather than on a real
// connection: a mismatched verb makes fmt walk everything a value points to, and a live
// http.Transport is being written by its own goroutines while it is read, and would hold copies
// of requests.
type answersEmpty struct{ authorized *atomic.Int32 }

func (a answersEmpty) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		a.authorized.Add(1)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"total_count":0,"repositories":[]}`)),
	}, nil
}

// Whatever a careless line of code formats, the source or its parts, the token is not in it.
func TestTheSourceNeverFormatsTheToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-token")
	tok := canaryToken(t)
	replaceTokenFile(t, path, tok)
	base := answersEmpty{authorized: &atomic.Int32{}}
	s, err := newSource(Options{BaseURL: "https://ghe.example.com/api/v3", TokenPath: path}, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListRepos(context.Background(), source.Filter{}); err != nil {
		t.Fatal(err)
	}
	if base.authorized.Load() != 1 {
		t.Fatal("the token was never sent, so there was nothing to leak and this proves nothing")
	}
	for name, v := range map[string]any{
		"the source":       s,
		"the source value": *s,
		"the token file":   s.tokenFile,
		"the transport":    s.client.Client().Transport,
		"the options":      Options{BaseURL: "https://ghe.example.com/api/v3", TokenPath: path},
	} {
		assertNoCanary(t, name, renderEveryWay(v), tok)
	}
}

// In App mode a header per git command is cheap: the transport mints one token and keeps it
// until a minute before it expires, so a thousand commands do not mint a thousand tokens.
func TestAppModeMintsOnceForManyCommands(t *testing.T) {
	var mints int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mints++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_minted",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	s, err := New(Options{BaseURL: srv.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: testKeyPEM(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.UsesTokenFile() {
		t.Error("UsesTokenFile = true in App mode")
	}
	if err := s.CheckToken(context.Background()); err == nil {
		t.Error("CheckToken in App mode, where there is no token file, did not refuse")
	}
	for i := 0; i < 5; i++ {
		if _, err := s.GitAuthHeader(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if mints != 1 {
		t.Errorf("%d tokens minted for 5 git commands, want 1", mints)
	}
	if got := s.client.Client().Timeout; got != 60*time.Second {
		t.Errorf("client timeout = %s, want 60s", got)
	}
}

// In App mode each git command asks the transport, which mints a new token once the one it holds
// is within a minute of expiring. A run that outlasts the token's hour carries a fresh one to its
// later commands, where it used to carry the header it minted at the start.
func TestAppModeRenewsATokenNearItsExpiry(t *testing.T) {
	var mu sync.Mutex
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mints++
		n := mints
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("ghs_minted_%d", n),
			// Inside the one-minute margin, so the transport renews it at every use.
			"expires_at": time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	s, err := New(Options{BaseURL: srv.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: testKeyPEM(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.GitAuthHeader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GitAuthHeader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("the second git command carried the token minted for the first, which was about to expire")
	}
	mu.Lock()
	defer mu.Unlock()
	if mints != 2 {
		t.Errorf("%d tokens minted, want 2", mints)
	}
}
