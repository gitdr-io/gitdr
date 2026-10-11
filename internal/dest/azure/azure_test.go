// Integration tests for the Azure backend, run against Azurite.
//
// What the emulator proves: the round trip, that writes are create-only, and that the WORM
// gate reports honestly when there is no immutability. That is the behaviour this package
// is responsible for.
//
// What it does not prove: wire compatibility with current Azure. Azurite is behind the SDK
// -- 3.36.0 rejects the API version the SDK sends and has to be run with
// --skipApiVersionCheck -- and it has no version-level immutability at all, so the enabled
// branch of VerifyWorm can only be exercised against a real storage account.
package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"gitdr.io/gitdr/internal/dest"
)

func TestNewValidation(t *testing.T) {
	if _, err := New(context.Background(), Options{}, nil); err == nil {
		t.Error("expected error for missing container")
	}
	if _, err := New(context.Background(), Options{Container: "c"}, nil); err == nil {
		t.Error("expected error for missing account/endpoint")
	}
}

// The account becomes the blob endpoint's host, and the credential goes to that host, so an account
// that is not a storage account name is refused before anything is built from it. Up to v0.1.20,
// "x@example.org/" made https://x@example.org/.blob.core.windows.net/, whose host is example.org.
func TestNewRefusesAnAccountThatIsNotAStorageAccountName(t *testing.T) {
	for _, account := range []string{"x@example.org/", "acme.evil.com", "Acme", "ab", strings.Repeat("a", 25), "acme-gitdr"} {
		b, err := New(context.Background(), Options{Container: "c", Account: account}, nil)
		if err == nil {
			host := "?"
			if u, perr := url.Parse(b.client.URL()); perr == nil {
				host = u.Hostname()
			}
			t.Errorf("account %q accepted, and the blob client talks to %s", account, host)
			continue
		}
		if strings.ContainsAny(account, ".@") && strings.Contains(err.Error(), account) {
			t.Errorf("the refusal quotes the value: %v", err)
		}
	}
}

// devConnectionString is Azurite's well-known development account. Microsoft publishes
// this exact key in the emulator's own documentation; it authenticates nothing but a local
// emulator and is a constant, not a credential.
const devConnectionString = "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;" +
	"AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;" +
	"BlobEndpoint=%s/devstoreaccount1;"

// connectionString locates an Azurite to test against, or skips.
//
// Two ways in: AZURITE_BLOB_ENDPOINT (just a host, the usual case -- CI sets it to the
// service alias, and locally it is http://127.0.0.1:10000), or a full
// AZURE_STORAGE_CONNECTION_STRING to point somewhere else entirely.
//
// In CI it fails instead of skipping. A skipped Go test prints nothing and the package
// still reports ok, so a missing emulator would read exactly like a passing backend --
// which is how this backend went unproven for its whole life.
func connectionString(t *testing.T) string {
	t.Helper()
	if cs := os.Getenv("AZURE_STORAGE_CONNECTION_STRING"); cs != "" {
		return cs
	}
	if endpoint := os.Getenv("AZURITE_BLOB_ENDPOINT"); endpoint != "" {
		return fmt.Sprintf(devConnectionString, strings.TrimSuffix(endpoint, "/"))
	}
	if os.Getenv("CI") != "" {
		t.Fatal("no Azurite endpoint set; in CI the Azure tests must run, not skip")
	}
	t.Skip("set AZURITE_BLOB_ENDPOINT (e.g. http://127.0.0.1:10000) to run the Azure integration tests")
	return ""
}

// container gives each test its own container, created here rather than by hand. The
// previous version of this file required the operator to pre-create it, which is the same
// as requiring that nobody ever runs the test.
func container(t *testing.T, cs, name string) *Backend {
	t.Helper()
	ctx := context.Background()
	raw, err := azblob.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := raw.CreateContainer(ctx, name, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatalf("create container: %v", err)
	}
	b, err := New(ctx, Options{Container: name, ConnectionString: cs}, nil)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return b
}

