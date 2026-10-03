package s3

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gitdr.io/gitdr/internal/dest"
)

// An object over the multipart threshold is written in parts, because a single PutObject stops at
// 5 GiB on AWS, B2, R2 and Wasabi.
//
// Create-only holds as it does for one PUT. The key is checked before the upload starts, on every
// store, so gigabytes are never sent only to be refused. On AWS, Complete carries If-None-Match;
// elsewhere the key is checked again just before Complete. Complete is sent once and whatever
// happened to it is settled by asking the store what is at the key.
//
// No upload is ever aborted. An upload in flight is not an object, a bucket's lifecycle rule ends
// the ones a stopped run leaves behind, and this package holds no call that takes anything away.
//
// Every part carries its own CRC32 in a header, computed before it is sent. Asked for the
// algorithm alone, the SDK would send each part aws-chunked as a single chunk with the checksum in
// a trailer, and MinIO refuses a chunk over 16 MiB (see PutImmutable).

const (
	defaultMultipartThreshold = 4 << 30
	defaultPartSize           = 64 << 20
	// MinPartSize and MaxPartSize are S3's own bounds on a part, and so on both settings.
	MinPartSize = 5 << 20
	MaxPartSize = 5 << 30
	// partTarget keeps the count of parts well under S3's 10,000: the part size grows with the
	// object instead.
	partTarget = 9000
	// partsInFlight is how many parts are sent at once.
	partsInFlight = 4
	// partAttempts bounds the sends of one part: the SDK sends a failed part again, with backoff,
	// never the whole object.
	partAttempts = 3
)

// partPlan is how an object is cut into parts.
type partPlan struct {
	size, partSize int64
	count          int
}

func planParts(size, smallest int64) partPlan {
	ps := max(smallest, (size+partTarget-1)/partTarget)
	ps = min(ps, MaxPartSize)
	return partPlan{size: size, partSize: ps, count: int((size + ps - 1) / ps)}
}

// part is the offset and length of part i, from 0.
func (p partPlan) part(i int) (off, n int64) {
	off = int64(i) * p.partSize
	return off, min(p.partSize, p.size-off)
}

// partSums are the CRC32s of each part and of the whole object.
type partSums struct {
	parts []uint32
	full  uint32
}

func b64(sum uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], sum)
	return base64.StdEncoding.EncodeToString(b[:])
}

// composite is the CRC32 a store reports for a composite upload: the CRC32 of the parts' CRC32s,
// with the count of parts after a dash.
func (s partSums) composite() string {
	sum := crc32.NewIEEE()
	var b [4]byte
	for _, p := range s.parts {
		binary.BigEndian.PutUint32(b[:], p)
		_, _ = sum.Write(b[:])
	}
	return b64(sum.Sum32()) + "-" + strconv.Itoa(len(s.parts))
}

// checksumParts reads the object once and returns the CRC32 of every part and of the whole.
func checksumParts(ctx context.Context, r io.ReaderAt, base int64, plan partPlan) (partSums, error) {
	s := partSums{parts: make([]uint32, plan.count)}
	full := crc32.NewIEEE()
	buf := make([]byte, 1<<20)
	for i := range plan.count {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		off, n := plan.part(i)
		part := crc32.NewIEEE()
		got, err := io.CopyBuffer(io.MultiWriter(part, full), io.NewSectionReader(r, base+off, n), buf)
		if err != nil {
			return s, err
		}
		if got != n {
			return s, fmt.Errorf("part %d: read %d bytes of %d: the object is shorter than its size", i+1, got, n)
		}
		s.parts[i] = part.Sum32()
	}
	s.full = full.Sum32()
	return s, nil
}

