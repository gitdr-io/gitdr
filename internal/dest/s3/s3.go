// Package s3 implements the Destination interface for S3 and S3-compatible stores
// (AWS, MinIO, Wasabi, Backblaze B2, IDrive). It is create-only: there is no delete
// or overwrite path. Credentials come from the AWS SDK default chain, static keys
// (S3-compatible) are supplied via AWS_* env, never hand-resolved here.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/dest"
)

// Options configures the S3 backend. Credentials are intentionally absent: they are
// resolved by the SDK default chain (env, IRSA/Pod Identity, instance profile, SSO).
type Options struct {
	Bucket       string
	Region       string // defaults to us-east-1 (also fine for MinIO)
	Endpoint     string // empty = AWS; set for MinIO/Wasabi/B2/IDrive
	UsePathStyle bool   // true for MinIO and most S3-compatible stores
}

// Backend is a create-only S3 Destination.
type Backend struct {
	client *awss3.Client
	bucket string
	logger *slog.Logger
	// conditionalWrite uses If-None-Match for atomic create-only (real AWS). Many
	// S3-compatible stores return 501 for it, so for custom endpoints we fall back to a
	// HeadObject pre-check instead.
	conditionalWrite bool
}

var _ dest.Destination = (*Backend)(nil)

// New builds an S3 backend using the default credential chain.
func New(ctx context.Context, opts Options, logger *slog.Logger) (*Backend, error) {
	if strings.TrimSpace(opts.Bucket) == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if strings.HasPrefix(opts.Endpoint, "http://") && !isLoopback(opts.Endpoint) {
		logger.Warn("s3 endpoint uses plaintext http; traffic is unencrypted", "endpoint", opts.Endpoint)
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.UsePathStyle
	})
	return &Backend{
		client:           client,
		bucket:           opts.Bucket,
		logger:           logger,
		conditionalWrite: opts.Endpoint == "", // If-None-Match on AWS; HeadObject pre-check elsewhere
	}, nil
}

// VerifyWorm probes bucket Object Lock. A missing lock config is reported as
// not-locked (so the operator can override); other failures are returned as errors.
func (b *Backend) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	out, err := b.client.GetObjectLockConfiguration(ctx, &awss3.GetObjectLockConfigurationInput{
		Bucket: aws.String(b.bucket),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
			return dest.WormStatus{Details: "bucket has no Object Lock configuration"}, nil
		}
		return dest.WormStatus{}, fmt.Errorf("s3: get object lock config: %w", err)
	}

	cfg := out.ObjectLockConfiguration
	enabled := cfg != nil && cfg.ObjectLockEnabled == s3types.ObjectLockEnabledEnabled
	// Object Lock can only be enabled at bucket creation and cannot be turned off, so
	// "enabled" means any retention we apply per object is durably enforced.
	st := dest.WormStatus{Enabled: enabled, Locked: enabled, Details: "Object Lock enabled"}
	if !enabled {
		st.Details = "Object Lock not enabled"
		return st, nil
	}
	if cfg.Rule != nil && cfg.Rule.DefaultRetention != nil {
		st.Mode = string(cfg.Rule.DefaultRetention.Mode)
		st.Details = fmt.Sprintf("Object Lock enabled; default retention %s", st.Mode)
	}
	return st, nil
}

// PutImmutable creates key, create-only, never overwriting or deleting. Object Lock
// retention is applied only when ret carries a retain-until (i.e. the bucket is
// immutable); on the non-WORM adoption path we write plainly, because sending lock
// headers to a bucket without Object Lock is a 400. An explicit CRC32 checksum is always
// sent: AWS and some S3-compatible stores (Backblaze B2) require Content-MD5 or an
// x-amz-checksum header on Object Lock PutObject requests.
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	in := &awss3.PutObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		Body:              r,
		ContentLength:     aws.Int64(size),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
	}
	if !ret.Until.IsZero() {
		in.ObjectLockMode = s3types.ObjectLockModeCompliance
		if ret.Mode == dest.RetentionGovernance {
			in.ObjectLockMode = s3types.ObjectLockModeGovernance
		}
		in.ObjectLockRetainUntilDate = aws.Time(ret.Until.UTC())
	}

	if b.conditionalWrite {
		in.IfNoneMatch = aws.String("*") // atomic create-only (real AWS)
	} else if exists, err := b.objectExists(ctx, key); err != nil {
		return dest.PutResult{}, err
	} else if exists {
		// Portable create-only for S3-compatible stores lacking conditional writes.
		return dest.PutResult{}, fmt.Errorf("s3: refusing to overwrite existing object %q", key)
	}

	out, err := b.client.PutObject(ctx, in)
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("s3: put %q: %w", key, err)
	}
	res := dest.PutResult{Key: key, Size: size}
	if !ret.Until.IsZero() {
		res.RetainUntil = ret.Until.UTC()
	}
	if out.ETag != nil {
		res.ETag = strings.Trim(*out.ETag, `"`)
	}
	if out.VersionId != nil {
		res.VersionID = *out.VersionId
	}
	return res, nil
}

// objectExists reports whether key is already present. A NotFound (missing key) is the
// expected create-only case and returns false; other errors propagate.
func (b *Backend) objectExists(ctx context.Context, key string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if _, ok := errors.AsType[*s3types.NotFound](err); ok {
		return false, nil
	}
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return false, nil
		}
	}
	return false, fmt.Errorf("s3: head %q: %w", key, err)
}

// List returns objects under prefix (read-only).
func (b *Backend) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	var objs []dest.Object
	p := awss3.NewListObjectsV2Paginator(b.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			objs = append(objs, dest.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)})
		}
	}
	return objs, nil
}

// Get opens key for reading (read-only). Caller closes the reader.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

func isLoopback(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
