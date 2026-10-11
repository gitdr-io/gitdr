package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"gitdr.io/gitdr/internal/dest"
)

// Runs against an in-process GCS emulator (no Docker, no real bucket). The emulator
// can't model a locked retention policy, so real WORM-gate behavior is validated
// against a real bucket separately.
func TestGCSBackend(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	const bucket = "gitdr-test"
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})

	ctx := context.Background()
	b := newBackend(srv.Client(), bucket, nil)

	key := "github.com/octo/hello/2026-06-13/hello.bundle"
	data := []byte("bundle-bytes")
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// create-only: a second put to the same key must fail (DoesNotExist precondition).
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{}); err == nil {
		t.Error("expected create-only conflict on second put")
	}

	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("get mismatch: %q", got)
	}

	objs, err := b.List(ctx, "github.com/octo/hello/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) != 1 || objs[0].Key != key {
		t.Fatalf("list = %+v", objs)
	}
	// When the object was written, which a same-day rerun holds against the date in its key.
	if got := objs[0].LastModified; got.IsZero() || time.Since(got).Abs() > 10*time.Minute {
		t.Errorf("list says %s was written at %v, want about now", key, got)
	}

	st, err := b.VerifyWorm(ctx)
	if err != nil {
		t.Fatalf("verifyworm: %v", err)
	}
	if st.Verdict.Immutable() {
		t.Error("emulator should not report a locked retention policy")
	}
}

// ListPage names the objects on the first page of a listing and says whether there are more,
// which is how doctor finds an object to read a retention from without walking the bucket.
func TestGCSListPage(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	const bucket = "gitdr-listpage"
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	ctx := context.Background()
	b := newBackend(srv.Client(), bucket, nil)

	for _, key := range []string{"github.com/octo/a.bundle", "github.com/octo/b.bundle", "github.com/octo/c.bundle"} {
		data := []byte(key)
		if _, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	objs, more, err := b.ListPage(ctx, "github.com/octo/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || !strings.HasPrefix(objs[0].Key, "github.com/octo/") || objs[0].Size == 0 {
		t.Errorf("first page = %+v, want one object under the prefix, with its size", objs)
	}
	if !more {
		t.Error("more = false with two objects after the page")
	}

	objs, more, err = b.ListPage(ctx, "github.com/nobody/", 1)
	if err != nil || len(objs) != 0 || more {
		t.Errorf("an empty prefix gave %+v, more %v, %v; want nothing and no more", objs, more, err)
	}

	if _, _, err := b.ListPage(ctx, "", 0); err == nil {
		t.Error("a page of no objects was not refused")
	}
}

// emulator is fake-gcs-server holding bucket, behind a handler that keeps what each resumable
// upload was sent. publicHost is the host its reads are addressed to.
func emulator(t *testing.T, bucket, publicHost string) (http.Handler, *chunkLog) {
	t.Helper()
	fake, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true, PublicHost: publicHost})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Stop)
	fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	chunks := &chunkLog{ranges: map[string][]string{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A chunk of a resumable upload goes to the session's own URL, which names the object.
		if q := r.URL.Query(); q.Has("upload_id") {
			chunks.add(q.Get("name"), r.Header.Get("Content-Range"))
		}
		fake.HTTPHandler().ServeHTTP(w, r)
	}), chunks
}