// putParts writes size bytes of r, from base, to key in parts.
func (b *Backend) putParts(ctx context.Context, key string, r io.ReaderAt, base, size int64, ret dest.Retention) (dest.PutResult, error) {
	if exists, err := b.objectExists(ctx, key); err != nil {
		return dest.PutResult{}, err
	} else if exists {
		return dest.PutResult{}, fmt.Errorf("s3: refusing to overwrite existing object %q", key)
	}
	plan := planParts(size, b.partSize)
	sums, err := checksumParts(ctx, r, base, plan)
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("s3: checksum %q: %w", key, err)
	}
	res, again, err := b.upload(ctx, key, r, base, plan, sums, ret)
	if again {
		// A 409 at Complete with nothing at the key: AWS's answer is to upload the object again.
		// Once, so two writers racing for a key cannot keep each other going.
		b.logger.Warn("s3: the store answered 409 to completing an upload and holds nothing at the key; uploading once more", "key", key, "err", err)
		res, _, err = b.upload(ctx, key, r, base, plan, sums, ret)
	}
	return res, err
}

// upload makes one multipart upload of the object and completes it. again reports a 409 at
// Complete with nothing at the key afterwards.
func (b *Backend) upload(ctx context.Context, key string, r io.ReaderAt, base int64, plan partPlan, sums partSums, ret dest.Retention) (res dest.PutResult, again bool, err error) {
	id, composite, err := b.createUpload(ctx, key, ret)
	if err != nil {
		return dest.PutResult{}, false, err
	}
	parts, err := b.sendParts(ctx, key, id, r, base, plan, sums)
	if err != nil {
		return dest.PutResult{}, false, fmt.Errorf("s3: put %q in %d parts (the upload is left to the bucket's lifecycle rule): %w", key, plan.count, err)
	}
	return b.complete(ctx, key, id, parts, plan, sums, composite, ret)
}

// createUpload starts an upload with the object's lock and a full-object CRC32, or a composite
// one once the store has refused that.
func (b *Backend) createUpload(ctx context.Context, key string, ret dest.Retention) (id string, composite bool, err error) {
	composite = b.composite.Load()
	in := &awss3.CreateMultipartUploadInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
	}
	if !composite {
		in.ChecksumType = s3types.ChecksumTypeFullObject
	}
	if !ret.Until.IsZero() {
		in.ObjectLockMode = s3types.ObjectLockModeCompliance
		if ret.Mode == dest.RetentionGovernance {
			in.ObjectLockMode = s3types.ObjectLockModeGovernance
		}
		in.ObjectLockRetainUntilDate = aws.Time(ret.Until.UTC())
	}
	out, err := b.client.CreateMultipartUpload(ctx, in)
	if err != nil && !composite && refusedChecksumType(err) {
		if b.composite.CompareAndSwap(false, true) {
			b.logger.Warn("s3: the store refused a full-object CRC32 for an upload in parts; asking for a composite one from here on", "err", err)
		}
		composite, in.ChecksumType = true, ""
		out, err = b.client.CreateMultipartUpload(ctx, in)
	}
	if err != nil {
		return "", false, fmt.Errorf("s3: start upload %q: %w", key, err)
	}
	return aws.ToString(out.UploadId), composite, nil
}

// refusedChecksumType reports whether a store refused the checksum type it was asked for, as
// opposed to anything else about the request.
func refusedChecksumType(err error) bool {
	api, ok := errors.AsType[smithy.APIError](err)
	if !ok {
		return false
	}
	switch api.ErrorCode() {
	case "NotImplemented":
		return true
	case "InvalidArgument", "InvalidRequest", "BadRequest":
		return strings.Contains(strings.ToLower(api.ErrorMessage()), "checksum")
	}
	return false
}

// sendParts sends every part, partsInFlight at a time, and stops at the first that fails.
func (b *Backend) sendParts(ctx context.Context, key, id string, r io.ReaderAt, base int64, plan partPlan, sums partSums) ([]s3types.CompletedPart, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]s3types.CompletedPart, plan.count)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, partsInFlight)
	for i := range plan.count {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			etag, err := b.sendPart(ctx, key, id, r, base, plan, i, b64(sums.parts[i]))
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
				return
			}
			parts[i] = s3types.CompletedPart{
				PartNumber:    aws.Int32(int32(i + 1)),
				ETag:          etag,
				ChecksumCRC32: aws.String(b64(sums.parts[i])),
			}
		})
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return parts, nil
}

