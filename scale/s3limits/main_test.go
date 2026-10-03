//go:build scale

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// The proxy's own checks, against a fake store. Each limit and each fault is made to fire and is
// watched firing, so a scale run that reports one of them can be believed.

// fakeStore answers every request 200 and remembers what reached it.
type fakeStore struct {
	mu   sync.Mutex
	seen []string // "METHOD path?query"
	body map[string]int64
	// The last request, whole.
	lastBody   []byte
	lastHeader http.Header
	lastLength int64
}

func newFakeStore(t *testing.T) (*fakeStore, *httptest.Server) {
	t.Helper()
	fs := &fakeStore{body: map[string]int64{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.seen = append(fs.seen, r.Method+" "+r.URL.RequestURI())
		fs.body[r.URL.Path] += int64(len(b))
		fs.lastBody, fs.lastHeader, fs.lastLength = b, r.Header.Clone(), r.ContentLength
		fs.mu.Unlock()
		if _, ok := r.URL.Query()["uploads"]; ok && r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`))
			return
		}
		w.Header().Set("ETag", `"e"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return fs, srv
}

func (fs *fakeStore) requests() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.seen...)
}

func newTestProxy(t *testing.T, set settings) (*proxy, *httptest.Server, *fakeStore) {
	t.Helper()
	store, upstream := newFakeStore(t)
	pool := upstream.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	p, err := newProxy(upstream.URL, pool, set)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return p, front, store
}

// reply is what do read back: the status, the body already read and closed.
type reply struct{ StatusCode int }

func do(t *testing.T, method, url string, body io.Reader, size int64, hdr map[string]string) (reply, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return reply{StatusCode: resp.StatusCode}, string(b)
}

// unreadable fails the test if anything reads it: a refusal must come before the body.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Error("the body was read; the refusal should have come from the headers")
	return 0, errors.New("read")
}

func noRate() settings {
	s := defaultSettings()
	s.RateBytesPerSecond = 0
	return s
}

func TestAPutOverFiveGiBIsRefusedBeforeItsBody(t *testing.T) {
	_, front, store := newTestProxy(t, noRate())
	// The SDK asks before it sends a large body, and the refusal is the answer it gets.
	expect := map[string]string{"Expect": "100-continue"}
	resp, body := do(t, http.MethodPut, front.URL+"/b/big.bundle", unreadable{t}, 6<<30, expect)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "<Code>EntityTooLarge</Code>") {
		t.Fatalf("got %d %s, want 400 EntityTooLarge", resp.StatusCode, body)
	}
	if got := store.requests(); len(got) != 0 {
		t.Errorf("the store saw %v", got)
	}

	// The SDK sends aws-chunked over TLS, where Content-Length counts the framing and the object
	// size is in X-Amz-Decoded-Content-Length.
	resp, body = do(t, http.MethodPut, front.URL+"/b/big2.bundle", unreadable{t}, 6<<30+100,
		map[string]string{"Expect": "100-continue", "X-Amz-Decoded-Content-Length": fmt.Sprint(int64(6 << 30)), "Content-Encoding": "aws-chunked"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "EntityTooLarge") {
		t.Fatalf("aws-chunked: got %d %s, want EntityTooLarge", resp.StatusCode, body)
	}
}

func TestALockedWriteNeedsAChecksum(t *testing.T) {
	_, front, store := newTestProxy(t, noRate())
	lock := map[string]string{"X-Amz-Object-Lock-Mode": "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date": "2030-01-01T00:00:00Z"}
	resp, body := do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader("abc"), 3, lock)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("got %d %s, want 400 InvalidRequest", resp.StatusCode, body)
	}
	lock["X-Amz-Trailer"] = "x-amz-checksum-crc32"
	if resp, body = do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader("abc"), 3, lock); resp.StatusCode != http.StatusOK {
		t.Fatalf("with a checksum trailer: got %d %s", resp.StatusCode, body)
	}
	if got := store.requests(); len(got) != 1 {
		t.Errorf("the store saw %v, want the one write with a checksum", got)
	}
}

func TestIfNoneMatchLikeAWSOrLikeB2(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int
	}{{"aws", http.StatusOK}, {"b2", http.StatusNotImplemented}} {
		set := noRate()
		set.IfNoneMatch = tc.mode
		_, front, _ := newTestProxy(t, set)
		resp, body := do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader("x"), 1, map[string]string{"If-None-Match": "*"})
		if resp.StatusCode != tc.want {
			t.Errorf("%s: got %d %s, want %d", tc.mode, resp.StatusCode, body, tc.want)
		}
	}
}

