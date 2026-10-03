package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"

	"gitdr.io/gitdr/internal/source"
)

// fakeClock is the clock the source measures a rate-limit wait on. It moves only when the source
// sleeps, by exactly the sleep, and keeps every sleep, so a test can say how long a wait was
// without serving it.
type fakeClock struct {
	mu    sync.Mutex
	at    time.Time
	slept []time.Duration
}

// newFakeClock starts at the current second: whole seconds, like the reset GitHub sends, so a
// wait can be asserted to the second, and now, because a context's deadline is on the real clock.
func newFakeClock() *fakeClock { return &fakeClock{at: time.Now().Truncate(time.Second)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.at = c.at.Add(d)
	return ctx.Err()
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// reply is one answer from the fake API.
type reply struct {
	status int
	header map[string]string
	body   string
}

// primaryLimit is GitHub refusing a request because the installation's hourly limit is spent.
func primaryLimit(status int, reset time.Time) reply {
	return reply{status: status, header: map[string]string{
		"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10),
	}, body: `{"message":"API rate limit exceeded for installation ID 123.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-the-rate-limit"}`}
}

// secondaryLimit is GitHub refusing a request for going too fast, with a Retry-After unless
// retryAfter is empty.
func secondaryLimit(status int, retryAfter string) reply {
	r := reply{status: status, body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`}
	if retryAfter != "" {
		r.header = map[string]string{"Retry-After": retryAfter}
	}
	return r
}

func failure(status int) reply {
	return reply{status: status, body: `{"message":"` + http.StatusText(status) + `"}`}
}

// repoPage is one page of the installation's repositories, linking to page next unless it is 0.
func repoPage(next int, names ...string) reply {
	repos := make([]map[string]any, 0, len(names))
	for _, n := range names {
		repos = append(repos, map[string]any{"name": n, "owner": map[string]any{"login": "octo"}})
	}
	b, _ := json.Marshal(map[string]any{"total_count": len(names), "repositories": repos})
	r := reply{status: http.StatusOK, body: string(b)}
	if next != 0 {
		r.header = map[string]string{"Link": fmt.Sprintf(`<https://api.github.com/installation/repositories?page=%d>; rel="next"`, next)}
	}
	return r
}

// inOrder answers the n-th request to a path with the n-th reply.
func inOrder(replies ...reply) func(*http.Request, int) reply {
	return func(_ *http.Request, n int) reply {
		if n < len(replies) {
			return replies[n]
		}
		return failure(http.StatusTeapot) // a request nobody expected, answered with nothing worth retrying
	}
}

// metadataReply answers the metadata endpoints: the repository itself, and an empty page from
// every list.
func metadataReply(r *http.Request) reply {
	if strings.HasSuffix(r.URL.Path, "/repos/octo/hello") {
		return reply{status: http.StatusOK, body: `{"name":"hello","owner":{"login":"octo"}}`}
	}
	return reply{status: http.StatusOK, body: `[]`}
}

// fakeAPI mints installation tokens, and answers every other request with what answer says for
// it, given how many requests to the same path came before it. Its Date is clock's time, as if
// GitHub's clock and the source's agreed, unless the reply sets one.
type fakeAPI struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
	seen  map[string]int
	// mints counts the requests for an installation token; the first mintFailures of them are
	// answered with mintStatus instead of a token.
	mints, mintFailures, mintStatus int
}

func newFakeAPI(t *testing.T, clock *fakeClock, answer func(r *http.Request, n int) reply) *fakeAPI {
	t.Helper()
	f := &fakeAPI{seen: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", clock.now().UTC().Format(http.TimeFormat))
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			f.mu.Lock()
			f.mints++
			refuse, status := f.mints <= f.mintFailures, f.mintStatus
			f.mu.Unlock()
			if refuse {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"` + http.StatusText(status) + `"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			return
		}
		f.mu.Lock()
		n := f.seen[r.URL.Path]
		f.seen[r.URL.Path]++
		f.asked = append(f.asked, r.URL.Path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		a := answer(r, n)
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(f.Close)
	return f
}

// failMints answers the first n requests for an installation token with status.
func (f *fakeAPI) failMints(n, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintFailures, f.mintStatus = n, status
}

// minted returns how many installation tokens were asked for.
func (f *fakeAPI) minted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// requests returns every API request so far, as path and query.
func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// count returns how many requests reached a path ending in suffix.
func (f *fakeAPI) count(suffix string) int {
	n := 0
	for _, r := range f.requests() {
		if p, _, _ := strings.Cut(r, "?"); strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

// clockedSource is an App-mode source over api that measures and spends its waits on clock.
func clockedSource(t *testing.T, api *fakeAPI, key []byte, clock *fakeClock) *Source {
	t.Helper()
	s, err := New(Options{BaseURL: api.URL, AppID: 1, InstallationID: 123, PrivateKeyPEM: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.now, s.sleep = clock.now, clock.sleep
	return s
}

// slugs lists repos by owner/name.
func slugs(repos []source.Repo) string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Slug())
	}
	return strings.Join(out, ",")
}

// A rate limit is waited out, for as long as GitHub said, and the request is then sent again.
//
// Before, go-github's error was returned as it came, so the listing that met a limit failed the
// whole run. The wait is asserted from what GitHub sent, not from what the code computes: the
// source may add up to two seconds, for the reset's whole-second resolution and the jitter that
// keeps the requests that waited together from returning together.
func TestListReposWaitsOutARateLimit(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name  string
		limit func(now time.Time) reply
		want  time.Duration // what GitHub asked for
	}{
		{"a primary limit, as a 403", func(now time.Time) reply { return primaryLimit(http.StatusForbidden, now.Add(30*time.Minute)) }, 30 * time.Minute},
		{"a primary limit, as a 429", func(now time.Time) reply { return primaryLimit(http.StatusTooManyRequests, now.Add(30*time.Minute)) }, 30 * time.Minute},
		{"a secondary limit, as a 403 with Retry-After", func(time.Time) reply { return secondaryLimit(http.StatusForbidden, "90") }, 90 * time.Second},
		{"a secondary limit, as a 429 with Retry-After", func(time.Time) reply { return secondaryLimit(http.StatusTooManyRequests, "90") }, 90 * time.Second},
		// GitHub's advice for a secondary limit that names no time: wait at least a minute.
		{"a secondary limit with no Retry-After", func(time.Time) reply { return secondaryLimit(http.StatusForbidden, "") }, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			api := newFakeAPI(t, clock, inOrder(tc.limit(clock.now()), repoPage(0, "hello")))
			s := clockedSource(t, api, key, clock)

			repos, err := s.ListRepos(context.Background(), source.Filter{})
			if err != nil {
				t.Fatalf("ListRepos: %v", err)
			}
			if got := slugs(repos); got != "octo/hello" {
				t.Errorf("repos = %s, want octo/hello", got)
			}
			if n := len(api.requests()); n != 2 {
				t.Errorf("%d requests, want the refused one and its retry: %v", n, api.requests())
			}
			slept := clock.sleeps()
			if len(slept) != 1 {
				t.Fatalf("waited %v, want one wait", slept)
			}
			if slept[0] < tc.want || slept[0] > tc.want+2*time.Second {
				t.Errorf("waited %s, want %s and at most two seconds more", slept[0], tc.want)
			}
		})
	}
}

// A 5xx on a page of the listing is retried with backoff, a bounded number of times. Before, one
// failed the listing and with it the whole run.
func TestListReposRetriesAServerError(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name     string
		replies  []reply
		want     string // the repositories listed; empty means the listing fails
		requests int
		// The backoff before each retry: at least this, and less than a second more.
		backoff []time.Duration
	}{
		{
			name:     "a 502 and a 503, then the list",
			replies:  []reply{failure(http.StatusBadGateway), failure(http.StatusServiceUnavailable), repoPage(0, "hello")},
			want:     "octo/hello",
			requests: 3,
			backoff:  []time.Duration{time.Second, 2 * time.Second},
		},
		{
			name:     "a 503 on the second page retries that page",
			replies:  []reply{repoPage(2, "a"), failure(http.StatusServiceUnavailable), repoPage(0, "b")},
			want:     "octo/a,octo/b",
			requests: 3,
			backoff:  []time.Duration{time.Second},
		},
		{
			// Four tries, then the listing fails with what GitHub said. The count is written out
			// rather than read from the code, so a change to it is a change to this test.
			name:     "a 5xx that does not clear fails after four tries",
			replies:  []reply{failure(500), failure(500), failure(500), failure(500), repoPage(0, "never")},
			requests: 4,
			backoff:  []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			name:     "a 4xx is not retried",
			replies:  []reply{failure(http.StatusNotFound), repoPage(0, "never")},
			requests: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			api := newFakeAPI(t, clock, inOrder(tc.replies...))
			s := clockedSource(t, api, key, clock)

			repos, err := s.ListRepos(context.Background(), source.Filter{})
			if tc.want == "" {
				if _, ok := errors.AsType[*github.ErrorResponse](err); !ok {
					t.Errorf("ListRepos = %v, %v; want GitHub's own error", repos, err)
				}
			} else if err != nil {
				t.Fatalf("ListRepos: %v", err)
			} else if got := slugs(repos); got != tc.want {
				t.Errorf("repos = %s, want %s", got, tc.want)
			}
			if n := len(api.requests()); n != tc.requests {
				t.Errorf("%d requests, want %d: %v", n, tc.requests, api.requests())
			}
			slept := clock.sleeps()
			if len(slept) != len(tc.backoff) {
				t.Fatalf("waited %v, want %d backoffs", slept, len(tc.backoff))
			}
			for i, floor := range tc.backoff {
				if slept[i] < floor || slept[i] >= floor+time.Second {
					t.Errorf("backoff %d = %s, want at least %s and under a second more", i+1, slept[i], floor)
				}
			}
		})
	}
}

