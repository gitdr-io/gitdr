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
	"strconv"
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
	return verdictFromPolicy(attrs.RetentionPolicy), nil
}

// verdictFromPolicy decides from the bucket's retention policy as Cloud Storage reported it, and
// says it in the words Azure's check uses: "bucket retention policy Locked, 30 days". Up to v0.1.20
// it said "bucket retention 720h0m0s, locked=true".
//
//	immutable      a locked policy, with its period
//	not-immutable  no policy, or an unlocked one
//	unknown        a locked policy with no period. Cloud Storage keeps every period above 0
//	               seconds, so an answer without one has left the question out.
func verdictFromPolicy(rp *storage.RetentionPolicy) dest.WormStatus {
	switch {
	case rp == nil:
		// The native API answered and said there is no policy. An earned negative.
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Details: "no bucket retention policy",
		}
	case !rp.IsLocked:
		// An unlocked retention policy is also an earned negative, and the distinction matters:
		// the bucket has a retention period and the project owner can shorten or remove it, so
		// nothing here is enforced against the person most likely to be compromised.
		//
		// Its period is still reported. It is what holds the copies today, and it can only shorten
		// how long a skip relies on one.
		details := "bucket retention policy Unlocked"
		if rp.RetentionPeriod > 0 {
			details += ", " + periodWords(rp.RetentionPeriod)
		}
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Period:  rp.RetentionPeriod,
			Mode:    "RETENTION",
			Details: details + "; an unlocked policy can be shortened or removed",
		}
	case rp.RetentionPeriod <= 0:
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Mode:    "RETENTION",
			Details: "bucket retention policy Locked, with no retention period reported",
		}
	default:
		return dest.WormStatus{
			Verdict: dest.VerdictImmutable,
			Period:  rp.RetentionPeriod,
			Mode:    "RETENTION",
			Details: "bucket retention policy Locked, " + periodWords(rp.RetentionPeriod),
		}
	}
}

// periodWords says a retention period the way it was most likely set. Days, quarters of a day
// included, because Google counts a year as 365.25 days; below a day, or off a quarter, the largest
// unit that divides it.
func periodWords(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d == day:
		return "1 day"
	case d > day && d%(day/4) == 0:
		quarters := int64(d / (day / 4))
		return strconv.FormatInt(quarters/4, 10) + [...]string{"", ".25", ".5", ".75"}[quarters%4] + " days"
	case d%time.Hour == 0:
		return count(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return count(int64(d/time.Minute), "minute")
	default:
		return count(int64(d/time.Second), "second")
	}
}

func count(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.FormatInt(n, 10) + " " + unit + "s"
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
		written := attrs.Created
		if written.IsZero() {
			written = attrs.Updated
		}
		out = append(out, dest.Object{Key: attrs.Name, Size: attrs.Size, LastModified: written})
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