func TestMultipartLimits(t *testing.T) {
	set := noRate()
	set.MinPartBytes = 8 // small, so the test sends bytes rather than megabytes
	_, front, _ := newTestProxy(t, set)

	resp, body := do(t, http.MethodPost, front.URL+"/b/obj?uploads", nil, 0, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "up-1") {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPut, front.URL+"/b/obj?partNumber=10001&uploadId=up-1", strings.NewReader("x"), 1, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "InvalidArgument") {
		t.Errorf("part 10001: %d %s", resp.StatusCode, body)
	}
	for n, data := range map[int]string{1: "tiny", 2: "the last part"} {
		if resp, body = do(t, http.MethodPut, fmt.Sprintf("%s/b/obj?partNumber=%d&uploadId=up-1", front.URL, n),
			strings.NewReader(data), int64(len(data)), nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("part %d: %d %s", n, resp.StatusCode, body)
		}
	}
	complete := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber></Part><Part><PartNumber>2</PartNumber></Part></CompleteMultipartUpload>`
	resp, body = do(t, http.MethodPost, front.URL+"/b/obj?uploadId=up-1", strings.NewReader(complete), int64(len(complete)), nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "EntityTooSmall") {
		t.Errorf("complete with a 4-byte first part: %d %s, want EntityTooSmall", resp.StatusCode, body)
	}
}

func addFault(t *testing.T, front *httptest.Server, f fault) int {
	t.Helper()
	b, _ := json.Marshal(f)
	resp, body := do(t, http.MethodPost, front.URL+"/_s3limits/faults", bytes.NewReader(b), int64(len(b)), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add fault: %d %s", resp.StatusCode, body)
	}
	var out struct{ ID int }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}

func TestFaults(t *testing.T) {
	t.Run("a 500 on the second part", func(t *testing.T) {
		_, front, store := newTestProxy(t, noRate())
		addFault(t, front, fault{Op: "UploadPart", Nth: 2, Action: "status", Status: 500})
		var got []int
		for n := 1; n <= 3; n++ {
			resp, _ := do(t, http.MethodPut, fmt.Sprintf("%s/b/o?partNumber=%d&uploadId=u", front.URL, n), strings.NewReader("p"), 1, nil)
			got = append(got, resp.StatusCode)
		}
		if fmt.Sprint(got) != "[200 500 200]" {
			t.Errorf("statuses %v, want [200 500 200]", got)
		}
		if n := len(store.requests()); n != 2 {
			t.Errorf("the store saw %d parts, want 2", n)
		}
	})

	t.Run("a dropped response after the write landed", func(t *testing.T) {
		p, front, store := newTestProxy(t, noRate())
		addFault(t, front, fault{Op: "PutObject", KeyRegex: `\.bundle$`, Action: "drop-after-commit"})
		req, _ := http.NewRequest(http.MethodPut, front.URL+"/b/r.bundle", strings.NewReader("data"))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("the client got a response (%d); it should have lost the connection", resp.StatusCode)
		}
		if got := store.requests(); len(got) != 1 {
			t.Fatalf("the store saw %v, want the write", got)
		}
		// Sent again, as a client that never saw an answer does: counted as bytes sent twice.
		do(t, http.MethodPut, front.URL+"/b/r.bundle", strings.NewReader("data"), 4, nil)
		p.mu.Lock()
		resent, bytes := p.st.WritesResent, p.st.BytesResent
		p.mu.Unlock()
		if resent != 1 || bytes != 4 {
			t.Errorf("resent %d writes, %d bytes; want 1 and 4", resent, bytes)
		}
	})

	t.Run("a held write waits for its release", func(t *testing.T) {
		_, front, store := newTestProxy(t, noRate())
		id := addFault(t, front, fault{Op: "PutObject", Action: "hold-before"})
		done := make(chan int, 1)
		go func() {
			resp, _ := do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader("x"), 1, nil)
			done <- resp.StatusCode
		}()
		waitHolding(t, front, id)
		if n := len(store.requests()); n != 0 {
			t.Fatalf("a held write reached the store")
		}
		do(t, http.MethodPost, front.URL+"/_s3limits/faults/release", nil, 0, nil)
		select {
		case code := <-done:
			if code != http.StatusOK || len(store.requests()) != 1 {
				t.Errorf("after release: %d, store saw %v", code, store.requests())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the write was not released")
		}
	})
}

func waitHolding(t *testing.T, front *httptest.Server, id int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, body := do(t, http.MethodGet, front.URL+"/_s3limits/faults", nil, 0, nil)
		var fs []fault
		_ = json.Unmarshal([]byte(body), &fs)
		for _, f := range fs {
			if f.ID == id && f.Holding > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing was held")
}

// The SDK over TLS sends a body of known length as one aws-chunked chunk. MinIO refuses a chunk
// over 16 MiB and AWS does not, so the proxy splits it and signs the request again.
func TestASingleChunkIsSplitAndSignedAgain(t *testing.T) {
	set := noRate()
	set.RechunkBytes = 1000
	p, front, store := newTestProxy(t, set)
	p.creds = aws.Credentials{AccessKeyID: "AKIDSTORE", SecretAccessKey: "store-secret"}

	data := bytes.Repeat([]byte("0123456789"), 300) // 3,000 bytes
	trailer := "x-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	body := fmt.Sprintf("%x\r\n%s\r\n0\r\n%s", len(data), data, trailer)
	hdr := map[string]string{
		"X-Amz-Content-Sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"Content-Encoding":             "aws-chunked",
		"X-Amz-Decoded-Content-Length": fmt.Sprint(len(data)),
		"X-Amz-Trailer":                "x-amz-checksum-crc32",
		"Authorization":                "AWS4-HMAC-SHA256 Credential=AKIDCLIENT/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00",
	}
	if resp, b := do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader(body), int64(len(body)), hdr); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d %s", resp.StatusCode, b)
	}
	store.mu.Lock()
	got, gotHeader, gotLength := string(store.lastBody), store.lastHeader, store.lastLength
	store.mu.Unlock()

	want := "3e8\r\n" + string(data[:1000]) + "\r\n3e8\r\n" + string(data[1000:2000]) + "\r\n3e8\r\n" + string(data[2000:]) + "\r\n0\r\n" + trailer
	if got != want {
		t.Errorf("the store got\n%q\nwant\n%q", got, want)
	}
	if gotLength != int64(len(want)) {
		t.Errorf("content length %d, want %d: the length is signed, so it has to be the new one", gotLength, len(want))
	}
	if a := gotHeader.Get("Authorization"); !strings.Contains(a, "Credential=AKIDSTORE/") {
		t.Errorf("not signed again with the store's key: %s", a)
	}

	// Off, it goes as it came.
	set.RechunkBytes = 0
	p.mu.Lock()
	p.set = set
	p.mu.Unlock()
	do(t, http.MethodPut, front.URL+"/b/k2", strings.NewReader(body), int64(len(body)), hdr)
	store.mu.Lock()
	got = string(store.lastBody)
	store.mu.Unlock()
	if got != body {
		t.Errorf("with rechunkBytes 0 the body changed")
	}
}

func TestRequestsAreCountedByOperation(t *testing.T) {
	_, front, _ := newTestProxy(t, noRate())
	do(t, http.MethodPut, front.URL+"/b/k", strings.NewReader("abc"), 3, nil)
	do(t, http.MethodHead, front.URL+"/b/k", nil, 0, nil)
	do(t, http.MethodGet, front.URL+"/b?list-type=2&prefix=x", nil, 0, nil)
	do(t, http.MethodGet, front.URL+"/b/k?retention", nil, 0, nil)
	_, body := do(t, http.MethodGet, front.URL+"/_s3limits/stats", nil, 0, nil)
	var st stats
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	for op, want := range map[string]int64{"PutObject": 1, "HeadObject": 1, "ListObjectsV2": 1, "GetObjectRetention": 1} {
		if got := st.Ops[op]; got == nil || got.Count != want {
			t.Errorf("%s counted %v, want %d", op, got, want)
		}
	}
	if st.Ops["PutObject"].BytesIn != 3 {
		t.Errorf("PutObject bytes in = %d, want 3", st.Ops["PutObject"].BytesIn)
	}
}

func TestTheThrottleHoldsTheRate(t *testing.T) {
	set := defaultSettings()
	set.RateBytesPerSecond = 1 << 20
	_, front, _ := newTestProxy(t, set)
	payload := bytes.Repeat([]byte("x"), 1<<20)
	start := time.Now()
	do(t, http.MethodPut, front.URL+"/b/k", bytes.NewReader(payload), int64(len(payload)), nil)
	// A quarter of a second may be banked, so 1 MiB at 1 MiB/s takes at least three quarters.
	if took := time.Since(start); took < 700*time.Millisecond {
		t.Errorf("1 MiB at 1 MiB/s took %s", took)
	}
}