// uniqueKey gives every run its own key.
//
// The destination has no delete path by design, so a key written by one run is there
// forever. Reusing a fixed key means the first run passes and every run after it fails on
// its own leftovers -- which is exactly what happened the first time these tests were run
// twice.
func uniqueKey(t *testing.T, name string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "github.com/octo/hello/2026-06-13/" + hex.EncodeToString(b[:]) + "-" + name
}

func TestAzuriteRoundTrip(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	b := container(t, cs, "gitdr-roundtrip")

	key := uniqueKey(t, "hello.bundle")
	data := []byte("bundle-bytes")
	res, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// The manifest records this as the artifact's size. It was 0 for every Azure blob.
	if res.Size != int64(len(data)) {
		t.Errorf("put recorded %d bytes, want %d", res.Size, len(data))
	}

	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("get mismatch: got %q want %q", got, data)
	}

	objs, err := b.List(ctx, "github.com/octo/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("list returned nothing")
	}
	var found *dest.Object
	for i := range objs {
		if objs[i].Key == key {
			found = &objs[i]
		}
	}
	if found == nil {
		t.Fatalf("list did not return %q", key)
	}
	if found.Size != int64(len(data)) {
		t.Errorf("list size = %d, want %d", found.Size, len(data))
	}
	// When the blob was written, which a same-day rerun holds against the date in its key.
	if got := found.LastModified; got.IsZero() || time.Since(got).Abs() > 10*time.Minute {
		t.Errorf("list says %s was written at %v, want about now", key, got)
	}
}

// A blob too large for 50,000 blocks of 1 MiB, about 48.8 GiB, used to fail on its last request:
// Put Block List takes at most 50,000 blocks, and the upload only learnt that after sending all of
// them. The block size is now chosen from the blob's size.
//
// Sending 48.8 GiB is not a test anyone runs, so this one lowers the limit to 4 blocks and writes a
// little over 5 MiB, which 1 MiB blocks would split into 6. What has to hold is what holds at full
// size: the blob is committed in no more blocks than the limit, in order, at its own size.
func TestAzuriteWritesABlobThatNeedsLargerBlocks(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	const name = "gitdr-blocks"
	b := container(t, cs, name)
	b.maxBlocks = 4

	data := make([]byte, 5<<20+7)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	key := uniqueKey(t, "large.bundle")
	res, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.Size != int64(len(data)) {
		t.Errorf("put recorded %d bytes, want %d", res.Size, len(data))
	}

	raw, err := azblob.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := raw.ServiceClient().NewContainerClient(name).NewBlockBlobClient(key).GetBlockList(ctx, blockblob.BlockListTypeCommitted, nil)
	if err != nil {
		t.Fatalf("block list: %v", err)
	}
	if n := len(blocks.CommittedBlocks); n == 0 || n > b.maxBlocks {
		t.Errorf("the blob was committed in %d blocks, and the limit is %d", n, b.maxBlocks)
	}

	objs, err := b.List(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Size != int64(len(data)) {
		t.Errorf("list = %+v, want one blob of %d bytes", objs, len(data))
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
	if !bytes.Equal(got, data) {
		t.Errorf("the blob read back is not what was written: %d bytes, want %d", len(got), len(data))
	}
}

// A stream that ends before the artifact's size, or holds more than it, is not the artifact that
// was checksummed, and it must not become the blob at the key. Up to v0.1.22 the upload committed
// whatever the stream held before anything compared its size: in a locked container that is a blob
// nobody can remove, at a key no copy can be written to again that day. The upload now stops before
// its commit, and the artifact itself, written next, finds the key free.
//
// Below a block the blob goes in one Put Blob, and from a block up in Put Block List. Each is a
// commit, so each is tried.
func TestAzuriteCommitsNothingFromAStreamOfAnotherSize(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	b := container(t, cs, "gitdr-length")

	const mib = 1 << 20
	for _, tc := range []struct {
		name         string
		size, stream int
		wrappedEnd   bool // the stream's end comes as an error wrapping io.EOF
	}{
		{"one byte short, in one request", 100, 99, false},
		{"one byte over, in one request", 100, 101, false},
		{"nothing, for an artifact that has bytes", 10, 0, false},
		{"bytes, for an artifact that has none", 0, 10, false},
		{"a block short, in blocks", 3*mib + 5, 2 * mib, false},
		{"a block over, in blocks", 2 * mib, 3 * mib, false},
		{"a block short, its end wrapped in an error", 3*mib + 5, 2 * mib, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, max(tc.size, tc.stream))
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			var stream io.Reader = bytes.NewReader(data[:tc.stream])
			if tc.wrappedEnd {
				stream = io.MultiReader(stream, iotest.ErrReader(fmt.Errorf("the pipe closed: %w", io.EOF)))
			}
			key := uniqueKey(t, "length.bundle")
			if _, err := b.PutImmutable(ctx, key, stream, int64(tc.size), dest.Retention{}); err == nil {
				t.Fatalf("a stream of %d bytes was stored as the %d-byte artifact", tc.stream, tc.size)
			}
			if objs, err := b.List(ctx, key); err != nil || len(objs) != 0 {
				t.Fatalf("after the refusal the key holds %+v (%v), want nothing", objs, err)
			}

			res, err := b.PutImmutable(ctx, key, bytes.NewReader(data[:tc.size]), int64(tc.size), dest.Retention{})
			if err != nil {
				t.Fatalf("the artifact itself, written next, was refused: %v", err)
			}
			if res.Size != int64(tc.size) {
				t.Errorf("put recorded %d bytes, want %d", res.Size, tc.size)
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
				t.Errorf("the blob read back is %d bytes and not the %d-byte artifact", len(got), tc.size)
			}
		})
	}
}