// A limit that lifts after the context's deadline is not waited for: the request fails now, and
// the error names the reset, so an operator knows when to run again. A wait that ends past the
// deadline would only delay the same failure, holding the run for up to an hour.
func TestARateLimitPastTheDeadlineFailsNamingTheReset(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		limit    func(now time.Time) reply
		reset    time.Duration // from now, as GitHub said it
		// Each case runs the listing or the metadata, which is how a repository fails.
		metadata bool
		wantErr  bool
	}{
		{"a primary limit, listing", 5 * time.Minute, func(now time.Time) reply { return primaryLimit(http.StatusForbidden, now.Add(30*time.Minute)) }, 30 * time.Minute, false, true},
		{"a secondary limit, listing", 5 * time.Minute, func(time.Time) reply { return secondaryLimit(http.StatusTooManyRequests, "3600") }, time.Hour, false, true},
		{"a primary limit, metadata", 5 * time.Minute, func(now time.Time) reply { return primaryLimit(http.StatusForbidden, now.Add(30*time.Minute)) }, 30 * time.Minute, true, true},
		{"a reset the deadline leaves room for is waited for", 2 * time.Hour, func(now time.Time) reply { return primaryLimit(http.StatusForbidden, now.Add(30*time.Minute)) }, 30 * time.Minute, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			limit := tc.limit(clock.now())
			reset := clock.now().Add(tc.reset).UTC().Format(time.RFC3339)
			// The request the case is about: the listing, or the issues of the repository.
			target := "/installation/repositories"
			if tc.metadata {
				target = "/issues"
			}
			api := newFakeAPI(t, clock, func(r *http.Request, n int) reply {
				if n == 0 && strings.HasSuffix(r.URL.Path, target) {
					return limit
				}
				if tc.metadata {
					return metadataReply(r)
				}
				return repoPage(0, "hello")
			})
			s := clockedSource(t, api, key, clock)
			ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
			defer cancel()

			var err error
			if tc.metadata {
				_, err = s.FetchMetadata(ctx, source.Repo{Host: "github.com", Owner: "octo", Name: "hello"})
			} else {
				_, err = s.ListRepos(ctx, source.Filter{})
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a wait that ends before the deadline failed: %v", err)
				}
				if slept := clock.sleeps(); len(slept) != 1 {
					t.Errorf("waited %v, want one wait", slept)
				}
				return
			}
			if err == nil {
				t.Fatal("a rate limit that lifts after the deadline did not fail the request")
			}
			for _, want := range []string{reset, "deadline"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if tc.metadata && !strings.Contains(err.Error(), "issues") {
				t.Errorf("the error does not say which metadata failed: %v", err)
			}
			if slept := clock.sleeps(); len(slept) != 0 {
				t.Errorf("waited %v for a limit that lifts after the deadline", slept)
			}
			if n := api.count(target); n != 1 {
				t.Errorf("%d requests to %s, want only the refused one: %v", n, target, api.requests())
			}
		})
	}
}

