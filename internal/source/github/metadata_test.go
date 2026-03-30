package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/source"
)

func TestFetchMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case strings.HasSuffix(r.URL.Path, "/repos/octo/hello"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "hello", "owner": map[string]any{"login": "octo"}})
		default: // every paginated list endpoint
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	s, err := New(Options{BaseURL: srv.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: testKeyPEM(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.FetchMetadata(context.Background(), source.Repo{Host: "github.com", Owner: "octo", Name: "hello"})
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid metadata json: %v", err)
	}
	for _, k := range []string{"schema", "host", "owner", "name", "fetchedAt", "repo", "labels", "milestones", "issues", "comments", "pullRequests", "reviewComments", "releases"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("metadata missing key %q", k)
		}
	}
}