// overHTTP serves the emulator on a port of this machine and reaches it the way gitdr reaches one,
// through Options.Endpoint, so over a real HTTP transport, which stops a request in flight when
// its context is cancelled.
func overHTTP(t *testing.T, bucket string) (*Backend, *chunkLog) {
	t.Helper()
	h, chunks := emulator(t, bucket, "127.0.0.1")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	b, err := New(context.Background(), Options{Bucket: bucket, Endpoint: srv.URL + "/storage/v1/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, chunks
}

// inProcess hands every request straight to the emulator's handler, as fakestorage's own Client
// does, whatever its context says. A cancel stops nothing in flight there, so what reaches the
// emulator is all the library chose to send.
func inProcess(t *testing.T, bucket string) (*Backend, *chunkLog) {
	t.Helper()
	h, chunks := emulator(t, bucket, "storage.googleapis.com")
	client, err := storage.NewClient(context.Background(),
		option.WithHTTPClient(&http.Client{Transport: handlerTransport{h}}), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return newBackend(client, bucket, nil), chunks
}

// handlerTransport answers a request with h, in process.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// chunkLog keeps the Content-Range of every chunk sent to a resumable upload, by object name. A
// chunk that ends the upload names the object's size after the slash, and any other says "*".
type chunkLog struct {
	mu     sync.Mutex
	ranges map[string][]string
}

func (c *chunkLog) add(name, contentRange string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ranges[name] = append(c.ranges[name], contentRange)
}

func (c *chunkLog) of(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ranges[name])
}

// A stream that ends before the artifact's size, holds more than it, or fails part way is not the
// artifact that was checksummed, and it must not become the object at the key. Up to v0.1.22 the
// size was never looked at, and a failed read was followed by Close, which is what commits a Cloud
// Storage upload: whatever had been read became the object, and writes are create-only, so the key
// could take no other copy. The upload is now abandoned before anything ends it, and the artifact
// itself, written next, finds the key free.
//
// Below the library's chunk, 16 MiB, an object goes in one request, and from a chunk up in a
// resumable session that commits on its last chunk. Each is a commit, so each is tried. Past a
// chunk the emulator is shown to hold part of the stream when the write is refused, and never to
// have been sent the chunk that would have ended it.
//
// Each case runs over HTTP, as gitdr reaches an emulator, and again in process, where nothing but
// the library stands between a cancelled upload and the emulator.
func TestGCSCommitsNothingFromAStreamOfAnotherSize(t *testing.T) {
	// NewWriter's chunk, which the backend leaves as it is.
	const chunk = googleapi.DefaultUploadChunkSize
	readFailed := errors.New("read failed")
	cases := []struct {
		name         string
		size, stream int
		then         error // what the stream says after its bytes, in place of io.EOF
	}{
		{"one byte short, in one request", 100, 99, nil},
		{"one byte over, in one request", 100, 101, nil},
		{"a read that fails part way, in one request", 100, 50, readFailed},
		{"nothing, for an artifact that has bytes", 10, 0, nil},
		{"bytes, for an artifact that has none", 0, 10, nil},
		{"short, in the second chunk", chunk + 1000, chunk + 500, nil},
		{"over, in the second chunk", chunk + 500, chunk + 501, nil},
		{"a read that fails part way, in the second chunk", chunk + 1000, chunk + 500, readFailed},
		{"short, its end wrapped in an error", chunk + 1000, chunk + 500, fmt.Errorf("the pipe closed: %w", io.EOF)},
	}
	for _, via := range []struct {
		name string
		open func(t *testing.T, bucket string) (*Backend, *chunkLog)
	}{
		{"over HTTP", overHTTP},
		{"in process", inProcess},
	} {
		for _, tc := range cases {
			t.Run(via.name+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				b, chunks := via.open(t, "gitdr-length")
				data := make([]byte, max(tc.size, tc.stream))
				if _, err := rand.Read(data); err != nil {
					t.Fatal(err)
				}
				var stream io.Reader = bytes.NewReader(data[:tc.stream])
				if tc.then != nil {
					stream = io.MultiReader(stream, iotest.ErrReader(tc.then))
				}
				const key = "github.com/octo/hello/2026-06-13/hello.bundle"
				if _, err := b.PutImmutable(ctx, key, stream, int64(tc.size), dest.Retention{}); err == nil {
					t.Fatalf("a stream of %d bytes was stored as the %d-byte artifact", tc.stream, tc.size)
				}
				if objs, err := b.List(ctx, key); err != nil || len(objs) != 0 {
					t.Fatalf("after the refusal the key holds %+v (%v), want nothing", objs, err)
				}
				if tc.stream > chunk {
					sent := chunks.of(key)
					if len(sent) == 0 {
						t.Fatal("no chunk of the stream reached the emulator, so this proves the one-request path again")
					}
					for _, r := range sent {
						if !strings.HasSuffix(r, "/*") {
							t.Errorf("the emulator was sent %q, a chunk that ends the upload", r)
						}
					}
				}

				res, err := b.PutImmutable(ctx, key, bytes.NewReader(data[:tc.size]), int64(tc.size), dest.Retention{})
				if err != nil {
					t.Fatalf("the artifact itself, written next, was refused: %v", err)
				}
				if res.Key != key || res.Size != int64(tc.size) {
					t.Errorf("put recorded %q, %d bytes; want %q, %d", res.Key, res.Size, key, tc.size)
				}
				rc, err := b.Get(ctx, key)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				got, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data[:tc.size]) {
					t.Errorf("the object read back is %d bytes and not the %d-byte artifact", len(got), tc.size)
				}
			})
		}
	}
}

