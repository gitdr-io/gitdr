//go:build integration

// The WORM verdict against a real Object Lock store, MinIO in CI. Needs the `integration` build
// tag and GITDR_TEST_S3_ENDPOINT, with AWS_* credentials for it; `make test-integration` runs it.
package s3_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// A locked bucket, an unlocked one and one that does not exist get three different answers.
//
// MinIO answers the lock question about a bucket that does not exist with
// ObjectLockConfigurationNotFoundError, the same 404 an unlocked bucket gets, so before the
// listing that tells them apart a bucket nobody created read "not immutable". Backup's WORM gate
// and `gitdr doctor` both take their verdict from VerifyWorm.
func TestVerifyWormTellsAMissingBucketFromAnUnlockedOne(t *testing.T) {
	endpoint := os.Getenv("GITDR_TEST_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("GITDR_TEST_S3_ENDPOINT is not set; in CI the verdict against MinIO must be checked, not skipped")
		}
		t.Skip("set GITDR_TEST_S3_ENDPOINT (and AWS_* creds) to check the verdict against MinIO")
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

	// Names of this run's own. There is no delete path, so a bucket made here stays.
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(b[:])
	locked, open, missing := "gitdr-worm-locked-"+suffix, "gitdr-worm-open-"+suffix, "gitdr-worm-missing-"+suffix
	if _, err := raw.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(locked), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatalf("create %s: %v", locked, err)
	}
	if _, err := raw.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(open)}); err != nil {
		t.Fatalf("create %s: %v", open, err)
	}

	for _, tc := range []struct {
		bucket  string
		verdict dest.WormVerdict
		code    string // the code of the refusal behind an unknown verdict
		absent  bool   // the refusal says the bucket does not exist, which stops a backup
	}{
		{locked, dest.VerdictImmutable, "", false},
		{open, dest.VerdictNotImmutable, "", false},
		{missing, dest.VerdictUnknown, "NoSuchBucket", true},
	} {
		t.Run(tc.bucket, func(t *testing.T) {
			backend, err := s3backend.New(ctx, s3backend.Options{Bucket: tc.bucket, Region: region, Endpoint: endpoint, UsePathStyle: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			st, err := backend.VerifyWorm(ctx)
			if err != nil {
				t.Fatalf("VerifyWorm: %v", err)
			}
			if st.Verdict != tc.verdict {
				t.Errorf("verdict = %q (%s), want %q", st.Verdict.Wire(), st.Details, tc.verdict.Wire())
			}
			var api smithy.APIError
			if tc.code != "" && (!errors.As(st.Refusal, &api) || api.ErrorCode() != tc.code) {
				t.Errorf("Refusal = %v, want MinIO's %s", st.Refusal, tc.code)
			}
			if got := errors.Is(st.Refusal, dest.ErrNoSuchBucket); got != tc.absent {
				t.Errorf("Refusal %v: marked as a missing bucket = %v, want %v", st.Refusal, got, tc.absent)
			}
		})
	}
}
