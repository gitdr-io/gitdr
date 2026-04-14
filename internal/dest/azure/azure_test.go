package azure

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

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

// Env-gated integration test against Azurite (set AZURE_STORAGE_CONNECTION_STRING and
// pre-create the container). Skips locally; runs in CI.
func TestAzuriteLoop(t *testing.T) {
	cs := os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	if cs == "" {
		t.Skip("set AZURE_STORAGE_CONNECTION_STRING (Azurite) to run the Azure integration test")
	}
	ctx := context.Background()
	b, err := New(ctx, Options{Container: "gitdr-test", ConnectionString: cs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "github.com/octo/hello/2026-06-13/hello.bundle"
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
		t.Fatalf("get mismatch: %q", got)
	}
	objs, err := b.List(ctx, "github.com/octo/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("list returned nothing")
	}
}
