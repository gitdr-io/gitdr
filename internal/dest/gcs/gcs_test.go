package gcs

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"

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

	st, err := b.VerifyWorm(ctx)
	if err != nil {
		t.Fatalf("verifyworm: %v", err)
	}
	if st.Locked {
		t.Error("emulator should not report a locked retention policy")
	}
}