// PutImmutable commits only once io.Copy has ended without an error, which io.Copy does only when
// its source says io.EOF. So exactReader may say io.EOF only where the stream ends at the
// artifact's size, however the stream hands its bytes over, and it must never pass on a byte past
// size. Read here as io.Copy reads into the Cloud Storage writer, which is no io.ReaderFrom: into
// one buffer, what each read gives written before its error is looked at.
func TestExactReaderEndsOnlyWhereTheArtifactDoes(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuv")
	sources := []struct {
		name string
		wrap func(io.Reader) io.Reader
		ends bool // the stream ends after its bytes, rather than failing
	}{
		{"as it is", func(r io.Reader) io.Reader { return r }, true},
		{"a byte at a time", iotest.OneByteReader, true},
		{"half of each read", iotest.HalfReader, true},
		{"its end with its last bytes", iotest.DataErrReader, true},
		// io.Copy compares its source's error with io.EOF, so an end wrapped in another error would
		// be a failed copy of a whole artifact. It is an end, and exactReader says it as io.EOF.
		{"an end wrapped in an error", func(r io.Reader) io.Reader {
			return io.MultiReader(r, iotest.ErrReader(fmt.Errorf("the pipe closed: %w", io.EOF)))
		}, true},
		{"a read that fails after its bytes", func(r io.Reader) io.Reader {
			return io.MultiReader(r, iotest.ErrReader(errors.New("read failed")))
		}, false},
	}
	for _, src := range sources {
		// Reads are 4 bytes. An artifact of 19 ends inside one, as nearly every real artifact does,
		// so the last read asks for more than is left of it. Streams of 19, 18 and 7 bytes end
		// inside one as well, and one of 16 for an artifact of 20 ends on a full read, so its end
		// comes on a read of its own.
		for _, tc := range []struct{ size, stream int }{
			{20, 20}, {19, 19}, {8, 8}, {0, 0},
			{20, 19}, {19, 18}, {8, 7}, {20, 16}, {20, 0},
			{20, 21}, {19, 20}, {8, 9}, {8, 20}, {0, 1},
		} {
			e := &exactReader{r: src.wrap(bytes.NewReader(data[:tc.stream])), size: int64(tc.size)}
			var got bytes.Buffer
			_, err := io.CopyBuffer(struct{ io.Writer }{&got}, e, make([]byte, 4))
			what := fmt.Sprintf("%s: a %d-byte stream for a %d-byte artifact", src.name, tc.stream, tc.size)
			if got.Len() > tc.size || e.n != int64(got.Len()) {
				t.Errorf("%s passed on %d bytes and counted %d", what, got.Len(), e.n)
			}
			if tc.stream == tc.size && src.ends {
				if err != nil || !bytes.Equal(got.Bytes(), data[:tc.size]) {
					t.Errorf("%s gave %q, %v; want the artifact, and an end", what, got.Bytes(), err)
				}
				continue
			}
			// Not an end of any kind, so nothing that reads it, io.Copy or otherwise, can take it for
			// a whole stream.
			if err == nil || errors.Is(err, io.EOF) {
				t.Errorf("%s ended in %v, and the upload would be committed", what, err)
			}
		}
	}

	if err := iotest.TestReader(&exactReader{r: bytes.NewReader(data), size: int64(len(data))}, data); err != nil {
		t.Error(err)
	}
}

// A size below zero is no length, and the write is refused before anything is sent.
func TestGCSRefusesASizeThatIsNoLength(t *testing.T) {
	ctx := context.Background()
	b, _ := overHTTP(t, "gitdr-length")
	const key = "github.com/octo/hello/2026-06-13/hello.bundle"
	if _, err := b.PutImmutable(ctx, key, strings.NewReader("bundle-bytes"), -1, dest.Retention{}); err == nil {
		t.Fatal("a write of -1 bytes was taken")
	}
	if objs, err := b.List(ctx, key); err != nil || len(objs) != 0 {
		t.Fatalf("after the refusal the key holds %+v (%v), want nothing", objs, err)
	}
}
