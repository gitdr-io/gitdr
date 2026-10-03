package s3_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// What reaches the store when gitdr writes an object, over TLS, the way it does in production.
//
// The fake refuses what MinIO refuses: an aws-chunked chunk over 16 MiB. It also checks every
// checksum it is given, header or trailer, against the bytes that arrived, the way S3 does.

const minioMaxChunk = 16 << 20

type received struct {
	encoding    string // Content-Encoding
	length      int64  // Content-Length
	payloadHash string // X-Amz-Content-Sha256
	crc32Header string // X-Amz-Checksum-Crc32
	lockMode    string
	retainUntil string
	data        []byte // the object, decoded from aws-chunked when it came that way
}

type fakeS3 struct {
	mu   sync.Mutex
	puts []received
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound) // nothing exists yet: the create-only check passes
		case http.MethodPut:
			f.put(w, r)
		default:
			http.Error(w, "not in this fake", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) writes() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.puts...)
}

func (f *fakeS3) put(w http.ResponseWriter, r *http.Request) {
	got := received{
		encoding:    r.Header.Get("Content-Encoding"),
		length:      r.ContentLength,
		payloadHash: r.Header.Get("X-Amz-Content-Sha256"),
		crc32Header: r.Header.Get("X-Amz-Checksum-Crc32"),
		lockMode:    r.Header.Get("X-Amz-Object-Lock-Mode"),
		retainUntil: r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"),
	}
	want := got.crc32Header
	if strings.Contains(got.encoding, "aws-chunked") {
		data, trailers, err := decodeChunked(r.Body)
		if err != nil {
			s3Error(w, "BadRequest", err.Error())
			return
		}
		got.data, want = data, trailers["x-amz-checksum-crc32"]
	} else {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, "IncompleteBody", err.Error())
			return
		}
		got.data = data
	}
	if want == "" {
		s3Error(w, "InvalidRequest", "Content-MD5 OR x-amz-checksum- HTTP header is required")
		return
	}
	if sum := crc32Base64(got.data); sum != want {
		s3Error(w, "BadDigest", fmt.Sprintf("the CRC32 you specified, %s, did not match the body's, %s", want, sum))
		return
	}
	f.mu.Lock()
	f.puts = append(f.puts, got)
	f.mu.Unlock()
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
}

// decodeChunked reads an unsigned aws-chunked body, refusing a chunk over 16 MiB as MinIO does.
func decodeChunked(body io.Reader) ([]byte, map[string]string, error) {
	br := bufio.NewReader(body)
	var data []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("chunk header: %w", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("chunk size %q: %w", line, err)
		}
		if size > minioMaxChunk {
			return nil, nil, fmt.Errorf("chunk too big: choose chunk size <= 16MiB")
		}
		if size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, nil, fmt.Errorf("chunk data: %w", err)
		}
		data = append(data, chunk...)
		if _, err := br.Discard(2); err != nil {
			return nil, nil, fmt.Errorf("chunk end: %w", err)
		}
	}
	trailers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
		if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			trailers[strings.ToLower(name)] = value
		}
	}
	return data, trailers, nil
}

func s3Error(w http.ResponseWriter, code, msg string) {
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: msg})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(append([]byte(xml.Header), body...))
}

func crc32Base64(b []byte) string {
	sum := crc32.NewIEEE()
	_, _ = sum.Write(b)
	return base64.StdEncoding.EncodeToString(sum.Sum(nil))
}