// The block size at Azure's real limits: 50,000 blocks, each at most 4000 MiB.
func TestBlockSizeKeepsEveryBlobWithinTheBlockLimit(t *testing.T) {
	const mib = 1 << 20
	limit := int64(blockblob.MaxBlocks)
	largest := limit * blockblob.MaxStageBlockBytes // about 190.7 TiB

	for _, tc := range []struct {
		name string
		size int64
		want int64
	}{
		{"nothing", 0, mib},
		{"one byte", 1, mib},
		{"exactly what 50,000 blocks of 1 MiB hold, 48.8 GiB", limit * mib, mib},
		{"one byte more", limit*mib + 1, 2 * mib},
		{"100 GiB", 100 << 30, 3 * mib},
		{"the largest block blob", largest, blockblob.MaxStageBlockBytes},
	} {
		got, err := blockSizeFor(tc.size, blockblob.MaxBlocks)
		if err != nil || got != tc.want {
			t.Errorf("%s: block size %d (%v), want %d", tc.name, got, err, tc.want)
		}
	}
	for _, size := range []int64{-1, largest + 1, math.MaxInt64} {
		if got, err := blockSizeFor(size, blockblob.MaxBlocks); err == nil {
			t.Errorf("a blob of %d bytes was given %d-byte blocks; it cannot be a block blob", size, got)
		}
	}

	// Whatever the size: within the limit, whole MiB, and the smallest block that fits.
	sizes := []int64{2, mib - 1, mib, mib + 1, limit*mib - 1, limit*2*mib - 1, limit * 2 * mib, limit*2*mib + 1, 1 << 40, largest - 1}
	for _, maxBlocks := range []int{blockblob.MaxBlocks, 4, 1} {
		for _, size := range sizes {
			bs, err := blockSizeFor(size, maxBlocks)
			if err != nil {
				if size <= int64(maxBlocks)*blockblob.MaxStageBlockBytes {
					t.Errorf("%d bytes in %d blocks: %v", size, maxBlocks, err)
				}
				continue
			}
			blocks := (size + bs - 1) / bs
			if blocks > int64(maxBlocks) || bs%mib != 0 || bs < mib || bs > blockblob.MaxStageBlockBytes {
				t.Errorf("%d bytes in %d blocks: %d-byte blocks make %d", size, maxBlocks, bs, blocks)
			}
			if smaller := bs - mib; smaller >= mib && (size+smaller-1)/smaller <= int64(maxBlocks) {
				t.Errorf("%d bytes in %d blocks: %d-byte blocks, and %d-byte ones fit", size, maxBlocks, bs, smaller)
			}
		}
	}
}

