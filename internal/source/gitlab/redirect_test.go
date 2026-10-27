package gitlab

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/source"
)

// localhostTLS is a server certificate for 127.0.0.1 and for localhost, made for one test.
// httptest's own names 127.0.0.1 and not localhost, and the redirect tests need two hosts that a
// client tells apart and trusts both of: the API on 127.0.0.1, everything else on localhost.
func localhostTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// startServer serves h over TLS with cert, or over plain HTTP when cert is nil, until the test
// ends. A TLS server's Client trusts cert, so it trusts every server started with the same one.
func startServer(t *testing.T, cert *tls.Config, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	if cert != nil {
		srv.TLS = cert
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

// onLocalhost is srv's URL with its host spelled localhost: the same server, and to a client
// another host.
func onLocalhost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort("localhost", u.Port())
	return u.String()
}

// hop is one request a server received, with every header it carried.
type hop struct {
	server, path string
	header       http.Header
}

type hops struct {
	mu   sync.Mutex
	list []hop
}

func (h *hops) record(server string, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.list = append(h.list, hop{server, r.URL.Path, r.Header.Clone()})
}

func (h *hops) all() []hop {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hop(nil), h.list...)
}

// glCanary is a token-shaped value that must turn up nowhere but where it is sent.
func glCanary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "glpat-canary" + hex.EncodeToString(b)
}

// projectList answers with one project, whose clone URL is cloneURL.
func projectList(w http.ResponseWriter, cloneURL string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"path": "myrepo", "http_url_to_repo": cloneURL, "namespace": map[string]any{"full_path": "mygroup"},
	}})
}

// assertRefused fails unless err is the refusal of a redirect from one origin to the other for
// reason, and says nothing more of where the redirect pointed: no path, no query string, no token.
func assertRefused(t *testing.T, what string, err error, from, to, reason, tok string) {
	t.Helper()
	want := "refusing a redirect from " + from + " to " + to + ": " + reason
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s = %v; want it to say %q", what, err, want)
		return
	}
	for _, msg := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(msg, "?") || strings.Contains(msg, "/landed") || strings.Contains(msg, tok) {
			t.Errorf("%s's error shows more than the two hosts: %s", what, msg)
		}
	}
}

// A redirect is followed on the API and nowhere else. client-go puts PRIVATE-TOKEN on the request
// before Go's client sends it, and Go's client copies every header but Authorization onto each hop
// of a redirect, wherever it goes. A hop on the API host keeps the token; a hop to another host, or
// from https to plain http, fails before anything is sent there, once: client-go retries a GET that
// failed with a response, and must not send this one again.
func TestARedirectNeverTakesTheTokenOffTheAPI(t *testing.T) {
	cert := localhostTLS(t)
	tlsIf := func(on bool) *tls.Config {
		if on {
			return cert
		}
		return nil
	}

	for _, tc := range []struct {
		name             string
		apiTLS, otherTLS bool
		status           int
		sameHost         bool   // the redirect goes to /here on the API, not to localhost
		refusal          string // why the redirect is refused, or "" when it is followed
	}{
		{name: "to another host", status: http.StatusFound, refusal: "it leaves the API"},
		{name: "to another host, over TLS", apiTLS: true, otherTLS: true, status: http.StatusFound, refusal: "it leaves the API"},
		{name: "on the API host", apiTLS: true, sameHost: true, status: http.StatusMovedPermanently},
		{name: "from https to http", apiTLS: true, status: http.StatusTemporaryRedirect, refusal: "it leaves TLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := glCanary(t)
			var seen hops
			other := startServer(t, tlsIf(tc.otherTLS), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.record("localhost", r)
				projectList(w, "https://gitlab.com/mygroup/myrepo.git")
			}))
			otherOrigin := onLocalhost(t, other)
			target := otherOrigin + "/landed?X-Amz-Signature=" + strings.TrimPrefix(glCanary(t), "glpat-canary")
			if tc.sameHost {
				target = "/here"
			}
			api := startServer(t, tlsIf(tc.apiTLS), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.record("api", r)
				switch r.URL.Path {
				case "/api/v4/projects":
					http.Redirect(w, r, target, tc.status)
				case "/here":
					projectList(w, "https://gitlab.com/mygroup/myrepo.git")
				default:
					http.NotFound(w, r)
				}
			}))

			s, err := newSource(Options{BaseURL: api.URL, Token: tok}, nil, api.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			repos, err := s.ListRepos(context.Background(), source.Filter{})

			got := seen.all()
			if len(got) == 0 || got[0].server != "api" {
				t.Fatalf("the first request did not reach the API: %+v", got)
			}
			// The token goes to the API itself, or this test proves nothing.
			if v := got[0].header.Get("Private-Token"); v != tok {
				t.Errorf("the API was sent PRIVATE-TOKEN %q, want the token", v)
			}

			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("ListRepos: %v", err)
				}
				if len(repos) != 1 || len(got) != 2 || got[1].path != "/here" {
					t.Fatalf("got %d repositories over %d requests, want 1 over 2, the second to /here: %+v", len(repos), len(got), got)
				}
				if v := got[1].header.Get("Private-Token"); v != tok {
					t.Errorf("the redirect on the API host was sent PRIVATE-TOKEN %q, want the token", v)
				}
				return
			}
			assertRefused(t, "ListRepos", err, api.URL, otherOrigin, tc.refusal, tok)
			if len(repos) != 0 {
				t.Errorf("ListRepos returned %d repositories after refusing the redirect", len(repos))
			}
			if len(got) != 1 {
				t.Errorf("%d requests were sent, want only the first, to the API: %+v", len(got), got)
			}
		})
	}
}