// Metadata is where a large organisation spends its limit: eight endpoints per repository, every
// page one request. Each request waits out a limit, and each retries a 5xx, the way the listing
// does.
func TestFetchMetadataWaitsOutARateLimit(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name string
		// answer replaces metadataReply for the requests the case is about, and returns ok false
		// for every other.
		answer func(now time.Time, r *http.Request, n int) (reply, bool)
		// The endpoint the case is about, how many requests reached it, and the wait before the
		// one that succeeded: at least this, and at most two seconds more.
		endpoint string
		requests int
		wait     time.Duration
	}{
		{
			name: "a limit met on one endpoint",
			answer: func(now time.Time, r *http.Request, n int) (reply, bool) {
				return primaryLimit(http.StatusForbidden, now.Add(10*time.Minute)), n == 0 && strings.HasSuffix(r.URL.Path, "/issues")
			},
			endpoint: "/issues", requests: 2, wait: 10 * time.Minute,
		},
		{
			// The request that spends the last of the limit succeeds and says so. go-github then
			// refuses the next one without sending it, until the reset; this is the refusal that
			// failed every repository after the one that spent the limit.
			name: "the request after the one that spent the limit",
			answer: func(now time.Time, r *http.Request, _ int) (reply, bool) {
				return reply{status: http.StatusOK, body: `[]`, header: map[string]string{
					"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10),
				}}, strings.HasSuffix(r.URL.Path, "/labels")
			},
			endpoint: "/milestones", requests: 1, wait: 10 * time.Minute,
		},
		{
			name: "a 5xx on one endpoint",
			answer: func(_ time.Time, r *http.Request, n int) (reply, bool) {
				return failure(http.StatusBadGateway), n == 0 && strings.HasSuffix(r.URL.Path, "/pulls")
			},
			endpoint: "/pulls", requests: 2, wait: time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			start := clock.now()
			api := newFakeAPI(t, clock, func(r *http.Request, n int) reply {
				if a, ok := tc.answer(start, r, n); ok {
					return a
				}
				return metadataReply(r)
			})
			s := clockedSource(t, api, key, clock)

			b, err := s.FetchMetadata(context.Background(), source.Repo{Host: "github.com", Owner: "octo", Name: "hello"})
			if err != nil {
				t.Fatalf("FetchMetadata: %v", err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("invalid metadata json: %v", err)
			}
			for _, k := range []string{"repo", "labels", "milestones", "issues", "comments", "pullRequests", "reviewComments", "releases"} {
				if _, ok := doc[k]; !ok {
					t.Errorf("metadata missing %q", k)
				}
			}
			if n := api.count(tc.endpoint); n != tc.requests {
				t.Errorf("%d requests to %s, want %d: %v", n, tc.endpoint, tc.requests, api.requests())
			}
			slept := clock.sleeps()
			if len(slept) != 1 {
				t.Fatalf("waited %v, want one wait", slept)
			}
			if slept[0] < tc.wait || slept[0] > tc.wait+2*time.Second {
				t.Errorf("waited %s, want %s and at most two seconds more", slept[0], tc.wait)
			}
		})
	}
}

