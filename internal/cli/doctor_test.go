package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// `gitdr doctor`, end to end: the command line, the config, the real S3 backend and SDK against a
// fake S3 endpoint, and what lands on stdout and on stderr.
//
// The document is decoded into types written out here rather than borrowed from the code under
// test, so a field the code stops printing fails here instead of disappearing from both sides.

const (
	lockedCompliance = `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
		`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule>` +
		`</ObjectLockConfiguration>`
	objectKey     = "github.com/acme/api/2026-10-03/api.bundle"
	oneObjectPage = `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Name>b</Name><Prefix></Prefix><KeyCount>1</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated>` +
		`<Contents><Key>` + objectKey + `</Key><Size>12</Size></Contents></ListBucketResult>`
	// Far ahead: doctor reads it against the clock, and a date that has passed is a lock that ended.
	heldRetention = `<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Mode>COMPLIANCE</Mode><RetainUntilDate>2099-11-02T12:00:00Z</RetainUntilDate></Retention>`
	noLockConfiguration = `<Error><Code>ObjectLockConfigurationNotFoundError</Code>` +
		`<Message>Object Lock configuration does not exist for this bucket</Message></Error>`
)

// doctorStore is a fake S3 endpoint. It answers the three reads doctor makes, from handlers a
// test may replace before running doctor, counts them, and fails the test on any other request:
// a write above all, whatever doctor was asked to do.
type doctorStore struct {
	*httptest.Server
	t                     *testing.T
	lock, list, retention http.HandlerFunc

	mu          sync.Mutex
	calls       map[string]int
	listQueries []string
}

func newDoctorStore(t *testing.T) *doctorStore {
	t.Helper()
	return newDoctorStoreOn(t, nil)
}

// newDoctorStoreOn starts the fake on ln, or on httptest's own listener when ln is nil. It
// answers as a locked bucket holding one object under retention until a test says otherwise.
func newDoctorStoreOn(t *testing.T, ln net.Listener) *doctorStore {
	t.Helper()
	s := &doctorStore{
		t:         t,
		lock:      answer(http.StatusOK, lockedCompliance),
		list:      answer(http.StatusOK, oneObjectPage),
		retention: answer(http.StatusOK, heldRetention),
		calls:     map[string]int{},
	}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	if ln != nil {
		_ = s.Listener.Close()
		s.Listener = ln
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func (s *doctorStore) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	op := "other"
	switch {
	case r.Method != http.MethodGet:
		op = "write"
	case q.Has("object-lock"):
		op = "lock"
	case q.Get("list-type") == "2":
		op = "list"
	case q.Has("retention"):
		op = "retention"
	}
	s.mu.Lock()
	s.calls[op]++
	if op == "list" {
		s.listQueries = append(s.listQueries, r.URL.RawQuery)
	}
	s.mu.Unlock()

	switch op {
	case "lock":
		s.lock(w, r)
	case "list":
		s.list(w, r)
	case "retention":
		s.retention(w, r)
	default:
		// Errorf rather than Fatal: this runs on the server's goroutine, not the test's.
		s.t.Errorf("doctor sent %s %s; it reads the lock, one page of the listing and a retention, and writes nothing",
			r.Method, r.URL)
		w.WriteHeader(http.StatusForbidden)
	}
}

func (s *doctorStore) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// answer is a handler that always says the same thing.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// doctorEnv gives doctor a static S3 key and nothing else from the environment: no GITDR_ setting,
// no AWS profile, endpoint or CA bundle, and no instance metadata service to wait for.
func doctorEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GITDR_") || strings.HasPrefix(k, "AWS_") {
			t.Setenv(k, "") // restored when the test ends
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDOCTORTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "doctor-test-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent"))
}

// bucketConfig writes a config with a destination and no source, which is what someone has
// before connecting anything to back up into the bucket. extra is appended as YAML.
func bucketConfig(t *testing.T, endpoint, extra string) string {
	t.Helper()
	doc := "destination:\n  type: s3\n  s3:\n    bucket: b\n    region: us-east-1\n" +
		"    endpoint: " + endpoint + "\n    usePathStyle: true\n" + extra
	path := filepath.Join(t.TempDir(), "gitdr.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runDoctorCLI runs `gitdr doctor` with args, and returns its exit code, stdout and stderr.
func runDoctorCLI(ctx context.Context, t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = Run(ctx, append([]string{"doctor"}, args...)) })
	})
	return code, stdout, stderr
}

// doctorScopes are the two ways doctor runs. Everything about the destination holds in both: with
// every check, the source check fails for want of a credential and the destination is checked
// all the same, as it always was.
var doctorScopes = []struct {
	name string
	args []string
}{
	{"-only destination", []string{"-only", "destination"}},
	{"every check", nil},
}

func destOnly(args []string) bool { return slices.Contains(args, "-only") }

// doctorReport is gitdr.doctor/v1 as a reader decodes it.
type doctorReport struct {
	Schema string        `json:"schema"`
	OK     bool          `json:"ok"`
	Checks []doctorEntry `json:"checks"`
}

type doctorEntry struct {
	Name     string  `json:"name"`
	OK       bool    `json:"ok"`
	Detail   string  `json:"detail"`
	Verdict  *string `json:"verdict"`
	Mode     *string `json:"mode"`
	Code     *string `json:"code"`
	Observed *string `json:"observed"`

	keys []string // every key the check carries, null ones included
}

func decodeDoctor(t *testing.T, out string) doctorReport {
	t.Helper()
	var rep doctorReport
	var raw struct {
		Checks []map[string]json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor's output is not JSON: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for i, c := range raw.Checks {
		for k := range c {
			rep.Checks[i].keys = append(rep.Checks[i].keys, k)
		}
		slices.Sort(rep.Checks[i].keys)
	}
	return rep
}

func (r doctorReport) names() []string {
	var names []string
	for _, c := range r.Checks {
		names = append(names, c.Name)
	}
	return names
}

func (r doctorReport) check(t *testing.T, name string) doctorEntry {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("doctor reported no %q check; it reported %v", name, r.names())
	return doctorEntry{}
}

// text is a nullable field as a test reads it: the value, or "null".
func text(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// A config with no source, run with -only destination, exits 0 with a v1 document. Nothing in
// that mode needs a source, git or git-lfs, so PATH is empty here and the config names no source.
func TestDoctorChecksABucketWithNoSourceConfigured(t *testing.T) {
	doctorEnv(t)
	t.Setenv("PATH", t.TempDir())
	store := newDoctorStore(t)
	cfg := bucketConfig(t, store.URL, "")
	ctx := context.Background()

	code, out, stderr := runDoctorCLI(ctx, t, "-config", cfg, "-only", "destination", "-output", "json")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	rep := decodeDoctor(t, out)
	if rep.Schema != "gitdr.doctor/v1" {
		t.Errorf("schema = %q, want gitdr.doctor/v1", rep.Schema)
	}
	if !rep.OK {
		t.Errorf("ok = false for a locked bucket holding a retained object:\n%s", out)
	}
	if got, want := rep.names(), []string{"config", "worm", "retention"}; !slices.Equal(got, want) {
		t.Errorf("checks = %v, want %v: the destination's and nothing about a source or tooling", got, want)
	}
	worm := rep.check(t, "worm")
	if text(worm.Verdict) != "immutable" || text(worm.Mode) != "COMPLIANCE" || worm.Code != nil {
		t.Errorf("worm = verdict %s, mode %s, code %s; want immutable, COMPLIANCE, null",
			text(worm.Verdict), text(worm.Mode), text(worm.Code))
	}
	if ret := rep.check(t, "retention"); text(ret.Observed) != "present" || !ret.OK {
		t.Errorf("retention = %+v, want observed present", ret)
	}

	code, out, _ = runDoctorCLI(ctx, t, "-config", cfg, "-only", "destination")
	if code != 0 {
		t.Errorf("text output: exit %d, want 0\n%s", code, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "[ok] config,") && !strings.HasPrefix(line, "[ok] worm,") && !strings.HasPrefix(line, "[ok] retention,") {
			t.Errorf("text output has %q, which is not one of the destination's checks", line)
		}
	}
}

// The worm check states its verdict as data, beside an `ok` that keeps its meaning: a bucket
// that locks nothing is ok unless worm.require is set. Until v1 only the prose said which.
func TestDoctorStatesTheVerdictBesideOK(t *testing.T) {
	refused := answer(http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	for _, tc := range []struct {
		name                string
		lock                http.HandlerFunc
		require             bool
		verdict, mode, code string // "null" for JSON null
		ok                  bool
	}{
		{"no object lock configuration", answer(http.StatusNotFound, noLockConfiguration), false, "not-immutable", "null", "null", true},
		{"no object lock configuration, worm.require", answer(http.StatusNotFound, noLockConfiguration), true, "not-immutable", "null", "null", false},
		{"object lock not enabled", answer(http.StatusOK, `<ObjectLockConfiguration></ObjectLockConfiguration>`), false, "not-immutable", "null", "null", true},
		{"locked, governance", answer(http.StatusOK, strings.Replace(lockedCompliance, "COMPLIANCE", "GOVERNANCE", 1)), false, "immutable", "GOVERNANCE", "null", true},
		{"locked, a mode gitdr does not know", answer(http.StatusOK, strings.Replace(lockedCompliance, "COMPLIANCE", "Forever", 1)), false, "immutable", "null", "null", true},
		{"refused", refused, false, "unknown", "null", "AccessDenied", true},
		{"refused, worm.require", refused, true, "unknown", "null", "AccessDenied", false},
		{"refused with a code that is not shaped like one", answer(http.StatusForbidden, `<Error><Code>Access Denied!</Code></Error>`), false, "unknown", "null", "unnamed", true},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				store := newDoctorStore(t)
				store.lock = tc.lock
				extra := ""
				if tc.require {
					extra = "worm:\n  require: true\n"
				}
				args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, extra), "-output", "json")
				code, out, stderr := runDoctorCLI(context.Background(), t, args...)

				rep := decodeDoctor(t, out)
				worm := rep.check(t, "worm")
				if text(worm.Verdict) != tc.verdict || text(worm.Mode) != tc.mode || text(worm.Code) != tc.code {
					t.Errorf("worm = verdict %s, mode %s, code %s; want %s, %s, %s\n%s",
						text(worm.Verdict), text(worm.Mode), text(worm.Code), tc.verdict, tc.mode, tc.code, out)
				}
				if worm.OK != tc.ok {
					t.Errorf("worm ok = %v, want %v: ok keeps the meaning it had before the verdict was there", worm.OK, tc.ok)
				}
				if destOnly(scope.args) {
					wantCode := 0
					if !tc.ok {
						wantCode = 1
					}
					if code != wantCode || rep.OK != tc.ok {
						t.Errorf("exit %d and ok %v, want %d and %v\nstderr:\n%s", code, rep.OK, wantCode, tc.ok, stderr)
					}
				}
			})
		}
	}
}

// The object doctor reads a retention from comes from one page of the listing. A store that
// answers every page with another one used to keep doctor listing until something killed it.
func TestDoctorReadsOnePageOfTheListing(t *testing.T) {
	for _, scope := range doctorScopes {
		t.Run(scope.name, func(t *testing.T) {
			doctorEnv(t)
			store := newDoctorStore(t)
			var pages atomic.Int64
			store.list = func(w http.ResponseWriter, _ *http.Request) {
				n := pages.Add(1)
				// An object on every page, and every page says there is another one, under a token
				// never seen before, so nothing that compares tokens stops it either.
				_, _ = fmt.Fprintf(w, `<ListBucketResult><Name>b</Name><KeyCount>1</KeyCount><IsTruncated>true</IsTruncated>`+
					`<NextContinuationToken>page-%d</NextContinuationToken>`+
					`<Contents><Key>github.com/acme/api/%d.bundle</Key><Size>1</Size></Contents></ListBucketResult>`, n, n)
			}
			// The bound on a doctor that walks the listing. One that reads one page is done long
			// before it.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json")
			_, out, _ := runDoctorCLI(ctx, t, args...)

			if n := store.count("list"); n != 1 {
				t.Errorf("doctor asked for %d pages of a listing that never ends; it reads one", n)
			}
			store.mu.Lock()
			first := ""
			if len(store.listQueries) > 0 {
				first = store.listQueries[0]
			}
			store.mu.Unlock()
			if !strings.Contains(first, "max-keys=1") {
				t.Errorf("the first listing request asked %q; doctor needs one object and asks for one", first)
			}
			if ret := decodeDoctor(t, out).check(t, "retention"); text(ret.Observed) != "present" {
				t.Errorf("retention = %+v, want observed present from the object on the first page", ret)
			}
		})
	}
}

// What the store wrote never reaches stdout, except an error code of the right shape. The marker
// is in every place a store's text used to come through: the name of an element in a page that
// is not XML, an S3 error's message, its request and host ids, the x-amz-request-id header, and
// the error code itself.
func TestDoctorKeepsTheStoresWordsOffStdout(t *testing.T) {
	const marker = "gitdr-marker-5f3b9" // hyphens, so not shaped like a code
	html := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Sign in</title></head>`+
				`<body><`+marker+`>Sign in to continue</body></html>`)
		}
	}
	s3Error := func(status int, code string) http.HandlerFunc {
		return answer(status, `<Error><Code>`+code+`</Code><Message>`+marker+`</Message>`+
			`<RequestId>`+marker+`</RequestId><HostId>`+marker+`</HostId></Error>`)
	}
	for _, tc := range []struct {
		name  string
		setup func(*doctorStore)
		check func(t *testing.T, rep doctorReport)
		// page is an answer that is not the store's API. It is refused at its first element, so
		// the log says it was refused rather than carrying anything written inside it.
		page bool
	}{
		{
			"an HTML page for the lock configuration",
			func(s *doctorStore) { s.lock = html(http.StatusOK) },
			func(t *testing.T, rep doctorReport) {
				if w := rep.check(t, "worm"); w.Verdict != nil || text(w.Code) != "not-s3" {
					t.Errorf("worm = verdict %s, code %s; want null, not-s3", text(w.Verdict), text(w.Code))
				}
			},
			true,
		},
		{
			"an HTML error page for a retention",
			func(s *doctorStore) { s.retention = html(http.StatusForbidden) },
			func(t *testing.T, rep doctorReport) {
				if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "(not-s3)") {
					t.Errorf("retention = %+v, want unreadable, not-s3", r)
				}
			},
			true,
		},
		{
			"an S3 error with the marker in its message and ids, for a retention",
			func(s *doctorStore) { s.retention = s3Error(http.StatusForbidden, "AccessDenied") },
			func(t *testing.T, rep doctorReport) {
				if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "AccessDenied") {
					t.Errorf("retention = %+v, want unreadable, naming AccessDenied", r)
				}
			},
			false,
		},
		{
			"an S3 error with the marker in its message, for the listing",
			func(s *doctorStore) { s.list = s3Error(http.StatusForbidden, "AccessDenied") },
			func(t *testing.T, rep doctorReport) {
				if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "AccessDenied") {
					t.Errorf("retention = %+v, want unreadable, naming AccessDenied", r)
				}
			},
			false,
		},
		{
			"the marker as the error code, for the lock configuration",
			func(s *doctorStore) { s.lock = s3Error(http.StatusForbidden, marker) },
			func(t *testing.T, rep doctorReport) {
				if w := rep.check(t, "worm"); text(w.Verdict) != "unknown" || text(w.Code) != "unnamed" {
					t.Errorf("worm = verdict %s, code %s; want unknown, unnamed", text(w.Verdict), text(w.Code))
				}
			},
			false,
		},
		{
			"the marker in the request id headers, for a retention",
			func(s *doctorStore) {
				s.retention = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("x-amz-request-id", marker)
					w.Header().Set("x-amz-id-2", marker)
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `<html><body><h1>Forbidden</h1></body></html>`)
				}
			},
			func(t *testing.T, rep doctorReport) {
				if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" {
					t.Errorf("retention = %+v, want unreadable", r)
				}
			},
			// A page too, but the request ids come from headers, which the SDK puts in its error.
			false,
		},
	} {
		for _, scope := range doctorScopes {
			for _, output := range []string{"json", "text"} {
				t.Run(tc.name+", "+scope.name+", "+output, func(t *testing.T) {
					doctorEnv(t)
					store := newDoctorStore(t)
					tc.setup(store)
					args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", output)
					_, out, stderr := runDoctorCLI(context.Background(), t, args...)

					if strings.Contains(out, marker) {
						t.Errorf("the store's words reached stdout:\n%s", out)
					}
					// Not lost, either: the whole error goes to the log, on stderr.
					logged := marker
					if tc.page {
						logged = "did not answer as the storage API does"
					}
					if !strings.Contains(stderr, logged) {
						t.Errorf("the error on stderr does not carry %q:\n%s", logged, stderr)
					}
					if output == "json" {
						tc.check(t, decodeDoctor(t, out))
					}
				})
			}
		}
	}
}

