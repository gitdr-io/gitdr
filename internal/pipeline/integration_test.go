//go:build integration

// Full backup -> verify -> restore loop against a real Object-Lock S3 (MinIO in CI).
// Skipped unless GITDR_TEST_S3_ENDPOINT is set. The test provisions the locked bucket
// directly via the SDK, the tool itself never creates or deletes buckets.
package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

func TestMinIOFullLoop(t *testing.T) {
	endpoint := os.Getenv("GITDR_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GITDR_TEST_S3_ENDPOINT (and AWS_* creds) to run the MinIO integration test")
	}
	bucket := envOr("GITDR_TEST_S3_BUCKET", "gitdr-itest")
	region := envOr("AWS_REGION", "us-east-1")
	ctx := context.Background()

	provisionLockedBucket(ctx, t, endpoint, region, bucket)

	dst, err := s3backend.New(ctx, s3backend.Options{
		Bucket: bucket, Region: region, Endpoint: endpoint, UsePathStyle: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	repoDir := initFixtureRepo(t)
	repo := source.Repo{
		Host: "github.com", Owner: "octo",
		Name:     fmt.Sprintf("hello-%d", time.Now().UnixNano()), // unique: immutable keys can't be rewritten
		CloneURL: repoDir, DefaultBranch: "main",
	}
	src := &fixtureSource{repos: []source.Repo{repo}}

	pubPEM, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)

	conf := config.Default()
	conf.Destination.S3.Bucket = bucket
	conf.Destination.Retention.Days = 1
	conf.Source.Repo = repo.Slug()

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: conf, Source: src, Dest: dst, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "itest", Now: time.Now,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if res.Manifest.Status != pipeline.StatusSuccess {
		t.Fatalf("status = %s", res.Manifest.Status)
	}

	if _, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: dst, PublicKey: pub}, res.ManifestKey); err != nil {
		t.Fatalf("verify: %v", err)
	}

	out := filepath.Join(t.TempDir(), "restored")
	rres, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: dst, Git: gitexec.New(nil)}, pipeline.RestoreRequest{
		Host: repo.Host, Owner: repo.Owner, Name: repo.Name,
		Date: time.Now().UTC().Format("2006-01-02"), OutDir: out,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !rres.Verified {
		t.Fatal("restore not verified")
	}
	if _, err := os.Stat(filepath.Join(out, "README.md")); err != nil {
		t.Fatalf("restored repo missing README.md: %v", err)
	}
}

func provisionLockedBucket(ctx context.Context, t *testing.T, endpoint, region, bucket string) {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	raw := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	in := &awss3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)}
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, err := raw.CreateBucket(ctx, in); err != nil && !isAlreadyExists(err) {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err := raw.PutObjectLockConfiguration(ctx, &awss3.PutObjectLockConfigurationInput{
		Bucket: aws.String(bucket),
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{
			ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
			Rule: &s3types.ObjectLockRule{
				DefaultRetention: &s3types.DefaultRetention{
					Mode: s3types.ObjectLockRetentionModeCompliance,
					Days: aws.Int32(1),
				},
			},
		},
	}); err != nil {
		t.Fatalf("put object lock config: %v", err)
	}
}

func isAlreadyExists(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
