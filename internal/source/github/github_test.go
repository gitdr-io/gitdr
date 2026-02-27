package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/source"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_testtoken",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation/repositories"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"repositories": []map[string]any{{
					"name":           "hello",
					"clone_url":      "https://github.com/octo/hello.git",
					"default_branch": "main",
					"archived":       false,
					"size":           123,
					"owner":          map[string]any{"login": "octo"},
				}},
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestListReposAndAuth(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()

	s, err := New(Options{
		BaseURL:        srv.URL,
		AppID:          1,
		InstallationID: 123,
		PrivateKeyPEM:  testKeyPEM(t),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	repos, err := s.ListRepos(context.Background(), source.Filter{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].Owner != "octo" || repos[0].Name != "hello" {
		t.Fatalf("unexpected repo %+v", repos[0])
	}
	if repos[0].CloneURL != "https://github.com/octo/hello.git" {
		t.Fatalf("clone url %q", repos[0].CloneURL)
	}

	header, err := s.GitAuthHeader(context.Background())
	if err != nil {
		t.Fatalf("GitAuthHeader: %v", err)
	}
	const prefix = "Authorization: Basic "
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("header = %q", header)
	}
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if string(dec) != "x-access-token:ghs_testtoken" {
		t.Fatalf("decoded creds = %q", dec)
	}
}

func TestKeepFilter(t *testing.T) {
	r := source.Repo{Owner: "octo", Name: "hello"}
	cases := []struct {
		name   string
		filter source.Filter
		want   bool
	}{
		{"no filter", source.Filter{}, true},
		{"include slug", source.Filter{Include: []string{"octo/hello"}}, true},
		{"include name", source.Filter{Include: []string{"hello"}}, true},
		{"include glob", source.Filter{Include: []string{"octo/*"}}, true},
		{"include miss", source.Filter{Include: []string{"other"}}, false},
		{"exclude glob", source.Filter{Exclude: []string{"octo/*"}}, false},
		{"exclude wins", source.Filter{Include: []string{"hello"}, Exclude: []string{"octo/hello"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keep(r, tc.filter); got != tc.want {
				t.Fatalf("keep = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostFromURL(t *testing.T) {
	if h := hostFromURL("https://ghe.example.com/api/v3"); h != "ghe.example.com" {
		t.Fatalf("host = %q", h)
	}
}
