// Package s3 implements the Destination interface for S3 and S3-compatible stores
// (AWS, MinIO, Wasabi, Backblaze B2, IDrive). It is create-only: there is no delete
// or overwrite path. Credentials come from the AWS SDK default chain, static keys
// (S3-compatible) are supplied via AWS_* env, never hand-resolved here.
package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

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
	// MultipartThreshold is the size above which an object is written in parts; 0 is 4 GiB.
	// PartSize is the smallest part; 0 is 64 MiB, and a larger object gets larger parts so it
	// stays under 9,000 of them. Both are bounded by MinPartSize and MaxPartSize. They exist so
	// a test can write in parts without writing gigabytes.
	MultipartThreshold int64
	PartSize           int64
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

	multipartThreshold int64
	partSize           int64
	// composite is set once the store has refused a full-object CRC32 for an upload in parts.
	// Every later upload asks for a composite one instead.
	composite atomic.Bool
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
	threshold, partSize := opts.MultipartThreshold, opts.PartSize
	if threshold == 0 {
		threshold = defaultMultipartThreshold
	}
	if partSize == 0 {
		partSize = defaultPartSize
	}
	for name, v := range map[string]int64{"multipart threshold": threshold, "part size": partSize} {
		if v < MinPartSize || v > MaxPartSize {
			return nil, fmt.Errorf("s3: %s %d is outside %d to %d bytes", name, v, MinPartSize, MaxPartSize)
		}
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
		client:             client,
		bucket:             opts.Bucket,
		logger:             logger,
		conditionalWrite:   opts.Endpoint == "", // If-None-Match on AWS; HeadObject pre-check elsewhere
		multipartThreshold: threshold,
		partSize:           partSize,
	}, nil
}

// VerifyWorm probes bucket Object Lock. A missing lock config is reported as not-locked once a
// listing shows the bucket exists (so the operator can override); other failures, an answer that
// is not S3's among them, are returned as errors.
func (b *Backend) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	out, err := b.client.GetObjectLockConfiguration(ctx, &awss3.GetObjectLockConfigurationInput{
		Bucket: aws.String(b.bucket),
	}, answers("ObjectLockConfiguration"))
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
			// The store implements the call and said no. That negative is earned, and it is
			// one of the most useful things this tool prints, once the bucket is known to exist.
			return b.bucketHasNoLock(ctx)
		}
		/*
		 * Any other API error means the store refused the question, and a refusal is not a no.
		 *
		 * `NotImplemented`, a 501, a 405, a 404 carrying a different code: the store never told
		 * us what it locks. Google's S3 surface is the case that made this visible — it answers
		 * `?object-lock` about Object Retention Lock, so a bucket protected by a locked Bucket
		 * Lock policy is indistinguishable from an open one — but the rule is about the
		 * protocol and not about a provider. Sniffing the endpoint would suppress a true
		 * positive on a Google bucket that does have Object Retention Lock, and would help no
		 * other store.
		 *
		 * The error text is kept, redacted upstream: `NotImplemented` and `AccessDenied` land
		 * in the same verdict and are entirely different things to an operator. The code goes in
		 * Details shaped, since a store chooses it; the whole error goes in Refusal, for a log.
		 */
		var api smithy.APIError
		if errors.As(err, &api) {
			return dest.WormStatus{
				Verdict: dest.VerdictUnknown,
				Details: fmt.Sprintf("could not verify immutability: the bucket answered %s", dest.ShapedCode(api.ErrorCode())),
				Refusal: err,
			}, nil
		}
		return dest.WormStatus{}, fmt.Errorf("s3: get object lock config: %w", err)
	}

	cfg := out.ObjectLockConfiguration
	enabled := cfg != nil && cfg.ObjectLockEnabled == s3types.ObjectLockEnabledEnabled
	// Object Lock can only be enabled at bucket creation and cannot be turned off, so
	// "enabled" means any retention we apply per object is durably enforced.
	st := dest.WormStatus{Verdict: dest.VerdictImmutable, Details: "Object Lock enabled"}
	if !enabled {
		st.Verdict = dest.VerdictNotImmutable
		st.Details = "Object Lock not enabled"
		return st, nil
	}
	if cfg.Rule != nil && cfg.Rule.DefaultRetention != nil {
		st.Mode = string(cfg.Rule.DefaultRetention.Mode)
		st.Details = fmt.Sprintf("Object Lock enabled; default retention %s", dest.ShapedCode(st.Mode))
	}
	return st, nil
}

