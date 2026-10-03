package s3_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// Objects past the multipart threshold. The tests set the threshold at 8 MiB and parts at 5 MiB,
// the smallest S3 takes, so an object of 23 MiB goes in five parts: 5, 5, 5, 5 and 3 MiB.

const mib = 1 << 20

func newPartsBackend(t *testing.T, srv *httptest.Server, conditional bool) *s3backend.Backend {
	t.Helper()
	b := newBackendWith(t, srv, s3backend.Options{MultipartThreshold: 8 * mib, PartSize: 5 * mib})
	if conditional {
		s3backend.ConditionalWrites(b)
	}
	return b
}

// payloadFile writes n bytes that differ from part to part, and opens them as the pipeline hands
// a bundle over.
func payloadFile(t *testing.T, n int) (*os.File, []byte) {
	t.Helper()
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*31 + i/mib)
	}
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, data
}

// noAbort fails the test if the store was ever asked to abort an upload: there is no delete of
// any kind in gitdr, an abort included.
func noAbort(t *testing.T, f *fakeStore) {
	t.Helper()
	if n := f.count("AbortMultipartUpload"); n != 0 {
		t.Errorf("%d AbortMultipartUpload requests; gitdr never aborts an upload", n)
	}
}

func modes() []struct {
	name        string
	conditional bool
} {
	return []struct {
		name        string
		conditional bool
	}{{"AWS", true}, {"a store without conditional writes", false}}
}

func TestPartSizesKeepAnObjectUnderNineThousandParts(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		size, smallest, partSize int64
		count                    int
	}{
		{23 * mib, 5 * mib, 5 * mib, 5},
		{6 * gib, 64 * mib, 64 * mib, 96},
		{9000 * 64 * mib, 64 * mib, 64 * mib, 9000},
		// One byte more and the parts grow by one byte rather than becoming 9,001.
		{9000*64*mib + 1, 64 * mib, 64*mib + 1, 9000},
		{50 * 1000 * 1000 * 1000, 64 * mib, 64 * mib, 746},
		{5 * 1024 * gib, 64 * mib, (5*1024*gib + 8999) / 9000, 9000},
	} {
		ps, n := s3backend.PartPlan(tc.size, tc.smallest)
		if ps != tc.partSize || n != tc.count {
			t.Errorf("%d bytes: parts of %d, %d of them; want %d, %d", tc.size, ps, n, tc.partSize, tc.count)
		}
		if int64(n-1)*ps >= tc.size || int64(n)*ps < tc.size || ps > s3backend.MaxPartSize {
			t.Errorf("%d bytes in %d parts of %d does not cover the object exactly", tc.size, n, ps)
		}
	}
}

// Past the threshold the object goes in parts: the key checked first, the lock on Create, a
// CRC32 in a header on every part, the full-object CRC32 on Complete, and on AWS If-None-Match
// and the object's size there too.
func TestAnObjectPastTheThresholdGoesInPartsCreateOnly(t *testing.T) {
	for _, m := range modes() {
		t.Run(m.name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = m.conditional
			b := newPartsBackend(t, srv, m.conditional)
			f, data := payloadFile(t, 23*mib)
			until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

			res, err := b.PutImmutable(context.Background(), "github.com/octo/big/2026-10-03/big.bundle", f, int64(len(data)),
				dest.Retention{Mode: dest.RetentionCompliance, Until: until})
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if res.Size != int64(len(data)) || !res.RetainUntil.Equal(until) || res.VersionID != "v1" {
				t.Errorf("result %+v", res)
			}
			o := fake.object("github.com/octo/big/2026-10-03/big.bundle")
			if o == nil || !bytes.Equal(o.data, data) || o.crc32 != crc32Base64(data) {
				t.Fatalf("the store does not hold the object whole")
			}
			for op, want := range map[string]int{"CreateMultipartUpload": 1, "UploadPart": 5, "CompleteMultipartUpload": 1, "PutObject": 0} {
				if got := fake.count(op); got != want {
					t.Errorf("%s: %d requests, want %d", op, got, want)
				}
			}
			noAbort(t, fake)

			up := fake.lastUpload()
			if up.lockMode != "COMPLIANCE" || up.retainUntil != "2030-01-02T03:04:05Z" || up.checksumType != "FULL_OBJECT" || up.algorithm != "CRC32" {
				t.Errorf("Create carried lock %q until %q, checksum %q %q", up.lockMode, up.retainUntil, up.checksumType, up.algorithm)
			}
			for _, p := range fake.partWrites() {
				if strings.Contains(p.encoding, "aws-chunked") || p.crc32Header == "" || p.length != int64(len(p.data)) {
					t.Errorf("a part went with encoding %q, checksum %q, length %d of %d", p.encoding, p.crc32Header, p.length, len(p.data))
				}
			}
			calls := fake.completed()
			if len(calls) != 1 || calls[0].crc32 != crc32Base64(data) || calls[0].checksumType != "FULL_OBJECT" {
				t.Fatalf("Complete carried %+v", calls)
			}
			wantINM, wantSize := "", ""
			if m.conditional {
				wantINM, wantSize = "*", strconv.Itoa(len(data))
			}
			if calls[0].ifNoneMatch != wantINM || calls[0].mpuObjectSize != wantSize {
				t.Errorf("Complete carried If-None-Match %q and object size %q, want %q and %q",
					calls[0].ifNoneMatch, calls[0].mpuObjectSize, wantINM, wantSize)
			}
			// The key is checked before the upload on every store, and again before Complete
			// where nothing else stops a second writer.
			wantHeads := 1
			if !m.conditional {
				wantHeads = 2
			}
			if got := fake.count("HeadObject"); got != wantHeads {
				t.Errorf("%d HeadObject requests, want %d", got, wantHeads)
			}
		})
	}
}

