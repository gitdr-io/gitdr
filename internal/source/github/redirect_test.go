package github

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

// A redirect is followed on the API and nowhere else, in either mode. A hop on the API host keeps
// the token. A hop to another host, or from https to plain http, fails before anything is sent
// there, and the error names the two hosts and nothing more of either URL: where a redirect points
// is whoever sent it's to choose, and its query string can carry anything. Real servers, and Go's
// own client following the redirects.
func TestARedirectNeverTakesTheTokenOffTheAPI(t *testing.T) {
	cert := localhostTLS(t)
	key := testKeyPEM(t)
	tlsIf := func(on bool) *tls.Config {
		if on {
			return cert
		}
		return nil
	}

	for _, mode := range []string{"App", "token file"} {
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
			t.Run(mode+", "+tc.name, func(t *testing.T) {
				tok := canaryToken(t)
				// A query string a redirect might carry, a signature say, which no error may repeat.
				sig := strings.TrimPrefix(canaryToken(t), "ghs_canary")
				var seen hops
				list := func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"total_count": 1,
						"repositories": []map[string]any{{
							"name": "hello", "clone_url": "https://github.com/octo/hello.git", "owner": map[string]any{"login": "octo"},
						}},
					})
				}
				other := startServer(t, tlsIf(tc.otherTLS), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen.record("localhost", r)
					list(w)
				}))
				otherOrigin := onLocalhost(t, other)
				target := otherOrigin + "/landed?X-Amz-Signature=" + sig
				if tc.sameHost {
					target = "/here"
				}
				api := startServer(t, tlsIf(tc.apiTLS), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"token": tok, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
						})
						return
					}
					seen.record("api", r)
					switch r.URL.Path {
					case "/api/v3/installation/repositories":
						http.Redirect(w, r, target, tc.status)
					case "/here":
						list(w)
					default:
						http.NotFound(w, r)
					}
				}))

				opts := Options{BaseURL: api.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: key}
				want := "token " + tok
				if mode == "token file" {
					path := filepath.Join(t.TempDir(), "github-token")
					replaceTokenFile(t, path, tok)
					opts = Options{BaseURL: api.URL, TokenPath: path}
					want = "Bearer " + tok
				}
				s, err := newSource(opts, nil, api.Client().Transport)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				repos, err := s.ListRepos(ctx, source.Filter{})

				got := seen.all()
				if len(got) == 0 || got[0].server != "api" {
					t.Fatalf("the first request did not reach the API: %+v", got)
				}
				// The token goes to the API itself, or this test proves nothing. A token file is
				// never sent over plain http, even to the API.
				wantAtAPI := want
				if mode == "token file" && !tc.apiTLS {
					wantAtAPI = ""
				}
				if auth := got[0].header.Get("Authorization"); auth != wantAtAPI {
					t.Errorf("the API was sent Authorization %q, want %q", auth, wantAtAPI)
				}

				if tc.refusal == "" {
					if err != nil {
						t.Fatalf("ListRepos: %v", err)
					}
					if len(repos) != 1 || len(got) != 2 || got[1].path != "/here" {
						t.Fatalf("got %d repositories over %d requests, want 1 over 2, the second to /here: %+v", len(repos), len(got), got)
					}
					if auth := got[1].header.Get("Authorization"); auth != want {
						t.Errorf("the redirect on the API host was sent Authorization %q, want %q", auth, want)
					}
					return
				}

				wantErr := "refusing a redirect from " + api.URL + " to " + otherOrigin + ": " + tc.refusal
				errs := map[string]error{"ListRepos": err}
				if mode == "token file" {
					errs["CheckToken"] = s.CheckToken(ctx)
				}
				for call, err := range errs {
					if err == nil || !strings.Contains(err.Error(), wantErr) {
						t.Errorf("%s = %v; want it to say %q", call, err, wantErr)
						continue
					}
					if strings.Contains(err.Error(), "?") || strings.Contains(err.Error(), sig) {
						t.Errorf("%s's error shows where the redirect pointed beyond its host: %v", call, err)
					}
					assertNoCanary(t, call+"'s error", renderEveryWay(err), tok)
				}
				if len(repos) != 0 {
					t.Errorf("ListRepos returned %d repositories after refusing the redirect", len(repos))
				}
				for _, h := range seen.all() {
					if h.server == "localhost" {
						t.Errorf("a request reached localhost: %s", h.path)
					}
				}
			})
		}
	}
}

