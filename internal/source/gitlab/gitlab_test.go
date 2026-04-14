package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/source"
)

func TestListReposAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"path":             "myrepo",
				"http_url_to_repo": "https://gitlab.com/mygroup/myrepo.git",
				"default_branch":   "main",
				"archived":         false,
				"namespace":        map[string]any{"full_path": "mygroup"},
			}})
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	s, err := New(Options{BaseURL: srv.URL, Token: "glpat-test"}, nil)
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
	if repos[0].Owner != "mygroup" || repos[0].Name != "myrepo" {
		t.Fatalf("unexpected repo %+v", repos[0])
	}
	if repos[0].CloneURL != "https://gitlab.com/mygroup/myrepo.git" {
		t.Fatalf("clone url %q", repos[0].CloneURL)
	}

	header, err := s.GitAuthHeader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "Authorization: Basic "
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("header = %q", header)
	}
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if string(dec) != "oauth2:glpat-test" {
		t.Fatalf("decoded creds = %q", dec)
	}
}

func TestKeepFilter(t *testing.T) {
	r := source.Repo{Owner: "mygroup", Name: "myrepo"}
	cases := []struct {
		name string
		f    source.Filter
		want bool
	}{
		{"none", source.Filter{}, true},
		{"glob", source.Filter{Include: []string{"mygroup/*"}}, true},
		{"miss", source.Filter{Include: []string{"other"}}, false},
		{"exclude", source.Filter{Exclude: []string{"mygroup/myrepo"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keep(r, tc.f); got != tc.want {
				t.Fatalf("keep = %v, want %v", got, tc.want)
			}
		})
	}
}