// The upload commits when its source says io.EOF, so exactReader may say it only where the stream
// ends at the artifact's size, however the stream hands its bytes over, and never passes on a byte
// past size. Read here as the SDK reads: into whole blocks, each until it is full or a read fails,
// where an error that comes with a full block is dropped and has to come again on the next read.
func TestExactReaderEndsOnlyWhereTheArtifactDoes(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuv")
	readBlock := func(r io.Reader, block []byte) (n int, err error) {
		for n < len(block) && err == nil {
			var k int
			k, err = r.Read(block[n:])
			n += k
		}
		if n == len(block) {
			err = nil
		}
		return n, err
	}
	sources := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"as it is", func(r io.Reader) io.Reader { return r }},
		{"a byte at a time", iotest.OneByteReader},
		{"half of each read", iotest.HalfReader},
		{"its end with its last bytes", iotest.DataErrReader},
		// A read that ends in an error wrapping io.EOF, which the upload commits on as well.
		{"an end wrapped in an error", func(r io.Reader) io.Reader {
			return io.MultiReader(r, iotest.ErrReader(fmt.Errorf("the pipe closed: %w", io.EOF)))
		}},
	}
	for _, src := range sources {
		// Blocks are 4 bytes. An artifact of 19 ends inside one, as nearly every real artifact
		// does, so the last read asks for more than is left of it. A 16-byte stream for 20 ends on
		// a full block, so an end that comes with its last bytes is dropped there and has to be
		// said again.
		for _, tc := range []struct{ size, stream int }{
			{20, 20}, {19, 19}, {8, 8}, {0, 0},
			{20, 19}, {19, 18}, {8, 7}, {20, 16}, {20, 0},
			{20, 21}, {19, 20}, {8, 9}, {8, 20}, {0, 1},
		} {
			e := &exactReader{r: src.wrap(bytes.NewReader(data[:tc.stream])), size: int64(tc.size)}
			var got []byte
			var err error
			for err == nil {
				block := make([]byte, 4)
				var n int
				n, err = readBlock(e, block)
				got = append(got, block[:n]...)
			}
			what := fmt.Sprintf("%s: a %d-byte stream for a %d-byte artifact", src.name, tc.stream, tc.size)
			if len(got) > tc.size || e.n != int64(len(got)) {
				t.Errorf("%s passed on %d bytes and counted %d", what, len(got), e.n)
			}
			ended := errors.Is(err, io.EOF)
			if tc.stream == tc.size && (!ended || !bytes.Equal(got, data[:tc.size])) {
				t.Errorf("%s gave %q, %v; want the artifact and io.EOF", what, got, err)
			}
			if tc.stream != tc.size && ended {
				t.Errorf("%s ended in %v, which the upload would commit", what, err)
			}
		}
	}

	if err := iotest.TestReader(&exactReader{r: bytes.NewReader(data), size: int64(len(data))}, data); err != nil {
		t.Error(err)
	}
}

