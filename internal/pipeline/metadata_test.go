package pipeline_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
	ghsrc "gitdr.io/gitdr/internal/source/github"
)

// failingMetadata is a fixture source whose metadata cannot be fetched for one repository.
type failingMetadata struct {
	*fixtureSource
	fails string // the name of that repository
	err   error  // what fetching its metadata returns
}

func (s *failingMetadata) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	if r.Name == s.fails {
		return nil, s.err
	}
	return s.fixtureSource.FetchMetadata(ctx, r)
}

// Metadata is fetched before anything of a repository is written, and what a failure costs the
// repository depends on whether it is likely to pass.
//
// A transient one, a rate limit that could not be waited out, fails the repository with nothing
// stored, because the next run will probably get through. Fetched after the bundle, as it used to
// be, it left a bundle under object lock with no metadata beside it. Any other failure would fail
// every run the same way, so the code is stored regardless, bundle and checksum, and the repository
// fails on its metadata as it always did. Holding the code back on every metadata failure meant a
// repository with a permission missing never had its code copied at all.
func TestAMetadataFailureStoresTheCodeUnlessItIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string // what is stored for the repository whose metadata failed
	}{
		{
			name: "a rate limit that could not be waited out",
			err:  source.Transient(errors.New("github: issues: rate limited until 2026-06-13T13:00:00Z, past the deadline of 2026-06-13T12:30:00Z")),
		},
		{
			name: "a refusal a retry would not change",
			err:  errors.New("github: issues: GET .../issues: 403 Resource not accessible by integration"),
			want: []string{"broken.bundle", "broken.sha256"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			src := &failingMetadata{
				fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/broken", "octo/fine")},
				fails:         "broken",
				err:           tc.err,
			}
			md := newMemDest(true)
			cfg := testConfig()
			cfg.Source.Repo = ""

			res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
				Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
				SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
			})
			if err == nil {
				t.Fatal("a run in which a repository's metadata could not be fetched reported success")
			}
			if res == nil || res.Manifest == nil {
				t.Fatalf("the failed run wrote no manifest: %v", err)
			}
			entries := map[string]pipeline.RepoEntry{}
			for _, e := range res.Manifest.Repos {
				entries[e.Slug] = e
			}

			broken := entries["octo/broken"]
			if broken.Status != pipeline.StatusFailed || !strings.Contains(broken.Error, "metadata") {
				t.Errorf("octo/broken = %s %q, want failed on its metadata", broken.Status, broken.Error)
			}
			if got := storedNames(md, "github.com/octo/broken/"); !slices.Equal(got, tc.want) {
				t.Errorf("stored %v for the repository whose metadata failed, want %v", got, tc.want)
			}
			if len(broken.Artifacts) != len(tc.want) {
				t.Errorf("octo/broken records %d artifacts, want %d, one for each object stored", len(broken.Artifacts), len(tc.want))
			}

			// The other repository is copied in full, so the run did write, and none of the above
			// passes for a run that wrote nothing at all.
			if fine := entries["octo/fine"]; fine.Status != pipeline.StatusSuccess {
				t.Errorf("octo/fine = %s %q, want success", fine.Status, fine.Error)
			}
			if got := storedNames(md, "github.com/octo/fine/"); len(got) != 3 {
				t.Errorf("stored %v for octo/fine, want its bundle, metadata and checksum", got)
			}
		})
	}
}

