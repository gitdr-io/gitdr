package s3

import (
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"gitdr.io/gitdr/internal/dest"
)

// LimitResponses caps every response body b reads at limit bytes from here on: an answer that runs
// past it fails with dest.ErrResponseTooLarge rather than being read to its end. Call it before b
// is in use. A limit of zero or less leaves b as it is.
//
// It is for a caller that reads only a store's short answers and must not be held by a long one:
// `gitdr doctor`, pointed at an endpoint it has no reason to trust yet. Backup never calls it, since
// a GetObject body is a whole bundle. Uncapped, the SDK reads all of an answer: an error body is
// copied into memory whole, and every body is drained to its end before it is closed.
//
// The client is rebuilt from its own options, so everything else about it, the endpoint, the
// credentials, the retries and the transport's timeouts, stays as New made it.
func (b *Backend) LimitResponses(limit int64) {
	if limit <= 0 {
		return
	}
	b.client = awss3.New(b.client.Options(), func(o *awss3.Options) {
		next := o.HTTPClient
		if next == nil {
			next = awshttp.NewBuildableClient()
		}
		o.HTTPClient = limitedClient{next: next, limit: limit}
	})
}

// limitedClient is an HTTP client whose every response body is capped at limit bytes.
type limitedClient struct {
	next  awss3.HTTPClient
	limit int64
}

func (c limitedClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.next.Do(req)
	if resp != nil && resp.Body != nil {
		resp.Body = dest.LimitBody(resp.Body, c.limit)
	}
	return resp, err
}