// TestAzuriteCreateOnly is the one that matters. Invariant: a backup destination never
// overwrites. The backend enforces it with If-None-Match: *, and until now nothing
// anywhere proved that condition was actually being sent — an upload that silently
// replaced the previous day's bundle would have passed every test in this package.
func TestAzuriteCreateOnly(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	b := container(t, cs, "gitdr-createonly")

	key := uniqueKey(t, "immutable.bundle")
	first := []byte("the original bundle")
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(first), int64(len(first)), dest.Retention{}); err != nil {
		t.Fatalf("first put: %v", err)
	}

	second := []byte("an attacker's replacement")
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(second), int64(len(second)), dest.Retention{}); err == nil {
		t.Fatal("second put to the same key succeeded; the destination is not create-only")
	}

	// The refusal is only half of it: what is stored must still be the original.
	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get after refused overwrite: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, first) {
		t.Fatalf("stored bytes changed after a refused overwrite: got %q want %q", got, first)
	}
}

// A blob of a block or more is staged and then committed with Put Block List, the request every
// blob past 48.8 GiB ends in, and If-None-Match has to ride on that request as well. The test
// above writes a blob small enough for one Put Blob and proves only that one. Staging carries no
// condition, so the commit is the only place the refusal can happen.
func TestAzuriteCreateOnlyInBlocks(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	const name = "gitdr-createonly"
	b := container(t, cs, name)

	const mib = 1 << 20
	first, second := make([]byte, 2*mib+1), make([]byte, 3*mib)
	for _, data := range [][]byte{first, second} {
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
	}
	key := uniqueKey(t, "immutable.bundle")
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(first), int64(len(first)), dest.Retention{}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	// In blocks, or this proves the Put Blob path a second time.
	raw, err := azblob.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := raw.ServiceClient().NewContainerClient(name).NewBlockBlobClient(key).GetBlockList(ctx, blockblob.BlockListTypeCommitted, nil)
	if err != nil {
		t.Fatalf("block list: %v", err)
	}
	if n := len(blocks.CommittedBlocks); n < 2 {
		t.Fatalf("the first blob was committed in %d blocks, and this test is about Put Block List", n)
	}

	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(second), int64(len(second)), dest.Retention{}); err == nil {
		t.Fatal("second put to the same key succeeded; the destination is not create-only")
	}
	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get after refused overwrite: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("stored bytes changed after a refused overwrite: %d bytes, and the original is %d", len(got), len(first))
	}
}

// ListPage is one List Blobs request: the blobs on the first page, and whether Azure says there
// are more. doctor finds an object to read a retention from this way, without walking the
// container.
func TestAzuriteListPage(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	b := container(t, cs, "gitdr-listpage")

	// Under a prefix of this run's own: the container keeps every earlier run's blobs.
	prefix := strings.TrimSuffix(uniqueKey(t, ""), "-")
	for _, name := range []string{"/a.bundle", "/b.bundle"} {
		data := []byte(name)
		if _, err := b.PutImmutable(ctx, prefix+name, bytes.NewReader(data), int64(len(data)), dest.Retention{}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	objs, more, err := b.ListPage(ctx, prefix+"/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != prefix+"/a.bundle" || objs[0].Size != int64(len("/a.bundle")) {
		t.Errorf("first page = %+v, want %s/a.bundle with its size", objs, prefix)
	}
	if !more {
		t.Error("more = false with a blob after the page")
	}

	objs, more, err = b.ListPage(ctx, prefix+"/nothing/", 1)
	if err != nil || len(objs) != 0 || more {
		t.Errorf("an empty prefix gave %+v, more %v, %v; want nothing and no more", objs, more, err)
	}
}

// A plain container has no version-level immutability, so the gate must say so. Reporting
// WORM where there is none is worse than reporting none at all.
func TestAzuriteVerifyWormReportsNone(t *testing.T) {
	cs := connectionString(t)
	ctx := context.Background()
	b := container(t, cs, "gitdr-worm")

	st, err := b.VerifyWorm(ctx)
	if err != nil {
		t.Fatalf("verify worm: %v", err)
	}
	if st.Verdict.Immutable() {
		t.Error("VerifyWorm reported immutability on a plain container")
	}
}