// bucketHasNoLock takes the store at its word that the bucket has no Object Lock configuration,
// once a listing has shown the bucket is there.
//
// MinIO gives that answer about a bucket that does not exist, and gitdr reported a bucket nobody
// had created as not immutable. A listing of one key tells the two apart, and for a missing
// bucket its answer is S3's own NoSuchBucket. It needs s3:ListBucket on AWS and listFiles on
// Backblaze, which the policies in docs/QUICKSTART.md grant for the listings a backup makes
// anyway. HeadBucket would not do: it answers without a body, so a missing bucket and a refused
// one look alike, and on Backblaze it needs listBuckets, which those keys do not have.
func (b *Backend) bucketHasNoLock(ctx context.Context) (dest.WormStatus, error) {
	_, _, err := b.ListPage(ctx, "", 1)
	if err == nil {
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Details: "bucket has no Object Lock configuration",
		}, nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Details: fmt.Sprintf("could not verify immutability: the bucket answered %s", dest.ShapedCode(api.ErrorCode())),
			Refusal: err,
		}, nil
	}
	return dest.WormStatus{}, fmt.Errorf("s3: confirm the bucket exists: %w", err)
}

// PutImmutable creates key, create-only, never overwriting or deleting. Object Lock
// retention is applied only when ret carries a retain-until (i.e. the bucket is
// immutable); on the non-WORM adoption path we write plainly, because sending lock
// headers to a bucket without Object Lock is a 400. An explicit CRC32 checksum is always
// sent: AWS and some S3-compatible stores (Backblaze B2) require Content-MD5 or an
// x-amz-checksum header on Object Lock PutObject requests.
//
// The CRC32 is computed here and sent as a header, so the body goes as it is, with its length.
// Asked for the algorithm alone, the SDK computes the checksum on the way out, and over TLS it
// sends the body aws-chunked, the whole object as one chunk, with the checksum in a trailer
// (service/internal/checksum v1.11.3: middleware_compute_input_checksum.go, lines 163-177, and
// aws_chunked_encoding.go, lines 90-111). AWS takes a chunk of any size; MinIO refuses one over
// 16 MiB, so no object over 16 MiB could be written to MinIO over TLS. A checksum header already
// on the request turns the trailer off (the same file, lines 131-139 and 285-290).
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	// Past the threshold, in parts (multipart.go). Every caller in gitdr hands a file or a byte
	// slice, both readable at an offset; anything else goes as one PUT.
	if ra, ok := r.(io.ReaderAt); ok && size > b.multipartThreshold {
		var base int64
		if s, ok := r.(io.Seeker); ok {
			var err error
			if base, err = s.Seek(0, io.SeekCurrent); err != nil {
				return dest.PutResult{}, fmt.Errorf("s3: put %q: %w", key, err)
			}
		}
		return b.putParts(ctx, key, ra, base, size, ret)
	}

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

	// After the existence check, so an object that is refused is not read first.
	crc, ok, err := crc32Of(r, size)
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("s3: checksum %q: %w", key, err)
	}
	if ok {
		in.ChecksumCRC32 = aws.String(crc)
	}

	out, err := b.client.PutObject(ctx, in)
	if err != nil {
		// The SDK sends a write again when it lost the answer to the first one, and the first may
		// have landed. On AWS the second meets If-None-Match and is refused, 412, for the object
		// the first one wrote. So before calling a write failed, ask the store what is at the key.
		if !ok {
			return dest.PutResult{}, fmt.Errorf("s3: put %q: %w", key, err)
		}
		switch state, head, serr := b.settle(ctx, key, size, crc); state {
		case copyOurs:
			b.logger.Info("s3: the write had landed and its answer was lost; the object at the key is this one", "key", key)
			return b.settledResult(key, size, head, ret), nil
		case copyOther:
			if serr == nil {
				return dest.PutResult{}, fmt.Errorf("s3: refusing to overwrite existing object %q: %w", key, err)
			}
		}
		return dest.PutResult{}, fmt.Errorf("s3: put %q: %w", key, err)
	}
	return putResult(key, size, ret, out.ETag, out.VersionId), nil
}

