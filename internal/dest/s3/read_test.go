package s3_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// readerStore is a fake S3 endpoint that answers every request with one handler and records the
// query of each.
type readerStore struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newReaderStore(t *testing.T, h http.HandlerFunc) *readerStore {
	t.Helper()
	s := &readerStore{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.Method+" "+r.URL.RawQuery)
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *readerStore) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// newReaderBackend is the S3 backend pointed at the fake, with a static key and nothing from the
// environment's AWS configuration.
func newReaderBackend(t *testing.T, endpoint string) *s3backend.Backend {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AWS_") {
			t.Setenv(k, "")
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDREADERTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "reader-test-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent"))
	b, err := s3backend.New(context.Background(), s3backend.Options{
		Bucket: "b", Region: "us-east-1", Endpoint: endpoint, UsePathStyle: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func xmlAnswer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// ListPage is one ListObjectsV2 request for the number of keys asked for, and it hands back the
// store's word on whether there are more instead of asking for them.
func TestListPageSendsOneRequest(t *testing.T) {
	store := newReaderStore(t, xmlAnswer(http.StatusOK, `<ListBucketResult><Name>b</Name><KeyCount>1</KeyCount>`+
		`<IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken>`+
		`<Contents><Key>a</Key><Size>3</Size></Contents><Contents><Key>b</Key><Size>4</Size></Contents></ListBucketResult>`))
	b := newReaderBackend(t, store.URL)

	objs, more, err := b.ListPage(context.Background(), "github.com/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != "a" || objs[0].Size != 3 {
		t.Errorf("objects = %+v, want only a, 3 bytes: a store that sends more than asked is read no further", objs)
	}
	if !more {
		t.Error("more = false on a page the store marked truncated")
	}
	seen := store.seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests, want 1: %v", len(seen), seen)
	}
	if q := seen[0]; !strings.HasPrefix(q, "GET ") || !strings.Contains(q, "list-type=2") ||
		!strings.Contains(q, "max-keys=1") || !strings.Contains(q, "prefix=github.com") || strings.Contains(q, "continuation-token") {
		t.Errorf("request = %q, want one ListObjectsV2 for one key under the prefix, with no continuation", q)
	}

	for _, limit := range []int{0, -1, 1001} {
		if _, _, err := b.ListPage(context.Background(), "", limit); err == nil {
			t.Errorf("ListPage(%d) did not refuse", limit)
		}
	}
	if n := len(store.seen()); n != 1 {
		t.Errorf("a refused page size still reached the store: %d requests", n)
	}
}

// LimitResponses caps every answer the backend reads, error answers included, and leaves a short
// answer, and where the backend sends it, exactly as they were.
func TestLimitResponsesCapsEveryAnswer(t *testing.T) {
	const capBytes = 64 << 10
	padded := func(status int, open, close string) http.HandlerFunc {
		return xmlAnswer(status, open+strings.Repeat("x", 4*capBytes)+close)
	}
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"an answer", padded(http.StatusOK, `<ObjectLockConfiguration><Padding>`, `</Padding><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)},
		{"an error", padded(http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>`, `</Message></Error>`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Uncapped, the backend reads all of it, which is what backup needs and doctor must not do.
			whole := newReaderBackend(t, newReaderStore(t, tc.h).URL)
			if _, err := whole.VerifyWorm(context.Background()); errors.Is(err, dest.ErrResponseTooLarge) {
				t.Fatalf("an uncapped backend refused a long answer: %v", err)
			}

			capped := newReaderBackend(t, newReaderStore(t, tc.h).URL)
			capped.LimitResponses(capBytes)
			_, err := capped.VerifyWorm(context.Background())
			if !errors.Is(err, dest.ErrResponseTooLarge) {
				t.Errorf("err = %v, want ErrResponseTooLarge", err)
			}
		})
	}

	t.Run("a short answer", func(t *testing.T) {
		store := newReaderStore(t, xmlAnswer(http.StatusOK, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>`+
			`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`))
		b := newReaderBackend(t, store.URL)
		b.LimitResponses(capBytes)
		st, err := b.VerifyWorm(context.Background())
		if err != nil || !st.Verdict.Immutable() || st.Mode != "COMPLIANCE" {
			t.Errorf("VerifyWorm = %+v, %v; want immutable, COMPLIANCE", st, err)
		}
		if seen := store.seen(); len(seen) != 1 || !strings.Contains(seen[0], "object-lock") {
			t.Errorf("requests = %v, want the one lock read, at the configured endpoint", seen)
		}
	})
}

// A refusal keeps the store's error for a log, and puts only a shaped code in Details, which a
// signed manifest and doctor's document both carry.
func TestVerifyWormKeepsTheRefusalAndShapesWhatItWrites(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		details     string
		code        string // the refusal's code, as the store wrote it; "" for no refusal
		notInDetail string
	}{
		{"a refusal", `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`, http.StatusForbidden,
			"could not verify immutability: the bucket answered AccessDenied", "AccessDenied", ""},
		{"a refusal whose code is not shaped like one", `<Error><Code>gitdr-marker</Code></Error>`, http.StatusForbidden,
			"could not verify immutability: the bucket answered unnamed", "gitdr-marker", "gitdr-marker"},
		{"no lock configuration, which is an answer", `<Error><Code>ObjectLockConfigurationNotFoundError</Code></Error>`, http.StatusNotFound,
			"bucket has no Object Lock configuration", "", ""},
		{"a mode that is not shaped like one", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
			`<Rule><DefaultRetention><Mode>gitdr-marker</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`, http.StatusOK,
			"Object Lock enabled; default retention unnamed", "", "gitdr-marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newReaderBackend(t, newReaderStore(t, xmlAnswer(tc.status, tc.body)).URL)
			st, err := b.VerifyWorm(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Details != tc.details {
				t.Errorf("details = %q, want %q", st.Details, tc.details)
			}
			if tc.notInDetail != "" && strings.Contains(st.Details, tc.notInDetail) {
				t.Errorf("the store's text is in Details: %q", st.Details)
			}
			var api smithy.APIError
			switch {
			case tc.code == "" && st.Refusal != nil:
				t.Errorf("Refusal = %v on an answer", st.Refusal)
			case tc.code != "" && (!errors.As(st.Refusal, &api) || api.ErrorCode() != tc.code):
				t.Errorf("Refusal = %v, want the store's error with code %q", st.Refusal, tc.code)
			}
		})
	}
}