// newBackend points the S3 destination at the fake over TLS, trusting the fake's certificate
// through AWS_CA_BUNDLE as an operator's store would be trusted, and keeps the machine's own AWS
// setup out of it.
func newBackend(t *testing.T, srv *httptest.Server) *s3backend.Backend {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CA_BUNDLE": ca, "AWS_ACCESS_KEY_ID": "AKIDTEST", "AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_CONFIG_FILE": empty, "AWS_SHARED_CREDENTIALS_FILE": empty,
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	b, err := s3backend.New(context.Background(), s3backend.Options{
		Bucket: "backups", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An object over 16 MiB goes as a plain body of its own length with its CRC32 in a header.
//
// Before, the SDK sent it aws-chunked, the whole object as one chunk with the checksum in a
// trailer, and MinIO refused every object over 16 MiB written over TLS: every bundle of a real
// repository. The write is a file as the pipeline makes one, and a byte slice as the manifest and
// metadata are.
func TestAnObjectOverSixteenMiBGoesWithItsChecksumInAHeader(t *testing.T) {
	payload := bytes.Repeat([]byte("gitdr bundle bytes\n"), (20<<20)/19+1) // a little over 20 MiB
	path := filepath.Join(t.TempDir(), "repo.bundle")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	for _, tc := range []struct {
		name string
		open func(t *testing.T) io.Reader
	}{
		{"a file", func(t *testing.T) io.Reader {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			return f
		}},
		{"a byte slice", func(*testing.T) io.Reader { return bytes.NewReader(payload) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeS3(t)
			b := newBackend(t, srv)
			res, err := b.PutImmutable(context.Background(), "github.com/octo/hello/2026-10-03/hello.bundle",
				tc.open(t), int64(len(payload)), dest.Retention{Mode: dest.RetentionCompliance, Until: until})
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if res.Size != int64(len(payload)) || !res.RetainUntil.Equal(until) {
				t.Errorf("result %+v", res)
			}
			puts := fake.writes()
			if len(puts) != 1 {
				t.Fatalf("%d writes reached the store, want 1", len(puts))
			}
			got := puts[0]
			if strings.Contains(got.encoding, "aws-chunked") {
				t.Errorf("the body went aws-chunked, Content-Encoding %q", got.encoding)
			}
			if got.length != int64(len(payload)) {
				t.Errorf("Content-Length %d, want the object's own %d", got.length, len(payload))
			}
			if want := crc32Base64(payload); got.crc32Header != want {
				t.Errorf("X-Amz-Checksum-Crc32 %q, want %q", got.crc32Header, want)
			}
			if got.payloadHash != "UNSIGNED-PAYLOAD" {
				t.Errorf("X-Amz-Content-Sha256 %q, want UNSIGNED-PAYLOAD over TLS", got.payloadHash)
			}
			if got.lockMode != "COMPLIANCE" || got.retainUntil != "2030-01-02T03:04:05Z" {
				t.Errorf("lock headers %q %q, want COMPLIANCE until 2030-01-02T03:04:05Z", got.lockMode, got.retainUntil)
			}
		})
	}
}

// The checksum covers the bytes from where the reader stands, which are the bytes the SDK sends.
// One computed from the start of the reader would not match the body, and the store would refuse
// the write.
func TestTheChecksumCoversTheBytesFromWhereTheReaderStands(t *testing.T) {
	fake, srv := newFakeS3(t)
	b := newBackend(t, srv)
	r := bytes.NewReader([]byte("skip this|the object"))
	if _, err := r.Seek(int64(len("skip this|")), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutImmutable(context.Background(), "k", r, int64(len("the object")), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if puts := fake.writes(); len(puts) != 1 || string(puts[0].data) != "the object" {
		t.Errorf("the store got %+v", puts)
	}
}

// A reader that cannot rewind has no checksum computed ahead, and the SDK still sends one, as a
// trailer. Nothing in gitdr passes such a reader; this keeps the write from going out bare.
func TestAReaderThatCannotRewindStillCarriesAChecksum(t *testing.T) {
	fake, srv := newFakeS3(t)
	b := newBackend(t, srv)
	data := []byte(`{"schema":"gitdr.meta/v1"}`)
	onlyReads := struct{ io.Reader }{bytes.NewReader(data)}
	if _, err := b.PutImmutable(context.Background(), "k", onlyReads, int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if puts := fake.writes(); len(puts) != 1 || !bytes.Equal(puts[0].data, data) {
		t.Fatalf("the store got %+v", puts)
	}
}