func putResult(key string, size int64, ret dest.Retention, etag, versionID *string) dest.PutResult {
	res := dest.PutResult{Key: key, Size: size, ETag: strings.Trim(aws.ToString(etag), `"`), VersionID: aws.ToString(versionID)}
	if !ret.Until.IsZero() {
		res.RetainUntil = ret.Until.UTC()
		res.RetainMode = dest.RetentionCompliance
		if ret.Mode == dest.RetentionGovernance {
			res.RetainMode = dest.RetentionGovernance
		}
	}
	return res
}

// settledResult is the result of a write settle took as ours, because the object at the key holds
// its bytes. Its retention is the one HeadObject shows that object held to, and never the one the
// write asked for: the object can be an earlier write's with the same bytes, held to that write's
// date and mode, and the date asked for would put a retention in the manifest that the store does
// not hold. AWS shows the lock on HeadObject only to a key allowed s3:GetObjectRetention, and
// Backblaze documents no lock headers on HeadObject at all. Where none is shown the result claims
// none, and the manifest says less than the store holds rather than more.
func (b *Backend) settledResult(key string, size int64, head *awss3.HeadObjectOutput, ret dest.Retention) dest.PutResult {
	res := dest.PutResult{Key: key, Size: size, ETag: strings.Trim(aws.ToString(head.ETag), `"`), VersionID: aws.ToString(head.VersionId)}
	if head.ObjectLockRetainUntilDate != nil && head.ObjectLockMode != "" {
		res.RetainUntil = head.ObjectLockRetainUntilDate.UTC()
		res.RetainMode = dest.RetentionMode(head.ObjectLockMode)
	}
	asked := putResult(key, size, ret, nil, nil)
	if res.RetainUntil.Before(asked.RetainUntil) || (asked.RetainMode == dest.RetentionCompliance && res.RetainMode != dest.RetentionCompliance) {
		b.logger.Warn("s3: the store does not show the object at the key held as long or as firmly as this write asked; the result records what it shows",
			"key", key, "held_until", res.RetainUntil, "held_mode", res.RetainMode, "asked_until", asked.RetainUntil, "asked_mode", asked.RetainMode)
	}
	return res
}

// copyState is what a store holds at a key, measured against the object a write sent.
type copyState int

const (
	copyAbsent copyState = iota // nothing is there
	copyOurs                    // our size and our CRC32
	copyOther                   // something else, or something the store would not show us
)

// settle asks the store what is at key after a write whose answer was lost or refused, and
// compares it with what the write sent: size bytes whose CRC32 is one of crcs.
//
// The checksum decides, with the size. A store that will not return the checksum has not shown
// that the object is this one, and that is answered as someone else's: a refusal stays a refusal.
func (b *Backend) settle(ctx context.Context, key string, size int64, crcs ...string) (copyState, *awss3.HeadObjectOutput, error) {
	out, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket:       aws.String(b.bucket),
		Key:          aws.String(key),
		ChecksumMode: s3types.ChecksumModeEnabled,
	})
	if err != nil {
		if notFound(err) {
			return copyAbsent, nil, nil
		}
		return copyOther, nil, err
	}
	if aws.ToInt64(out.ContentLength) == size && out.ChecksumCRC32 != nil && slices.Contains(crcs, *out.ChecksumCRC32) {
		return copyOurs, out, nil
	}
	return copyOther, out, nil
}

// crc32Of is the CRC32 S3 expects of the next size bytes of r, base64 of the big-endian sum, read
// and then rewound to where r stood.
//
// Every caller in gitdr passes a file or a byte slice. A reader that cannot rewind reports no
// checksum, and the SDK computes it on the way out instead, as a trailer over TLS. That is the
// path a store limiting chunk size refuses past the limit, and nothing in gitdr takes it.
func crc32Of(r io.Reader, size int64) (string, bool, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return "", false, nil
	}
	start, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", false, err
	}
	sum := crc32.NewIEEE()
	if _, err := io.CopyN(sum, rs, size); err != nil {
		return "", false, err
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return "", false, err
	}
	return base64.StdEncoding.EncodeToString(sum.Sum(nil)), true, nil
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
	if notFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3: head %q: %w", key, err)
}

