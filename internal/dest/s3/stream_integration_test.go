//go:build integration

// Streams of another size than the artifact, against a real Object Lock store, MinIO in CI. Needs
// the `integration` build tag and GITDR_TEST_S3_ENDPOINT, with AWS_* credentials for it; `make
// test-integration` runs it.
package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// minioCase is a stream case as MinIO gets it. A late one is a stream that grows once it is
// checksummed and hands over its extra byte only once the store holds an object at the key, or
// after two seconds when it never does.
type minioCase struct {
	streamCase
	late bool
}

// TestMinIOCommitsNothingFromAStreamOfAnotherSize writes every stream of streamCases, and one more,
// to a locked MinIO bucket and asks the store what it holds at the key: no version at all. Then the
// artifact itself goes to the same key and is held there under its retention.
//
// One PUT is written over plain HTTP, as CI reaches MinIO, and over TLS, as AWS and every hosted
// store is reached, and the two differ where it matters. Over plain HTTP the SDK signs a SHA-256 of
// the payload, which it computes by reading the stream to its end, and the store refuses a body that
// does not match it, so there a stream that was long when the SDK read it was refused, with gitdr's
// own check or without it. Over TLS the payload goes unsigned, and gitdr's checksum is the only check. TLS here is a front on this
// machine that ends TLS and passes each connection on to MinIO byte for byte, a hang-up included. In
// parts the transport changes nothing: each part is read at its offset and stops at size, whatever
// carries it, and the question is whether Complete is sent.
//
// The late stream is the one TLS is there for. MinIO commits the whole request and answers it
// before net/http asks for the byte after it, and the SDK takes that answer
// (net/http/transport.go, lines 2952-2953, Go 1.26.9). Up to v0.1.21 the write was reported done and
// the key held the stream's first size bytes. Where the extra byte comes at once, the client hangs up
// as soon as the request is sent, and whether MinIO commits it races MinIO noticing: it committed
// one write in twelve in a probe on 2026-10-11.
func TestMinIOCommitsNothingFromAStreamOfAnotherSize(t *testing.T) {
	endpoint := os.Getenv("GITDR_TEST_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("GITDR_TEST_S3_ENDPOINT is not set; in CI the writes against MinIO must be checked, not skipped")
		}
		t.Skip("set GITDR_TEST_S3_ENDPOINT (and AWS_* creds) to check the writes against MinIO")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	raw := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	// A bucket of this run's own. There is no delete path, so it stays.
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gitdr-stream-" + hex.EncodeToString(b[:])
	if _, err := raw.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatalf("create %s: %v", bucket, err)
	}

	type transport struct {
		name, endpoint, ca string
		tls                bool
	}
	plain := transport{name: "plain HTTP", endpoint: endpoint}
	overTLS := transport{name: "TLS", endpoint: endpoint, tls: true}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" {
		overTLS.endpoint, overTLS.ca = tlsFront(t, u.Host)
	}

	var cases []minioCase
	for _, c := range streamCases() {
		cases = append(cases, minioCase{streamCase: c})
	}
	cases = append(cases, minioCase{streamCase: streamCase{name: "one PUT/grows once it is checksummed, its extra byte late", size: onePut}, late: true})

	ret := dest.Retention{Mode: dest.RetentionCompliance, Until: time.Now().Add(24 * time.Hour).Truncate(time.Second)}
	written := 0
	for _, tr := range []transport{plain, overTLS} {
		for _, m := range modes() {
			for _, c := range cases {
				if (c.inParts && tr.tls) || (c.late && !tr.tls) {
					continue
				}
				written++
				key := fmt.Sprintf("github.com/octo/stream/%03d/hello.bundle", written)
				t.Run(tr.name+"/"+m.name+"/"+c.name, func(t *testing.T) {
					if tr.ca != "" {
						t.Setenv("AWS_CA_BUNDLE", tr.ca)
					}
					opts := streamOptions()
					opts.Bucket, opts.Region, opts.Endpoint, opts.UsePathStyle = bucket, region, tr.endpoint, true
					be, err := s3backend.New(ctx, opts, nil)
					if err != nil {
						t.Fatal(err)
					}
					s3backend.NoBackoff(be)
					if m.conditional {
						s3backend.ConditionalWrites(be)
					}
					data := artifactBytes(t, c.streamCase)
					var stream io.Reader
					if c.late {
						stream = &changing{artifact: data[:c.size], later: data[:c.size+1], fail: -1, hold: func() {
							heldUntilStored(ctx, raw, bucket, key)
						}}
					} else {
						stream = c.open(data)
					}

					if _, err := be.PutImmutable(ctx, key, stream, int64(c.size), ret); err == nil {
						t.Fatalf("the stream was stored as the %d-byte artifact", c.size)
					}
					if got := versionsAt(ctx, t, raw, bucket, key); got != 0 {
						t.Fatalf("after the refusal MinIO holds %d versions at the key, want none", got)
					}

					res, err := be.PutImmutable(ctx, key, bytes.NewReader(data[:c.size]), int64(c.size), ret)
					if err != nil {
						t.Fatalf("the artifact itself, written next, was refused: %v", err)
					}
					if res.Size != int64(c.size) {
						t.Errorf("put recorded %d bytes, want %d", res.Size, c.size)
					}
					rc, err := be.Get(ctx, key)
					if err != nil {
						t.Fatalf("get: %v", err)
					}
					got, err := io.ReadAll(rc)
					_ = rc.Close()
					if err != nil || !bytes.Equal(got, data[:c.size]) {
						t.Fatalf("read back %d bytes (%v), not the %d-byte artifact", len(got), err, c.size)
					}
					if obs, until, err := be.ObserveRetention(ctx, key); obs != dest.RetentionPresent || !until.Equal(ret.Until) {
						t.Errorf("retention %s until %v (%v), want present until %v", obs, until, err, ret.Until)
					}
					if got := versionsAt(ctx, t, raw, bucket, key); got != 1 {
						t.Errorf("MinIO holds %d versions at the key, want the artifact's one", got)
					}
				})
			}
		}
	}
}

