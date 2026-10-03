package s3_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// What reaches the store when gitdr writes an object, over TLS, the way it does in production.

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
			fake, srv := newFakeStore(t)
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
	fake, srv := newFakeStore(t)
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
	fake, srv := newFakeStore(t)
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

// A write that landed and lost its answer is not refused by its own retry.
//
// The store writes the object and closes the connection without answering. The SDK sends the
// write again, and on AWS If-None-Match refuses it, 412, for the object the first attempt wrote.
// That failed the repository. The object at the key is this one: same size, same CRC32.
func TestAPutThatLandedIsNotRefusedByItsOwnRetry(t *testing.T) {
	fake, srv := newFakeStore(t)
	fake.conditional = true
	fake.dropAfterCommit = 1
	b := newBackend(t, srv)
	s3backend.ConditionalWrites(b)

	data := []byte("the bundle of one repository")
	res, err := b.PutImmutable(context.Background(), "k", bytes.NewReader(data), int64(len(data)), dest.Retention{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	o := fake.object("k")
	if o == nil || o.versions != 1 || !bytes.Equal(o.data, data) {
		t.Fatalf("the store holds %+v, want the one object", o)
	}
	if res.Size != int64(len(data)) || res.Key != "k" {
		t.Errorf("result %+v", res)
	}
	t.Logf("%d PutObject requests, %d HeadObject", fake.count("PutObject"), fake.count("HeadObject"))
}

// A different object at the key is still refused, on AWS by If-None-Match and elsewhere by the
// check before the write, and the object there is left as it was.
func TestADifferentObjectAtTheKeyIsStillRefused(t *testing.T) {
	for _, conditional := range []bool{true, false} {
		name := "a store without conditional writes"
		if conditional {
			name = "AWS"
		}
		t.Run(name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = conditional
			b := newBackend(t, srv)
			if conditional {
				s3backend.ConditionalWrites(b)
			}
			fake.seed("k", []byte("somebody else's object"))

			_, err := b.PutImmutable(context.Background(), "k", bytes.NewReader([]byte("ours")), 4, dest.Retention{})
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if o := fake.object("k"); o.versions != 1 || string(o.data) != "somebody else's object" {
				t.Errorf("the object at the key changed: %+v", o)
			}
		})
	}
}