// A limit that never lifts is not waited for indefinitely. Five waits, then the request fails
// with GitHub's error; written out, so a change to the bound is a change to this test.
func TestARateLimitThatNeverLiftsFailsAfterFiveWaits(t *testing.T) {
	clock := newFakeClock()
	api := newFakeAPI(t, clock, func(*http.Request, int) reply {
		return primaryLimit(http.StatusForbidden, clock.now().Add(time.Minute))
	})
	s := clockedSource(t, api, testKeyPEM(t), clock)

	_, err := s.ListRepos(context.Background(), source.Filter{})
	if _, ok := errors.AsType[*github.RateLimitError](err); !ok {
		t.Fatalf("ListRepos = %v, want GitHub's rate-limit error", err)
	}
	if n := len(clock.sleeps()); n != 5 {
		t.Errorf("waited %d times, want 5", n)
	}
	if n := len(api.requests()); n != 6 {
		t.Errorf("%d requests, want the first and five retries", n)
	}
}

// In App mode the installation token is minted inside the round trip of the first API request
// that needs it, so a 5xx from the token endpoint arrives as the transport's error, wrapped in the
// request's, and not as go-github's. It was never retried: one bad answer there failed the
// listing, and with it the run.
func TestAServerErrorMintingTheTokenIsRetried(t *testing.T) {
	key := testKeyPEM(t)
	for _, tc := range []struct {
		name     string
		failures int // mints answered 502 before one succeeds
		wantErr  bool
		mints    int // requests for a token, all told
		backoffs int
	}{
		{name: "a 502, then a token", failures: 1, mints: 2, backoffs: 1},
		// Four tries, as for any 5xx, then the listing fails with the transport's error.
		{name: "a 502 that does not clear", failures: 10, wantErr: true, mints: 4, backoffs: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			api := newFakeAPI(t, clock, inOrder(repoPage(0, "hello")))
			api.failMints(tc.failures, http.StatusBadGateway)
			s := clockedSource(t, api, key, clock)

			repos, err := s.ListRepos(context.Background(), source.Filter{})
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ListRepos = %v, want the token endpoint's error", repos)
			case !tc.wantErr && err != nil:
				t.Fatalf("ListRepos: %v", err)
			case !tc.wantErr && slugs(repos) != "octo/hello":
				t.Errorf("repos = %s, want octo/hello", slugs(repos))
			}
			if n := api.minted(); n != tc.mints {
				t.Errorf("%d requests for a token, want %d", n, tc.mints)
			}
			if n := len(clock.sleeps()); n != tc.backoffs {
				t.Errorf("%d backoffs, want %d", n, tc.backoffs)
			}
		})
	}
}