// A part over 16 MiB still goes as a plain body with its CRC32 in a header. Sent with a trailer,
// it would be one aws-chunked chunk, which MinIO refuses past 16 MiB.
func TestAPartOverSixteenMiBGoesWithItsChecksumInAHeader(t *testing.T) {
	fake, srv := newFakeStore(t)
	b := newBackendWith(t, srv, s3backend.Options{MultipartThreshold: 8 * mib, PartSize: 20 * mib})
	f, data := payloadFile(t, 45*mib)
	if _, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := fake.count("UploadPart"); got != 3 {
		t.Errorf("%d parts, want 3 of 20, 20 and 5 MiB", got)
	}
}

// A part that fails is sent again by itself, and the object is not.
func TestAPartThatFailsTwiceIsSentAgain(t *testing.T) {
	fake, srv := newFakeStore(t)
	fake.failPart[3] = 2
	b := newPartsBackend(t, srv, true)
	f, data := payloadFile(t, 23*mib)
	if _, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := fake.count("UploadPart"); got != 7 {
		t.Errorf("%d UploadPart requests, want 7: five parts, and part 3 twice more", got)
	}
	if o := fake.object("k"); o == nil || !bytes.Equal(o.data, data) {
		t.Error("the object is not whole")
	}
	noAbort(t, fake)
}

// A part that keeps failing fails the write. Nothing is completed and nothing is aborted: the
// upload is left to the bucket's lifecycle rule.
func TestAPartThatKeepsFailingFailsTheWriteWithoutAnAbort(t *testing.T) {
	fake, srv := newFakeStore(t)
	fake.failPart[2] = 100
	b := newPartsBackend(t, srv, true)
	f, data := payloadFile(t, 23*mib)
	_, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{})
	if err == nil || !strings.Contains(err.Error(), "part 2 of 5") || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("err = %v, want part 2 named and the upload left to the lifecycle rule", err)
	}
	if fake.count("CompleteMultipartUpload") != 0 || fake.object("k") != nil {
		t.Error("an upload with a missing part was completed")
	}
	if got := fake.count("UploadPart"); got > 5+2 {
		t.Errorf("%d UploadPart requests: a failing part is sent at most three times, and the rest stop", got)
	}
	noAbort(t, fake)
}

// Complete is sent once. One that landed and lost its answer is settled by asking the store, and
// the object there is this one.
func TestACompleteThatLandedAndLostItsAnswerIsThisWrite(t *testing.T) {
	for _, m := range modes() {
		t.Run(m.name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = m.conditional
			fake.dropCompleteAfterCommit = true
			b := newPartsBackend(t, srv, m.conditional)
			f, data := payloadFile(t, 23*mib)
			if _, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{}); err != nil {
				t.Fatalf("put: %v", err)
			}
			if got := fake.count("CompleteMultipartUpload"); got != 1 {
				t.Errorf("Complete sent %d times, want once", got)
			}
			if o := fake.object("k"); o == nil || o.versions != 1 {
				t.Errorf("the store holds %+v, want one object", o)
			}
			noAbort(t, fake)
		})
	}
}