// notFound reports whether err is a store saying there is nothing at the key.
func notFound(err error) bool {
	if _, ok := errors.AsType[*s3types.NotFound](err); ok {
		return true
	}
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
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
			objs = append(objs, dest.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified)})
		}
	}
	return objs, nil
}

// ListPage lists at most limit objects under prefix with one ListObjectsV2 request (read-only),
// and says whether the store reported more. It never sends the continuation token back, so on a
// bucket of a million objects it is one request, and on a store that answers every page with
// another one it is still one.
func (b *Backend) ListPage(ctx context.Context, prefix string, limit int) ([]dest.Object, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, fmt.Errorf("s3: list %q: a page holds 1 to 1000 objects, not %d", prefix, limit)
	}
	out, err := b.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(limit)),
	}, answers("ListBucketResult"))
	if err != nil {
		return nil, false, fmt.Errorf("s3: list %q: %w", prefix, err)
	}
	objs := make([]dest.Object, 0, len(out.Contents))
	for _, o := range out.Contents {
		objs = append(objs, dest.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)})
	}
	// A store that sends more than it was asked for is read no further than one that did not.
	if len(objs) > limit {
		objs = objs[:limit]
	}
	return objs, aws.ToBool(out.IsTruncated), nil
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

// ObserveRetention asks S3 what retention is actually on an object gitdr wrote.
//
// `GetObjectRetention` rather than `HeadObject`, and the difference decides whether this check is
// usable at all. HeadObject returns `x-amz-object-lock-mode` **only if the caller holds
// `s3:GetObjectRetention`** - without it the response is a 200 with the headers silently omitted,
// byte-identical to an object that carries no retention. SPEC tells every operator to scope
// destination credentials create/put-only, so a HEAD-based check would report "the destination
// accepted a write it did not retain" about correctly configured, genuinely protected buckets
// belonging to the operators who followed that advice. That is the unearned negative WormVerdict
// exists to prevent, and it would teach people to ignore the one warning that matters.
//
// `?retention` fails distinguishably instead:
//
//	NoSuchObjectLockConfiguration  the store implements the question and this object holds
//	                               nothing. An earned negative. AWS answers it with a 404 and
//	                               MinIO with a 400.
//	ObjectLockConfigurationNotFoundError
//	                               the same negative from Backblaze B2, a 404. AWS gives this code
//	                               to the bucket's lock question, which VerifyWorm asks; here it
//	                               answers a question about one object.
//	AccessDenied / NotImplemented  a refusal, and a refusal is not a no.
//	anything else                  unclassified, and unclassified is not a no either.
func (b *Backend) ObserveRetention(ctx context.Context, key string) (dest.RetentionObservation, time.Time, error) {
	out, err := b.client.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}, answers("Retention"))
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && objectHoldsNoRetention(api.ErrorCode()) {
			return dest.RetentionAbsent, time.Time{}, nil
		}
		// Every other failure is the store declining to answer. Returned with the error so a
		// caller can log why, and with the observation that makes no claim.
		return dest.RetentionNotChecked, time.Time{}, err
	}
	if out.Retention == nil || out.Retention.RetainUntilDate == nil {
		// A Retention document with nothing in it is the store answering "none", which is the
		// same earned negative as the error code above. answers() has seen to it that it is one.
		return dest.RetentionAbsent, time.Time{}, nil
	}
	return dest.RetentionPresent, out.Retention.RetainUntilDate.UTC(), nil
}

// objectHoldsNoRetention reports whether code, a store's answer to GetObjectRetention, says the
// object holds no retention. Only that read asks it: the same code about a bucket is VerifyWorm's
// to read, on its own terms.
func objectHoldsNoRetention(code string) bool {
	switch code {
	case "NoSuchObjectLockConfiguration", // AWS, MinIO
		"ObjectLockConfigurationNotFoundError": // Backblaze B2
		return true
	}
	return false
}
