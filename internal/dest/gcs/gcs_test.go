package gcs

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

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