// tightListener gives every connection a small send buffer, so what the fake counts as written is
// close to what doctor read, rather than a socket buffer's worth ahead of it.
type tightListener struct{ net.Listener }

func (l tightListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(16 << 10)
	}
	return c, err
}

// A 64 MiB answer stops at the 1 MiB cap. Each of doctor's three reads gets one in turn, padding
// inside a document that would otherwise parse, so that read in full it is a good answer.
//
// The fake counts what it got onto the wire before doctor hung up. That is the cap plus whatever
// the two sockets buffer, which the kernel sizes: about 1 MiB and 16 KiB on a laptop, and the
// receive side can grow to several MiB elsewhere. So the bound is half the answer, loose on
// purpose. What it rules out is the old behaviour, where the SDK read every byte of an answer,
// an error's included. The exact cap is TestLimitBodyStopsALongAnswerAtTheCap's.
func TestDoctorStopsReadingAnAnswerAtOneMiB(t *testing.T) {
	const size = 64 << 20
	long := func(written *atomic.Int64, done chan<- struct{}, status int, open, close string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			defer func() {
				select {
				case done <- struct{}{}:
				default: // a second request finds the signal already sent, and must not wait on it
				}
			}()
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.WriteHeader(status)
			chunk := []byte(strings.Repeat("x", 32<<10))
			pad := size - len(open) - len(close)
			n, err := io.WriteString(w, open)
			written.Add(int64(n))
			for pad > 0 && err == nil {
				part := chunk[:min(len(chunk), pad)]
				n, err = w.Write(part)
				written.Add(int64(n))
				pad -= len(part)
			}
			if err == nil {
				n, _ = io.WriteString(w, close)
				written.Add(int64(n))
			}
		}
	}
	for _, tc := range []struct {
		name, at    string
		status      int
		open, close string
		check       func(t *testing.T, rep doctorReport)
	}{
		{
			"the lock configuration", "lock", http.StatusOK,
			`<ObjectLockConfiguration><Padding>`, `</Padding><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`,
			wormSaysTooLarge,
		},
		{
			"an error for the lock configuration", "lock", http.StatusForbidden,
			`<Error><Code>AccessDenied</Code><Message>`, `</Message></Error>`,
			wormSaysTooLarge,
		},
		{
			"the listing", "list", http.StatusOK,
			`<ListBucketResult><Name>b</Name><Padding>`, `</Padding><Contents><Key>k</Key></Contents></ListBucketResult>`,
			retentionSaysTooLarge,
		},
		{
			"a retention", "retention", http.StatusOK,
			`<Retention><Padding>`, `</Padding><Mode>COMPLIANCE</Mode><RetainUntilDate>2026-11-02T12:00:00Z</RetainUntilDate></Retention>`,
			retentionSaysTooLarge,
		},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				store := newDoctorStoreOn(t, tightListener{ln})

				var written atomic.Int64
				done := make(chan struct{}, 1)
				h := long(&written, done, tc.status, tc.open, tc.close)
				switch tc.at {
				case "lock":
					store.lock = h
				case "list":
					store.list = h
				case "retention":
					store.retention = h
				}

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json")
				code, out, _ := runDoctorCLI(ctx, t, args...)
				if store.count(tc.at) == 0 {
					t.Fatalf("doctor never made the read that answers long (exit %d)\n%s", code, out)
				}
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("the fake was still writing 30 seconds after doctor returned")
				}
				got := written.Load()
				t.Logf("the fake got %d bytes onto the wire", got)
				if got >= size/2 {
					t.Errorf("the fake got %d bytes of a %d-byte answer onto the wire; doctor reads 1 MiB of one", got, size)
				}
				tc.check(t, decodeDoctor(t, out))
			})
		}
	}
}