// What a refusal from GitHub costs a repository, through the real GitHub source and a fake API, so
// the pipeline sees exactly the errors and documents GitHub's answers produce.
//
// A feature turned off is no failure at all: the repository is copied in full, and its metadata
// says which section GitHub would not give. A refusal for good, a permission the App lacks, still
// stores the code. Only a limit that will lift later leaves the repository with nothing stored.
func TestWhatAGitHubRefusalCostsTheRepository(t *testing.T) {
	key := appKey(t)
	limited := map[string]string{
		"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10),
	}
	for _, tc := range []struct {
		name string
		// The endpoint GitHub refuses, by the end of its path, and how.
		end    string
		status int
		header map[string]string
		body   string
		// What the run records and stores for the repository.
		want   string
		stored []string
		// The section the metadata names as unavailable, and GitHub's answer recorded for it.
		section, unavailable string
	}{
		{
			name: "issues turned off, answered 410", end: "/issues", status: http.StatusGone,
			body: `{"message":"Issues are disabled for this repo"}`,
			want: pipeline.StatusSuccess, stored: []string{"hello.bundle", "hello.meta.json", "hello.sha256"},
			section: "issues", unavailable: "410 Issues are disabled for this repo",
		},
		{
			name: "pull requests turned off, answered 404", end: "/pulls", status: http.StatusNotFound,
			body: `{"message":"Not Found"}`,
			want: pipeline.StatusSuccess, stored: []string{"hello.bundle", "hello.meta.json", "hello.sha256"},
			section: "pullRequests", unavailable: "404 Not Found",
		},
		{
			name: "a permission the App does not have, answered 403", end: "/issues", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration"}`,
			want: pipeline.StatusFailed, stored: []string{"hello.bundle", "hello.sha256"},
		},
		{
			name: "a rate limit that lifts after the deadline", end: "/issues", status: http.StatusForbidden,
			header: limited, body: `{"message":"API rate limit exceeded for installation ID 123."}`,
			want: pipeline.StatusFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
				case strings.HasSuffix(r.URL.Path, tc.end):
					for k, v := range tc.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case strings.HasSuffix(r.URL.Path, "/repos/octo/hello"):
					_, _ = io.WriteString(w, `{"name":"hello","owner":{"login":"octo"}}`)
				default: // every other section, read and empty
					_, _ = io.WriteString(w, `[]`)
				}
			}))
			defer api.Close()
			github, err := ghsrc.New(ghsrc.Options{BaseURL: api.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: key}, nil)
			if err != nil {
				t.Fatal(err)
			}
			src := &githubMetadata{
				fixtureSource: &fixtureSource{repos: slugRepos("github.com", initFixtureRepo(t), "octo/hello")},
				github:        github,
			}
			md := newMemDest(true)
			// A minute: the rate limit lifts in thirty, so the source must not wait for it.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
				Config: testConfig(), Source: src, Dest: md, Git: gitexec.New(nil),
				SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
			})
			if (err == nil) != (tc.want == pipeline.StatusSuccess) {
				t.Fatalf("backup error = %v, want the repository %s", err, tc.want)
			}
			if res == nil || res.Manifest == nil {
				t.Fatalf("no manifest: %v", err)
			}
			entry := res.Manifest.Repos[0]
			if entry.Status != tc.want {
				t.Errorf("octo/hello = %s %q, want %s", entry.Status, entry.Error, tc.want)
			}
			if got := storedNames(md, "github.com/octo/hello/"); !slices.Equal(got, tc.stored) {
				t.Errorf("stored %v, want %v", got, tc.stored)
			}
			if tc.section == "" {
				return
			}
			var meta struct {
				Unavailable map[string]string `json:"unavailable"`
			}
			if err := json.Unmarshal(md.objs["github.com/octo/hello/2026-06-13/hello.meta.json"], &meta); err != nil {
				t.Fatalf("stored metadata: %v", err)
			}
			if got := meta.Unavailable[tc.section]; got != tc.unavailable {
				t.Errorf("the metadata records %s as %q, want %q", tc.section, got, tc.unavailable)
			}
		})
	}
}

// githubMetadata lists a local repository and fetches its metadata from a real GitHub source.
type githubMetadata struct {
	*fixtureSource
	github *ghsrc.Source
}

func (s *githubMetadata) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	return s.github.FetchMetadata(ctx, r)
}

// appKey is a GitHub App private key, for a source that mints its token from a fake API.
func appKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// storedNames lists, sorted, the names of the objects md holds under prefix.
func storedNames(md *memDest, prefix string) []string {
	md.mu.Lock()
	defer md.mu.Unlock()
	var names []string
	for key := range md.objs {
		if strings.HasPrefix(key, prefix) {
			names = append(names, path.Base(key))
		}
	}
	slices.Sort(names)
	return names
}
