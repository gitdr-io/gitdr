package s3

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// ConditionalWrites puts b on the path it takes against AWS itself, If-None-Match on every
// write, while it points at a fake store's endpoint.
func ConditionalWrites(b *Backend) { b.conditionalWrite = true }

// NoBackoff keeps b's retries and takes the wait out from between them, so a test whose write is
// refused on every attempt does not sleep through the SDK's backoff.
func NoBackoff(b *Backend) {
	b.client = awss3.New(b.client.Options(), func(o *awss3.Options) {
		o.Retryer = retry.NewStandard(func(so *retry.StandardOptions) {
			so.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
		})
	})
}

// PartPlan is how an object of size bytes is cut into parts of at least smallest.
func PartPlan(size, smallest int64) (partSize int64, count int) {
	p := planParts(size, smallest)
	return p.partSize, p.count
}