func wormSaysTooLarge(t *testing.T, rep doctorReport) {
	t.Helper()
	if w := rep.check(t, "worm"); w.Verdict != nil || text(w.Code) != "too-large" {
		t.Errorf("worm = verdict %s, code %s; want null, too-large", text(w.Verdict), text(w.Code))
	}
}

func retentionSaysTooLarge(t *testing.T, rep doctorReport) {
	t.Helper()
	if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "(too-large)") {
		t.Errorf("retention = %+v, want unreadable, too-large", r)
	}
}

// Nothing reaches the bucket but reads: no probe object, no canary, whichever way the bucket
// answers. The fake fails the test on any other request.
func TestDoctorSendsTheBucketNothingButReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*doctorStore)
	}{
		{"locked, with an object", func(*doctorStore) {}},
		{"locked and empty", func(s *doctorStore) {
			s.list = answer(http.StatusOK, `<ListBucketResult><Name>b</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
		}},
		{"not locked", func(s *doctorStore) { s.lock = answer(http.StatusNotFound, noLockConfiguration) }},
		{"refusing", func(s *doctorStore) {
			s.lock = answer(http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`)
		}},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				store := newDoctorStore(t)
				tc.setup(store)
				args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json")
				code, out, _ := runDoctorCLI(context.Background(), t, args...)
				if n := store.count("write") + store.count("other"); n != 0 {
					t.Errorf("%d requests that were not one of doctor's reads", n)
				}
				if destOnly(scope.args) && code != 0 {
					t.Errorf("exit %d, want 0\n%s", code, out)
				}
			})
		}
	}
}