// Another writer that finishes first keeps its object. On AWS, Complete meets If-None-Match and
// is refused, 412, and the store holds somebody else's checksum. Elsewhere the key is looked at
// again just before Complete, which is then never sent.
func TestAnObjectAnotherWriterFinishedFirstIsNotOverwritten(t *testing.T) {
	for _, m := range modes() {
		t.Run(m.name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = m.conditional
			b := newPartsBackend(t, srv, m.conditional)
			f, data := payloadFile(t, 23*mib)
			other := func() { fake.seed("k", []byte("another writer's object")) }
			if m.conditional {
				fake.beforeComplete = other
			} else {
				// The second look, after every part has landed.
				fake.beforeHead = map[int]func(){2: other}
			}
			_, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{})
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if o := fake.object("k"); o == nil || string(o.data) != "another writer's object" || o.versions != 1 {
				t.Errorf("the other writer's object changed: %+v", o)
			}
			if !m.conditional && fake.count("CompleteMultipartUpload") != 0 {
				t.Error("Complete was sent although the key was taken")
			}
			noAbort(t, fake)
		})
	}
}

// A 409 at Complete, with nothing at the key afterwards, starts the upload once more, as AWS
// documents for a conflict.
func TestAConflictAtCompleteUploadsOnceMore(t *testing.T) {
	fake, srv := newFakeStore(t)
	fake.conditional = true
	fake.completeStatus = http.StatusConflict
	b := newPartsBackend(t, srv, true)
	f, data := payloadFile(t, 23*mib)
	if _, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	for op, want := range map[string]int{"CreateMultipartUpload": 2, "UploadPart": 10, "CompleteMultipartUpload": 2} {
		if got := fake.count(op); got != want {
			t.Errorf("%s: %d requests, want %d", op, got, want)
		}
	}
	if o := fake.object("k"); o == nil || !bytes.Equal(o.data, data) {
		t.Error("the object is not whole")
	}
	noAbort(t, fake)
}

// A key that already holds an object is refused before any part is sent, on every store, so
// gigabytes are never sent only to be refused.
func TestAKeyThatExistsIsRefusedBeforeAnyPart(t *testing.T) {
	for _, m := range modes() {
		t.Run(m.name, func(t *testing.T) {
			fake, srv := newFakeStore(t)
			fake.conditional = m.conditional
			fake.seed("k", []byte("an object from an earlier run"))
			b := newPartsBackend(t, srv, m.conditional)
			f, data := payloadFile(t, 23*mib)
			_, err := b.PutImmutable(context.Background(), "k", f, int64(len(data)), dest.Retention{})
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if fake.count("CreateMultipartUpload") != 0 || fake.count("UploadPart") != 0 {
				t.Errorf("the upload started anyway: %d Create, %d parts", fake.count("CreateMultipartUpload"), fake.count("UploadPart"))
			}
		})
	}
}

// A store that refuses a full-object checksum at Create gets a composite one, for that upload and
// every later one, and a lost answer to such an upload is still settled as this write.
func TestAStoreThatRefusesAFullObjectChecksumGetsAComposite(t *testing.T) {
	fake, srv := newFakeStore(t)
	fake.refuseFullObject = true
	b := newPartsBackend(t, srv, false)

	f, data := payloadFile(t, 23*mib)
	if _, err := b.PutImmutable(context.Background(), "first", f, int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if got := fake.count("CreateMultipartUpload"); got != 2 {
		t.Errorf("%d Create requests for the first upload, want 2: refused, then composite", got)
	}
	if up := fake.lastUpload(); up.checksumType != "" || up.algorithm != "CRC32" {
		t.Errorf("the composite upload asked for %q %q", up.checksumType, up.algorithm)
	}
	if calls := fake.completed(); calls[0].crc32 != "" || calls[0].checksumType != "" {
		t.Errorf("a composite Complete carried a full-object checksum: %+v", calls[0])
	}

	fake.dropCompleteAfterCommit = true
	g, more := payloadFile(t, 23*mib+1)
	if _, err := b.PutImmutable(context.Background(), "second", g, int64(len(more)), dest.Retention{}); err != nil {
		t.Fatalf("second put: %v", err)
	}
	if got := fake.count("CreateMultipartUpload"); got != 3 {
		t.Errorf("%d Create requests in all, want 3: the second upload asks for a composite at once", got)
	}
	noAbort(t, fake)
}