// The answer from a host the API redirected to is never read. Whatever answers there could list
// projects whose clone URLs name its own host, and git sends the token to the clone URL's host, so
// a listing it answered would hand it the token at clone time. The listing fails instead, and
// nothing is sent there.
func TestAnAnswerFromOffTheAPIIsNeverRead(t *testing.T) {
	cert := localhostTLS(t)
	var reached atomic.Int32
	other := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		projectList(w, "https://"+r.Host+"/mygroup/myrepo.git")
	}))
	forged := onLocalhost(t, other)
	api := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, forged+"/api/v4/projects", http.StatusFound)
	}))
	s, err := newSource(Options{BaseURL: api.URL, Token: glCanary(t)}, nil, api.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := s.ListRepos(context.Background(), source.Filter{})
	if wantErr := "refusing a redirect from " + api.URL + " to " + forged; err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Errorf("ListRepos = %v; want it to say %q", err, wantErr)
	}
	for _, r := range repos {
		t.Errorf("listed %s, to be cloned from %s", r.Slug(), r.CloneURL)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d requests reached localhost", n)
	}
}

// Metadata reaches the API by two paths, one request for the project and pages for every list, and
// a redirect off the API fails either one the same way.
func TestFetchMetadataRefusesARedirectOffTheAPI(t *testing.T) {
	cert := localhostTLS(t)
	for _, tc := range []struct{ name, redirected, wantPrefix string }{
		{name: "the project", redirected: "/api/v4/projects/mygroup/myrepo", wantPrefix: "gitlab: get project mygroup/myrepo: "},
		{name: "a list", redirected: "/api/v4/projects/mygroup/myrepo/labels", wantPrefix: "gitlab: labels: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := glCanary(t)
			var reached atomic.Int32
			other := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				projectList(w, "https://"+r.Host+"/mygroup/myrepo.git")
			}))
			otherOrigin := onLocalhost(t, other)
			api := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The first case that matches wins, so the redirected path is redirected even when it
				// is the project's own.
				switch r.URL.Path {
				case tc.redirected:
					http.Redirect(w, r, otherOrigin+"/landed?sig=1", http.StatusFound)
				case "/api/v4/projects/mygroup/myrepo":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"path":"myrepo","namespace":{"full_path":"mygroup"}}`))
				default:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			s, err := newSource(Options{BaseURL: api.URL, Token: tok}, nil, api.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.FetchMetadata(context.Background(), source.Repo{Host: "gitlab.com", Owner: "mygroup", Name: "myrepo"})
			assertRefused(t, "FetchMetadata", err, api.URL, otherOrigin, "it leaves the API", tok)
			if err != nil && !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Errorf("FetchMetadata = %v; want it to start %q", err, tc.wantPrefix)
			}
			if n := reached.Load(); n != 0 {
				t.Errorf("%d requests reached localhost", n)
			}
		})
	}
}

// recordingTransport stands in for the network: it keeps each request it is handed and answers
// 200 with an empty body.
type recordingTransport struct{ reqs []*http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, req)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// Every header client-go can put a credential in stays on a request to the API and comes off any
// other, and so does every header it might add later: only Accept and User-Agent go elsewhere.
func TestAPIOnlyKeepsCredentialsOnTheAPI(t *testing.T) {
	headers := http.Header{
		"Private-Token": {"glpat-secret"},                   // an access token
		"Job-Token":     {"glcbt-secret"},                   // a CI job token
		"Authorization": {"Bearer secret"},                  // an OAuth token
		"Sudo":          {"root"},                           // WithSudo: meaningful only with an admin token
		"Cookie":        {"_gitlab_session=secret"},         // a session
		"X-Next-Token":  {"what a later version might add"}, // anything else
		"Accept":        {"application/json"},
		"User-Agent":    {"go-gitlab"},
	}
	for _, tc := range []struct {
		name, url string
		onAPI     bool
	}{
		{name: "the API", url: "https://gitlab.example.com/api/v4/projects", onAPI: true},
		{name: "another host", url: "https://elsewhere.example.net/landed"},
		{name: "a host that starts with the API host", url: "https://gitlab.example.com.example.net/api/v4/projects"},
		{name: "the API host on another port", url: "https://gitlab.example.com:8443/api/v4/projects"},
		{name: "the API host over plain http", url: "http://gitlab.example.com/api/v4/projects"},
		{name: "the API host spelled in capitals", url: "https://GITLAB.EXAMPLE.COM/api/v4/projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := &recordingTransport{}
			tr := &apiOnly{next: network, scheme: "https", host: "gitlab.example.com"}
			req, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = headers.Clone()
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if len(network.reqs) != 1 {
				t.Fatalf("%d requests went out, want 1", len(network.reqs))
			}
			sent := network.reqs[0].Header
			for name := range headers {
				want := headers.Get(name)
				if !tc.onAPI && name != "Accept" && name != "User-Agent" {
					want = ""
				}
				if got := sent.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			// The caller's request is its own: a RoundTripper must not modify it.
			if len(req.Header) != len(headers) || req.Header.Get("Private-Token") == "" {
				t.Errorf("the caller's request was changed: %v", req.Header)
			}
		})
	}
}