// gitdr.doctor/v1, pinned. A reader finds schema, ok and checks at the top; name, ok and detail
// on every check; verdict, mode and code on the worm check, null or not; and observed on the
// retention check. A key added is a dated SPEC note; one renamed or taken away is a break.
func TestDoctorDocumentIsV1(t *testing.T) {
	for _, scope := range doctorScopes {
		t.Run(scope.name, func(t *testing.T) {
			doctorEnv(t)
			store := newDoctorStore(t)
			args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json")
			_, out, _ := runDoctorCLI(context.Background(), t, args...)

			var top map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &top); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			var keys []string
			for k := range top {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if want := []string{"checks", "ok", "schema"}; !slices.Equal(keys, want) {
				t.Errorf("top-level keys = %v, want %v", keys, want)
			}
			rep := decodeDoctor(t, out)
			if rep.Schema != "gitdr.doctor/v1" {
				t.Errorf("schema = %q", rep.Schema)
			}
			for _, c := range rep.Checks {
				want := []string{"detail", "name", "ok"}
				switch c.Name {
				case "worm":
					want = []string{"code", "detail", "mode", "name", "ok", "verdict"}
				case "retention":
					want = []string{"detail", "name", "observed", "ok"}
				}
				if !slices.Equal(c.keys, want) {
					t.Errorf("the %s check has keys %v, want %v", c.Name, c.keys, want)
				}
			}
		})
	}
}

