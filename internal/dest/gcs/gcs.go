// Package gcs implements the create-only Destination for Google Cloud Storage. WORM is
// a locked bucket retention policy (Bucket Lock); writes are create-only and inherit
// it. Auth uses Application Default Credentials. There is no delete path.
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"gitdr.io/gitdr/internal/dest"
)

// Options configures the GCS backend. Credentials come from ADC (not set here).
type Options struct {
	Bucket   string
	Endpoint string // empty = real GCS; set for an emulator, e.g. http://host:4443/storage/v1/
}

// Backend is a create-only GCS Destination.
type Backend struct {
	client *storage.Client
	bucket *storage.BucketHandle
	name   string
	logger *slog.Logger
}

var _ dest.Destination = (*Backend)(nil)

// New builds a GCS backend using ADC (or no auth when pointed at an emulator endpoint).
func New(ctx context.Context, opts Options, logger *slog.Logger) (*Backend, error) {
	if strings.TrimSpace(opts.Bucket) == "" {
		return nil, errors.New("gcs: bucket is required")
	}
	var clientOpts []option.ClientOption
	if opts.Endpoint != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(opts.Endpoint), option.WithoutAuthentication())
	}
	client, err := storage.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("gcs: new client: %w", err)
	}
	return newBackend(client, opts.Bucket, logger), nil
}

func newBackend(client *storage.Client, bucket string, logger *slog.Logger) *Backend {
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{client: client, bucket: client.Bucket(bucket), name: bucket, logger: logger}
}

// VerifyWorm requires a locked bucket retention policy. An unlocked or absent policy is
// reported as not-locked so the operator can override.
func (b *Backend) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	attrs, err := b.bucket.Attrs(ctx)
	if err != nil {
		return dest.WormStatus{}, fmt.Errorf("gcs: bucket attrs: %w", err)
	}
	rp := attrs.RetentionPolicy
	if rp == nil {
		// The native API answered and said there is no policy. An earned negative.
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Details: "no bucket retention policy",
		}, nil
	}
	// An unlocked retention policy is also an earned negative, and the distinction matters:
	// the bucket has a retention period and the project owner can shorten or remove it, so
	// nothing here is enforced against the person most likely to be compromised.
	verdict := dest.VerdictNotImmutable
	if rp.IsLocked {
		verdict = dest.VerdictImmutable
	}
	return dest.WormStatus{
		Verdict: verdict,
		Mode:    "RETENTION",
		Details: fmt.Sprintf("bucket retention %s, locked=%v", rp.RetentionPeriod, rp.IsLocked),
	}, nil
}

// PutImmutable creates key. Create-only via the DoesNotExist precondition; immutability
// is enforced by the bucket's locked retention policy. Never overwrites, never deletes.
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, _ int64, _ dest.Retention) (dest.PutResult, error) {
	obj := b.bucket.Object(key).If(storage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return dest.PutResult{}, fmt.Errorf("gcs: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return dest.PutResult{}, fmt.Errorf("gcs: put %q: %w", key, err)
	}
	a := w.Attrs()
	return dest.PutResult{Key: key, ETag: a.Etag, Size: a.Size, RetainUntil: a.RetentionExpirationTime}, nil
}

// List returns objects under prefix (read-only).
func (b *Backend) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	var out []dest.Object
	it := b.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list %q: %w", prefix, err)
		}
		out = append(out, dest.Object{Key: attrs.Name, Size: attrs.Size})
	}
	return out, nil
}

// Get opens key for reading (read-only). Caller closes the reader.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.bucket.Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: get %q: %w", key, err)
	}
	return rc, nil
}

// ObserveRetention asks Cloud Storage what retention is on an object gitdr wrote.
//
// The native backend is the one case where the write itself already answers this - PutImmutable
// records `RetentionExpirationTime` off the returned attributes - so this re-reads the object
// rather than trusting a value carried from the write, which is the whole point of the check.
//
// `RetentionExpirationTime` is the bucket retention policy's expiry for this object. A zero value
// from a successful read is the store answering "nothing holds this object", which is an earned
// negative. A failed read says nothing and must not be read as one.
func (b *Backend) ObserveRetention(ctx context.Context, key string) (dest.RetentionObservation, time.Time, error) {
	attrs, err := b.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return dest.RetentionNotChecked, time.Time{}, fmt.Errorf("gcs: attrs %q: %w", key, err)
	}
	if attrs.RetentionExpirationTime.IsZero() {
		return dest.RetentionAbsent, time.Time{}, nil
	}
	return dest.RetentionPresent, attrs.RetentionExpirationTime.UTC(), nil
}