// sendPart sends part i with its CRC32 in a header. The SDK sends it again on a failure, up to
// partAttempts times, all inside the part's own deadline.
func (b *Backend) sendPart(ctx context.Context, key, id string, r io.ReaderAt, base int64, plan partPlan, i int, crc string) (*string, error) {
	off, n := plan.part(i)
	ctx, cancel := context.WithTimeout(ctx, partTimeout(n))
	defer cancel()
	out, err := b.client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		UploadId:          aws.String(id),
		PartNumber:        aws.Int32(int32(i + 1)),
		Body:              io.NewSectionReader(r, base+off, n),
		ContentLength:     aws.Int64(n),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
		ChecksumCRC32:     aws.String(crc),
	}, func(o *awss3.Options) { o.RetryMaxAttempts = partAttempts })
	if err != nil {
		return nil, fmt.Errorf("part %d of %d: %w", i+1, plan.count, err)
	}
	return out.ETag, nil
}

// partTimeout is the deadline for one part and its retries: five minutes, or the part at 1 MiB a
// second if that is longer.
func partTimeout(n int64) time.Duration {
	return max(5*time.Minute, time.Duration(n>>20)*time.Second)
}

// complete asks the store to assemble the parts, once, and settles any answer but success by
// asking the store what is at the key. again reports a 409 with nothing there.
func (b *Backend) complete(ctx context.Context, key, id string, parts []s3types.CompletedPart, plan partPlan, sums partSums, composite bool, ret dest.Retention) (dest.PutResult, bool, error) {
	in := &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(id),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}
	ours := []string{b64(sums.full)}
	if composite {
		ours = []string{sums.composite(), strings.Split(sums.composite(), "-")[0]}
	} else {
		in.ChecksumCRC32 = aws.String(b64(sums.full))
		in.ChecksumType = s3types.ChecksumTypeFullObject
	}
	if b.conditionalWrite {
		// AWS refuses the object, before it commits and locks, if it already exists or does not
		// come to the size every part added up to.
		in.IfNoneMatch = aws.String("*")
		in.MpuObjectSize = aws.Int64(plan.size)
	} else if exists, err := b.objectExists(ctx, key); err != nil {
		return dest.PutResult{}, false, err
	} else if exists {
		// No conditional write here, so the key is looked at again just before the object comes
		// into being: another writer may have finished first.
		return dest.PutResult{}, false, fmt.Errorf("s3: refusing to overwrite existing object %q, written while this upload was in flight", key)
	}

	out, err := b.client.CompleteMultipartUpload(ctx, in, func(o *awss3.Options) { o.RetryMaxAttempts = 1 })
	if err == nil {
		return putResult(key, plan.size, ret, out.ETag, out.VersionId), false, nil
	}
	switch state, head, serr := b.settle(ctx, key, plan.size, ours...); state {
	case copyOurs:
		b.logger.Info("s3: completing the upload landed and its answer was lost; the object at the key is this one", "key", key)
		return putResult(key, plan.size, ret, head.ETag, head.VersionId), false, nil
	case copyOther:
		if serr == nil {
			return dest.PutResult{}, false, fmt.Errorf("s3: refusing to overwrite existing object %q: %w", key, err)
		}
	case copyAbsent:
		if conflict(err) {
			return dest.PutResult{}, true, err
		}
	}
	return dest.PutResult{}, false, fmt.Errorf("s3: complete %q: %w", key, err)
}

// conflict reports a 409: on Complete, AWS's way of saying the upload must start again.
func conflict(err error) bool {
	if resp, ok := errors.AsType[*smithyhttp.ResponseError](err); ok && resp.HTTPStatusCode() == 409 {
		return true
	}
	api, ok := errors.AsType[smithy.APIError](err)
	return ok && api.ErrorCode() == "ConditionalRequestConflict"
}