// -only takes one value. Anything else, an empty one included, is a command line that asks for
// a group of checks that does not exist: exit 2 and nothing on stdout, before anything runs.
func TestDoctorOnlyTakesDestination(t *testing.T) {
	doctorEnv(t)
	store := newDoctorStore(t)
	cfg := bucketConfig(t, store.URL, "")
	for _, args := range [][]string{{"-only", "source"}, {"-only", ""}, {"-only=Destination"}} {
		code, out, stderr := runDoctorCLI(context.Background(), t, append(args, "-config", cfg, "-output", "json")...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if out != "" {
			t.Errorf("%v: a refused command line printed:\n%s", args, out)
		}
		if !strings.Contains(stderr, "-only") {
			t.Errorf("%v: stderr does not say what was wrong:\n%s", args, stderr)
		}
	}
	if n := store.count("lock") + store.count("list") + store.count("retention"); n != 0 {
		t.Errorf("%d requests reached the bucket from command lines that were refused", n)
	}
}

// -only destination checks the destination block and only that: a source block every other
// command refuses is no business of a bucket check, and a broken destination block is still
// refused, in a v1 document that says why.
func TestDoctorOnlyDestinationChecksTheDestinationBlock(t *testing.T) {
	doctorEnv(t)
	store := newDoctorStore(t)
	ctx := context.Background()

	cfg := bucketConfig(t, store.URL, "source:\n  type: bitbucket\n")
	code, out, stderr := runDoctorCLI(ctx, t, "-config", cfg, "-only", "destination", "-output", "json")
	if code != 0 {
		t.Errorf("an unsupported source failed a bucket check: exit %d\n%s\n%s", code, out, stderr)
	}

	noBucket := filepath.Join(t.TempDir(), "gitdr.yaml")
	if err := os.WriteFile(noBucket, []byte("destination:\n  type: s3\n  s3:\n    endpoint: "+store.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := store.count("lock")
	code, out, _ = runDoctorCLI(ctx, t, "-config", noBucket, "-only", "destination", "-output", "json")
	rep := decodeDoctor(t, out)
	if code != 1 || rep.OK || rep.Schema != "gitdr.doctor/v1" {
		t.Errorf("exit %d, ok %v, schema %q; want 1, false, gitdr.doctor/v1", code, rep.OK, rep.Schema)
	}
	if c := rep.check(t, "config"); c.OK || !strings.Contains(c.Detail, "destination.s3.bucket is required") {
		t.Errorf("config = %+v, want it failed naming destination.s3.bucket", c)
	}
	if store.count("lock") != before {
		t.Error("doctor asked a bucket it had no name for")
	}
}

// What is on an object, end to end over S3: the four answers the retention check can give.
func TestDoctorSaysWhatRetentionIsOnAnObject(t *testing.T) {
	emptyPage := func(more bool) http.HandlerFunc {
		return answer(http.StatusOK, fmt.Sprintf(`<ListBucketResult><Name>b</Name><KeyCount>0</KeyCount>`+
			`<IsTruncated>%v</IsTruncated><NextContinuationToken>t</NextContinuationToken></ListBucketResult>`, more))
	}
	none := answer(http.StatusNotFound, `<Error><Code>NoSuchObjectLockConfiguration</Code>`+
		`<Message>The specified object does not have a ObjectLock configuration</Message></Error>`)
	for _, tc := range []struct {
		name      string
		list      http.HandlerFunc
		retention http.HandlerFunc
		require   bool
		observed  string
		ok        bool
		detail    string
	}{
		{"an empty bucket", emptyPage(false), nil, false, "none", true, "nothing written here yet"},
		{"an empty first page with more after it", emptyPage(true), nil, false, "none", true, "first page of the listing named no object"},
		{"an object holding a retention", nil, nil, false, "present", true, "held until 2099-11-02T12:00:00Z"},
		{"an object holding none", nil, none, false, "absent", true, "carries no retention"},
		{"an object holding none, worm.require", nil, none, true, "absent", false, "carries no retention"},
		{"a retention the key may not read", nil, answer(http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`), false,
			"unreadable", true, "the store answered AccessDenied; on S3 this needs s3:GetObjectRetention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorEnv(t)
			store := newDoctorStore(t)
			if tc.list != nil {
				store.list = tc.list
			}
			if tc.retention != nil {
				store.retention = tc.retention
			}
			extra := ""
			if tc.require {
				extra = "worm:\n  require: true\n"
			}
			_, out, _ := runDoctorCLI(context.Background(), t,
				"-config", bucketConfig(t, store.URL, extra), "-only", "destination", "-output", "json")
			r := decodeDoctor(t, out).check(t, "retention")
			if text(r.Observed) != tc.observed || r.OK != tc.ok || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("retention = observed %s, ok %v, %q; want %s, %v, saying %q",
					text(r.Observed), r.OK, r.Detail, tc.observed, tc.ok, tc.detail)
			}
		})
	}
}

// A bucket that does not exist is never reported as one that locks nothing. MinIO answers the lock
// question about a missing bucket with ObjectLockConfigurationNotFoundError, the answer an
// unlocked bucket gets, and doctor called a mistyped bucket name "NOT immutable". AWS answers the
// lock question itself with NoSuchBucket.
//
// Nor is it a pass. A bucket that is not there is not a WORM question: a backup has nothing to
// write into and stops before it copies anything, so the check fails, worm.require or not, and
// doctor exits 1. A bucket that is there and locks nothing still passes unless worm.require is set.
func TestDoctorNeverCallsAMissingBucketUnlocked(t *testing.T) {
	noLock := answer(http.StatusNotFound, noLockConfiguration)
	noBucket := answer(http.StatusNotFound,
		`<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`)
	for _, tc := range []struct {
		name          string
		lock, list    http.HandlerFunc
		require       bool
		verdict, code string
		ok            bool
	}{
		{"a bucket that is not there, as MinIO says it", noLock, noBucket, false, "unknown", "NoSuchBucket", false},
		{"a bucket that is not there, as AWS says it", noBucket, nil, false, "unknown", "NoSuchBucket", false},
		{"a bucket that is not there, worm.require", noLock, noBucket, true, "unknown", "NoSuchBucket", false},
		{"a bucket that is there", noLock, nil, false, "not-immutable", "null", true},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				store := newDoctorStore(t)
				store.lock = tc.lock
				if tc.list != nil {
					store.list = tc.list
				}
				extra := ""
				if tc.require {
					extra = "worm:\n  require: true\n"
				}
				cfg := bucketConfig(t, store.URL, extra)
				code, out, _ := runDoctorCLI(context.Background(), t, append(slices.Clone(scope.args), "-config", cfg, "-output", "json")...)
				rep := decodeDoctor(t, out)
				w := rep.check(t, "worm")
				if text(w.Verdict) != tc.verdict || text(w.Code) != tc.code || w.OK != tc.ok {
					t.Errorf("worm = verdict %s, code %s, ok %v; want %s, %s, %v\n%s",
						text(w.Verdict), text(w.Code), w.OK, tc.verdict, tc.code, tc.ok, out)
				}
				if !tc.ok && !strings.Contains(w.Detail, "the bucket does not exist") {
					t.Errorf("worm detail %q does not say the bucket does not exist", w.Detail)
				}
				if !destOnly(scope.args) {
					return // the source check fails here for want of a credential, whatever the bucket
				}
				wantCode := 0
				if !tc.ok {
					wantCode = 1
				}
				if code != wantCode || rep.OK != tc.ok {
					t.Errorf("exit %d and ok %v, want %d and %v", code, rep.OK, wantCode, tc.ok)
				}
				code, printed, _ := runDoctorCLI(context.Background(), t, "-only", "destination", "-config", cfg)
				if !tc.ok && (code != 1 || !strings.Contains(printed, "[FAIL] worm, the bucket does not exist")) {
					t.Errorf("text output: exit %d, want 1 and a failed worm check\n%s", code, printed)
				}
			})
		}
	}
}

// An endpoint that answers with a web page has not answered the question, so it gets no verdict
// and the code not-s3. Read as XML, a well-formed page was a lock configuration with nothing in it
// ("NOT immutable"), a 404 page a store's NotFound ("could not read"), and a 200 page in place of
// a retention an object holding none.
func TestDoctorTakesNoPageForAnAnswer(t *testing.T) {
	page := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Sign in</title></head><body><h1>Sign in to continue</h1></body></html>`)
		}
	}
	noAnswer := func(t *testing.T, rep doctorReport) {
		if w := rep.check(t, "worm"); w.Verdict != nil || text(w.Code) != "not-s3" {
			t.Errorf("worm = verdict %s, code %s; want null, not-s3", text(w.Verdict), text(w.Code))
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(*doctorStore)
		check func(*testing.T, doctorReport)
	}{
		{"a page with a 200 for the lock configuration", func(s *doctorStore) { s.lock = page(http.StatusOK) }, noAnswer},
		{"a page with a 404 for the lock configuration", func(s *doctorStore) { s.lock = page(http.StatusNotFound) }, noAnswer},
		{"a page with a 200 for a retention", func(s *doctorStore) { s.retention = page(http.StatusOK) },
			func(t *testing.T, rep doctorReport) {
				if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "(not-s3)") {
					t.Errorf("retention = %+v, want unreadable, not-s3", r)
				}
			}},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				store := newDoctorStore(t)
				tc.setup(store)
				args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json")
				_, out, _ := runDoctorCLI(context.Background(), t, args...)
				tc.check(t, decodeDoctor(t, out))
			})
		}
	}
}

