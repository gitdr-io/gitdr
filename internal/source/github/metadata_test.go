package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if _, ok := doc["unavailable"]; ok {
		t.Error("metadata names unavailable sections when GitHub answered every one")
	}
}

// A section GitHub answers with 404 or 410 is a feature turned off for the repository, and it is
// recorded as such instead of failing the repository.
//
// A mirror with issues and pull requests turned off, torvalds/linux and postgres/postgres among
// them, answers 404 on its pull requests and on its issue comments, and so failed on every run.
// Any other refusal, a permission the installation does not have among them, still fails.
func TestATurnedOffSectionIsRecordedNotFailed(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name   string
		refuse map[string]reply // by the end of the path refused
		// Recorded as unavailable, by section; or, when wantErr is set, what the error says.
		want    map[string]string
		wantErr string
	}{
		{
			name:   "issues and pull requests turned off, as GitHub answers it",
			refuse: map[string]reply{"/issues/comments": failure(http.StatusNotFound), "/pulls": failure(http.StatusNotFound)},
			want:   map[string]string{"comments": "404 Not Found", "pullRequests": "404 Not Found"},
		},
		{
			name:   "issues turned off, answered 410",
			refuse: map[string]reply{"/issues": {status: http.StatusGone, body: `{"message":"Issues are disabled for this repo"}`}},
			want:   map[string]string{"issues": "410 Issues are disabled for this repo"},
		},
		{
			name:    "a permission the installation does not have",
			refuse:  map[string]reply{"/issues": {status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`}},
			wantErr: "403 Resource not accessible by integration",
		},
		{
			name:    "the repository itself not found",
			refuse:  map[string]reply{"/repos/octo/hello": failure(http.StatusNotFound)},
			wantErr: "get repo octo/hello",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			api := newFakeAPI(t, clock, func(r *http.Request, _ int) reply {
				for end, answer := range tc.refuse {
					if strings.HasSuffix(r.URL.Path, end) {
						return answer
					}
				}
				return metadataReply(r)
			})
			s := clockedSource(t, api, key, clock)

			b, err := s.FetchMetadata(context.Background(), source.Repo{Host: "github.com", Owner: "octo", Name: "hello"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("FetchMetadata = %v, want an error saying %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchMetadata: %v", err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("invalid metadata json: %v", err)
			}
			var got map[string]string
			if err := json.Unmarshal(doc["unavailable"], &got); err != nil {
				t.Fatalf("unavailable = %s: %v", doc["unavailable"], err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("unavailable = %v, want %v", got, tc.want)
			}
			for _, k := range []string{"repo", "labels", "milestones", "issues", "comments", "pullRequests", "reviewComments", "releases"} {
				v, ok := doc[k]
				if !ok {
					t.Errorf("metadata missing section %q", k)
				} else if _, off := tc.want[k]; off && string(v) != "null" {
					t.Errorf("unavailable section %q holds %s, want it empty", k, v)
				}
			}
		})
	}
}