// heldUntilStored returns once the store holds an object at key, or after two seconds when it does
// not. The deadline is on every request as well: while MinIO reads a PUT with If-None-Match it holds
// the key's lock, and a HeadObject for that key waits on it for about thirty seconds.
func heldUntilStored(ctx context.Context, raw *awss3.Client, bucket, key string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if _, err := raw.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// versionsAt counts what the store holds at key, every version and every delete marker, so an
// object written and then hidden would still count.
func versionsAt(ctx context.Context, t *testing.T, raw *awss3.Client, bucket, key string) int {
	t.Helper()
	out, err := raw.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	if err != nil {
		t.Fatalf("list the versions at %s: %v", key, err)
	}
	n := 0
	for _, v := range out.Versions {
		if aws.ToString(v.Key) == key {
			n++
		}
	}
	for _, d := range out.DeleteMarkers {
		if aws.ToString(d.Key) == key {
			n++
		}
	}
	return n
}

// tlsFront ends TLS on this machine and passes every connection on to MinIO at host, its plain
// HTTP endpoint, byte for byte. It returns the front's URL and a CA file that trusts it, for
// AWS_CA_BUNDLE. When a client stops sending, MinIO's connection is closed for writing, as the
// client's own would be over TLS of MinIO's own.
func tlsFront(t *testing.T, host string) (string, string) {
	t.Helper()
	// httptest's certificate, for 127.0.0.1, taken from a server that is closed again at once.
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	conf := srv.TLS.Clone()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	srv.Close()
	caFile := filepath.Join(t.TempDir(), "front-ca.pem")
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", conf)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var open []net.Conn
	keep := func(c net.Conn) {
		mu.Lock()
		open = append(open, c)
		mu.Unlock()
	}
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range open {
			_ = c.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			keep(client)
			go func() {
				defer func() { _ = client.Close() }()
				minio, err := net.Dial("tcp", host)
				if err != nil {
					return
				}
				keep(minio)
				defer func() { _ = minio.Close() }()
				answered := make(chan struct{})
				go func() {
					_, _ = io.Copy(client, minio)
					_ = client.Close()
					close(answered)
				}()
				_, _ = io.Copy(minio, client)
				if tc, ok := minio.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				<-answered
			}()
		}
	}()
	return "https://" + ln.Addr().String(), caFile
}