// doctor stops at its own deadline and still prints its document, with the code timeout on the
// check that ran out. A caller that stops doctor itself gets nothing, so the default, 45 seconds,
// is under the 60 a caller might give it.
func TestDoctorStopsAtItsOwnDeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    string
		check func(*testing.T, doctorReport)
	}{
		{"the lock question never answered", "lock", func(t *testing.T, rep doctorReport) {
			if w := rep.check(t, "worm"); w.Verdict != nil || text(w.Code) != "timeout" {
				t.Errorf("worm = verdict %s, code %s; want null, timeout", text(w.Verdict), text(w.Code))
			}
		}},
		{"a retention never answered", "retention", func(t *testing.T, rep doctorReport) {
			if text(rep.check(t, "worm").Verdict) != "immutable" {
				t.Error("the lock question, answered in time, lost its verdict")
			}
			if r := rep.check(t, "retention"); text(r.Observed) != "unreadable" || !strings.Contains(r.Detail, "(timeout)") {
				t.Errorf("retention = %+v, want unreadable, timeout", r)
			}
		}},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				store := newDoctorStore(t)
				stop := make(chan struct{})
				t.Cleanup(func() { close(stop) }) // runs before the fake's Close, which waits for this handler
				hang := func(_ http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-stop:
					}
				}
				switch tc.at {
				case "lock":
					store.lock = hang
				case "retention":
					store.retention = hang
				}

				start := time.Now()
				args := append(slices.Clone(scope.args), "-config", bucketConfig(t, store.URL, ""), "-output", "json", "-timeout", "1s")
				code, out, stderr := runDoctorCLI(context.Background(), t, args...)
				if took := time.Since(start); took > 15*time.Second {
					t.Errorf("doctor took %s with a 1s deadline", took)
				}
				if destOnly(scope.args) && code != 0 {
					t.Errorf("exit %d, want 0: running out of time is not a failure unless worm.require is set\n%s", code, stderr)
				}
				tc.check(t, decodeDoctor(t, out))
			})
		}
	}
}