// The answer from a host the API redirected to is never read. Whatever answers there could list
// repositories whose clone URLs name its own host, and git sends the token to the clone URL's host,
// so a listing it answered would hand it the token at clone time. The listing fails instead, and
// nothing is sent there.
func TestAnAnswerFromOffTheAPIIsNeverRead(t *testing.T) {
	cert := localhostTLS(t)
	key := testKeyPEM(t)
	for _, mode := range []string{"App", "token file"} {
		t.Run(mode, func(t *testing.T) {
			tok := canaryToken(t)
			var reached atomic.Int32
			other := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"total_count": 1,
					"repositories": []map[string]any{{
						"name": "hello", "clone_url": "https://" + r.Host + "/octo/hello.git", "owner": map[string]any{"login": "octo"},
					}},
				})
			}))
			forged := onLocalhost(t, other)
			api := startServer(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"token": tok, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					})
					return
				}
				http.Redirect(w, r, forged+"/installation/repositories", http.StatusFound)
			}))

			opts := Options{BaseURL: api.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: key}
			if mode == "token file" {
				path := filepath.Join(t.TempDir(), "github-token")
				replaceTokenFile(t, path, tok)
				opts = Options{BaseURL: api.URL, TokenPath: path}
			}
			s, err := newSource(opts, nil, api.Client().Transport)
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
		})
	}
}

// In App mode only a request to the API base's scheme and host is handed to the App's transport,
// which adds the token. Everything else goes to the network with no Authorization header.
func TestAppTransportAuthenticatesOnlyTheAPIHost(t *testing.T) {
	for _, tc := range []struct {
		name, scheme, host, url string
		// A header already on the request, which must not survive a request that goes out bare.
		preset string
		toApp  bool
	}{
		{name: "github.com over https", scheme: "https", host: "api.github.com", url: "https://api.github.com/installation/repositories", toApp: true},
		{name: "github.com over plain http", scheme: "https", host: "api.github.com", url: "http://api.github.com/installation/repositories"},
		{name: "another github.com host", scheme: "https", host: "api.github.com", url: "https://uploads.github.com/repos/o/r/releases"},
		{name: "a host that starts with the API host", scheme: "https", host: "api.github.com", url: "https://api.github.com.example.net/installation/repositories"},
		{name: "the API host on another port", scheme: "https", host: "api.github.com", url: "https://api.github.com:8443/installation/repositories"},
		{name: "the API host spelled in capitals", scheme: "https", host: "api.github.com", url: "https://API.GITHUB.COM/installation/repositories"},
		{name: "another host, carrying a header already", scheme: "https", host: "api.github.com", url: "https://example.net/", preset: "token stale"},
		{name: "Enterprise Server over https", scheme: "https", host: "ghe.example.com", url: "https://ghe.example.com/api/v3/installation/repositories", toApp: true},
		{name: "github.com, from an Enterprise Server source", scheme: "https", host: "ghe.example.com", url: "https://api.github.com/installation/repositories"},
		// A base of plain http is the operator's choice, and the token is minted over it too.
		{name: "a plain http base, over plain http", scheme: "http", host: "ghe.internal:8080", url: "http://ghe.internal:8080/api/v3/installation/repositories", toApp: true},
		{name: "a plain http base, over https", scheme: "http", host: "ghe.internal:8080", url: "https://ghe.internal:8080/api/v3/installation/repositories"},
		{name: "a base URL with no host", url: "https:///installation/repositories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, network := &recordingTransport{}, &recordingTransport{}
			tr := &appTransport{app: app, base: network, scheme: tc.scheme, host: tc.host}
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

			if tc.toApp {
				if len(app.reqs) != 1 || len(network.reqs) != 0 {
					t.Fatalf("the App's transport got %d requests and the network %d, want 1 and 0", len(app.reqs), len(network.reqs))
				}
				return
			}
			if len(app.reqs) != 0 || len(network.reqs) != 1 {
				t.Fatalf("the App's transport got %d requests and the network %d, want 0 and 1", len(app.reqs), len(network.reqs))
			}
			if got := network.reqs[0].Header.Get("Authorization"); got != "" {
				t.Errorf("a request off the API went out with Authorization %q", got)
			}
			// The caller's request is its own: a RoundTripper must not modify it.
			if got := req.Header.Get("Authorization"); got != tc.preset {
				t.Errorf("the caller's request was changed: Authorization = %q", got)
			}
		})
	}
}