// A host whose clock is off from GitHub's waits for the reset GitHub meant.
//
// GitHub names the reset on its own clock. Measured on the host's, a host a few seconds fast sent
// its retry before the reset, every time, and after that the reset was in its past, so each later
// wait was a second or two: five waits covered about seven seconds of skew, and a host nine seconds
// fast failed every request that met the limit. Measured from the Date of the refusal, one wait
// is enough whatever the skew.
func TestARateLimitResetIsMeasuredOnGitHubsClock(t *testing.T) {
	key := testKeyPEM(t)
	for _, skew := range []time.Duration{3 * time.Second, 9 * time.Second, 15 * time.Second, -9 * time.Second} {
		t.Run(fmt.Sprintf("host clock %s off", skew), func(t *testing.T) {
			clock := newFakeClock()
			github := func() time.Time { return clock.now().Add(-skew) }
			reset := github().Add(10 * time.Minute)
			api := newFakeAPI(t, clock, func(*http.Request, int) reply {
				if now := github(); now.Before(reset) {
					r := primaryLimit(http.StatusForbidden, reset)
					r.header["Date"] = now.UTC().Format(http.TimeFormat)
					return r
				}
				return repoPage(0, "hello")
			})
			s := clockedSource(t, api, key, clock)

			if _, err := s.ListRepos(context.Background(), source.Filter{}); err != nil {
				t.Fatalf("ListRepos: %v", err)
			}
			slept := clock.sleeps()
			if len(slept) != 1 {
				t.Fatalf("waited %v, want one wait, measured on GitHub's clock", slept)
			}
			if slept[0] < 10*time.Minute || slept[0] > 10*time.Minute+2*time.Second {
				t.Errorf("waited %s, want the ten minutes GitHub named and at most two seconds more", slept[0])
			}
		})
	}
}

// The waits SPEC.md promises for a limit that names no time: a minute, doubled for each wait
// already spent on the request, five of them, and then the request fails.
func TestALimitThatNamesNoTimeWaitsLongerEachTime(t *testing.T) {
	clock := newFakeClock()
	api := newFakeAPI(t, clock, func(*http.Request, int) reply { return secondaryLimit(http.StatusForbidden, "") })
	s := clockedSource(t, api, testKeyPEM(t), clock)

	if _, err := s.ListRepos(context.Background(), source.Filter{}); err == nil {
		t.Fatal("a limit that never lifted did not fail the request")
	}
	slept := clock.sleeps()
	if len(slept) != 5 {
		t.Fatalf("waited %v, want five waits", slept)
	}
	for i, minutes := range []time.Duration{1, 2, 4, 8, 16} {
		if want := minutes * time.Minute; slept[i] < want || slept[i] > want+2*time.Second {
			t.Errorf("wait %d = %s, want %s and at most two seconds more", i+1, slept[i], want)
		}
	}
}

// The real wait ends when its context does. A run stopped during an hour's wait for a rate limit
// stops now, not when the hour is up.
func TestTheWaitEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext = %v, want the context's error", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("a cancelled wait took %s", waited)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("a wait that ran its course = %v, want nil", err)
	}
}
