package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/source"
)

func TestFetchMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// GetProject hits .../projects/mygroup%2Fmyrepo (decoded to .../mygroup/myrepo);
		// every list endpoint adds a further path segment.
		if strings.HasSuffix(r.URL.Path, "projects/mygroup/myrepo") {
			_, _ = w.Write([]byte(`{"path":"myrepo","path_with_namespace":"mygroup/myrepo"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	s, err := New(Options{BaseURL: srv.URL, Token: "glpat-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.FetchMetadata(context.Background(), source.Repo{Host: "gitlab.com", Owner: "mygroup", Name: "myrepo"})
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid metadata json: %v", err)
	}
	for _, k := range []string{"schema", "host", "owner", "name", "fetchedAt", "project", "labels", "milestones", "issues", "mergeRequests", "releases", "notes"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("metadata missing key %q", k)
		}
	}
}