// The deadline is a flag, 45 seconds unless set, and a negative one is a command line that makes
// no sense: exit 2, nothing on stdout.
func TestDoctorTimeoutFlag(t *testing.T) {
	var code int
	help := captureStderr(t, func() { code = Run(context.Background(), []string{"doctor", "-h"}) })
	if !strings.Contains(help, "-timeout duration") || !strings.Contains(help, "(default 45s)") {
		t.Errorf("doctor -h (exit %d) does not show a -timeout defaulting to 45s:\n%s", code, help)
	}

	doctorEnv(t)
	store := newDoctorStore(t)
	code, out, stderr := runDoctorCLI(context.Background(), t, "-config", bucketConfig(t, store.URL, ""), "-only", "destination", "-timeout", "-1s")
	if code != 2 || out != "" || !strings.Contains(stderr, "-timeout") {
		t.Errorf("-timeout -1s: exit %d, stdout %q, stderr %q; want 2, nothing, a word about -timeout", code, out, stderr)
	}
}

// A config that cannot be loaded still gets a v1 document under --output json: one failed config
// check, with the code config, and the same exit code as before. The error stays on stderr, since
// a parse error can quote the file.
func TestDoctorReportsAConfigItCannotLoad(t *testing.T) {
	const marker = "mk-7c21a" // yaml.v3 shortens a value over 10 bytes in its errors
	dir := t.TempDir()
	write := func(name, doc string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name, path string
		stderr     string // what the error on stderr names
	}{
		{"YAML that does not parse", write("broken.yaml", "destination: [s3\n"), "parse config"},
		{"a value of the wrong type", write("typed.yaml", "backup:\n  concurrency: "+marker+"\n"), marker},
		{"no file at the path", filepath.Join(dir, "absent.yaml"), "read config"},
	} {
		for _, scope := range doctorScopes {
			t.Run(tc.name+", "+scope.name, func(t *testing.T) {
				doctorEnv(t)
				args := append(slices.Clone(scope.args), "-config", tc.path, "-output", "json")
				code, out, stderr := runDoctorCLI(context.Background(), t, args...)
				if code != 1 {
					t.Errorf("exit %d, want 1, as before", code)
				}
				if strings.Contains(out, marker) {
					t.Errorf("the config's text reached stdout:\n%s", out)
				}
				if !strings.Contains(stderr, tc.stderr) {
					t.Errorf("stderr does not carry the error (%q):\n%s", tc.stderr, stderr)
				}
				rep := decodeDoctor(t, out)
				if rep.Schema != "gitdr.doctor/v1" || rep.OK || len(rep.Checks) != 1 {
					t.Fatalf("document = %+v, want v1, not ok, with one check", rep)
				}
				c := rep.Checks[0]
				if c.Name != "config" || c.OK || text(c.Code) != "config" {
					t.Errorf("check = %+v, want config, failed, with the code config", c)
				}
				if want := []string{"code", "detail", "name", "ok"}; !slices.Equal(c.keys, want) {
					t.Errorf("the check has keys %v, want %v", c.keys, want)
				}
			})
		}
	}

	// Text output is as it was: the error on stderr and nothing on stdout.
	doctorEnv(t)
	code, out, _ := runDoctorCLI(context.Background(), t, "-config", filepath.Join(dir, "absent.yaml"))
	if code != 1 || out != "" {
		t.Errorf("text output: exit %d, stdout %q; want 1 and nothing", code, out)
	}
}

