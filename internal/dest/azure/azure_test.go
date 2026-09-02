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
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

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
	if _, err := b.PutImmutable(ctx, key, bytes.NewReader(data), int64(len(data)), dest.Retention{}); err != nil {
		t.Fatalf("put: %v", err)
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