// azureEnv clears the Azure SDK's settings from the environment and has its default chain try the
// environment alone, so it finds no credential and asks no service, here or in CI.
func azureEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AZURE_") || strings.HasPrefix(k, "IDENTITY_") || k == "MSI_ENDPOINT" {
			t.Setenv(k, "") // restored when the test ends
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "EnvironmentCredential")
}

// An Azure credential the SDK's default chain cannot get is named for what it is, no-credentials.
// Nothing reached the store, so it is neither the store's answer nor an endpoint that is not the
// storage API. It read not-s3, "the endpoint did not answer as the storage API does", which sent
// an operator to the endpoint when what was missing was a credential.
func TestDoctorNamesAnAzureCredentialItCannotGet(t *testing.T) {
	for _, scope := range doctorScopes {
		t.Run(scope.name, func(t *testing.T) {
			doctorEnv(t)
			azureEnv(t)
			// Never dialled: the token is asked for before the request, and there is none to get.
			doc := "destination:\n  type: azure\n  azure:\n    account: gitdrdoctor\n    container: backups\n" +
				"    endpoint: https://127.0.0.1:9/\n"
			cfg := filepath.Join(t.TempDir(), "gitdr.yaml")
			if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append(slices.Clone(scope.args), "-config", cfg, "-output", "json")
			code, out, stderr := runDoctorCLI(context.Background(), t, args...)
			w := decodeDoctor(t, out).check(t, "worm")
			if w.Verdict != nil || text(w.Code) != "no-credentials" || !strings.Contains(w.Detail, "(no-credentials)") {
				t.Errorf("worm = verdict %s, code %s, %q; want null, no-credentials", text(w.Verdict), text(w.Code), w.Detail)
			}
			if destOnly(scope.args) && code != 0 {
				t.Errorf("exit %d, want 0: an unread lock is not a failure unless worm.require is set", code)
			}
			// The SDK's own account of what it tried goes to stderr, with the log.
			if !strings.Contains(stderr, "EnvironmentCredential") {
				t.Errorf("stderr does not say which credentials were tried:\n%s", stderr)
			}
		})
	}
}

// A lock whose date has passed is not called present. The release gates sampled objects on B2 held
// until 2026-07-04 and 2026-09-14, months before, and doctor said "an object here is held until"
// each of them.
func TestDoctorCallsAnEndedLockLapsed(t *testing.T) {
	ended := answer(http.StatusOK, `<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		`<Mode>COMPLIANCE</Mode><RetainUntilDate>2026-07-04T00:00:00Z</RetainUntilDate></Retention>`)
	for _, require := range []bool{false, true} {
		t.Run(fmt.Sprintf("worm.require %v", require), func(t *testing.T) {
			doctorEnv(t)
			store := newDoctorStore(t)
			store.retention = ended
			extra := ""
			if require {
				extra = "worm:\n  require: true\n"
			}
			code, out, _ := runDoctorCLI(context.Background(), t,
				"-config", bucketConfig(t, store.URL, extra), "-only", "destination", "-output", "json")
			r := decodeDoctor(t, out).check(t, "retention")
			if text(r.Observed) != "lapsed" || !strings.Contains(r.Detail, "ended on 2026-07-04T00:00:00Z") || strings.Contains(r.Detail, "held until") {
				t.Errorf("retention = observed %s, %q; want lapsed, saying the lock ended on 2026-07-04T00:00:00Z", text(r.Observed), r.Detail)
			}
			// The store applied a lock and it ran its course, as every lock does. That is not a
			// bucket that drops locks, so the check passes, worm.require or not.
			if !r.OK || code != 0 {
				t.Errorf("retention ok %v, exit %d; want ok and 0", r.OK, code)
			}
		})
	}
}
