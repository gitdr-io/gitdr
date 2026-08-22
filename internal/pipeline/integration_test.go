//go:build integration

// Full backup -> verify -> restore loop against a real Object-Lock S3 (MinIO in CI).
// Needs the `integration` build tag and GITDR_TEST_S3_ENDPOINT; run it with
// `make test-integration`. The test provisions the locked bucket
// directly via the SDK, the tool itself never creates or deletes buckets.
package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

func TestMinIOFullLoop(t *testing.T) {
	endpoint := s3Endpoint(t)
	bucket := envOr("GITDR_TEST_S3_BUCKET", "gitdr-itest")
	region := envOr("AWS_REGION", "us-east-1")
	ctx := context.Background()

	// Run from somewhere that is not a git repository. Go tests run in the package
	// directory, which is inside this repository, so anything the restore path shells out
	// to inherits a valid repo it will not have in production -- the container runs at /.
	// That is exactly how a broken `git bundle verify` passed its tests and shipped in
	// every release.
	t.Chdir(t.TempDir())

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
	rres, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: dst, Git: gitexec.New(nil), PublicKey: pub}, pipeline.RestoreRequest{
		Host: repo.Host, Owner: repo.Owner, Name: repo.Name,
		Date: time.Now().UTC().Format("2006-01-02"), OutDir: out,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !rres.Verified {
		t.Fatal("restore not verified")
	}
	if !strings.Contains(rres.Verification, "signed manifest") {
		t.Fatalf("Verification = %q, want the signed manifest used", rres.Verification)
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

// s3Endpoint locates the object-lock S3 to test against, or skips.
//
// In CI it fails instead of skipping. A skipped Go test prints nothing and the package
// still reports ok, so a missing MinIO would look exactly like a passing full loop -- and
// this is the loop the whole product is.
func s3Endpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("GITDR_TEST_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("GITDR_TEST_S3_ENDPOINT is not set; in CI the full backup/verify/restore loop must run, not skip")
		}
		t.Skip("set GITDR_TEST_S3_ENDPOINT (and AWS_* creds) to run the MinIO integration test")
	}
	return endpoint
}

// TestS3CreateOnly proves the S3 destination refuses to overwrite.
//
// This is invariant three -- backups are append-only by construction -- on the backend most
// people actually use: AWS, and every S3-compatible store behind it. Nothing proved it. The
// custom endpoint here selects the portable HeadObject path, which is the one Backblaze B2,
// Cloudflare R2 and Wasabi take; real AWS uses If-None-Match instead.
func TestS3CreateOnly(t *testing.T) {
	endpoint := s3Endpoint(t)
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

	// Unique per run: the destination has no delete path, so a fixed key would pass once
	// and fail on its own leftovers ever after.
	key := fmt.Sprintf("github.com/octo/immutable/%d.bundle", time.Now().UnixNano())
	first := []byte("the original bundle")
	if _, err := dst.PutImmutable(ctx, key, bytes.NewReader(first), int64(len(first)), dest.Retention{}); err != nil {
		t.Fatalf("first put: %v", err)
	}

	second := []byte("an attacker's replacement")
	if _, err := dst.PutImmutable(ctx, key, bytes.NewReader(second), int64(len(second)), dest.Retention{}); err == nil {
		t.Fatal("second put to the same key succeeded; the destination is not create-only")
	}

	// Refusing is only half of it. What is stored must still be the original.
	rc, err := dst.Get(ctx, key)
	if err != nil {
		t.Fatalf("get after refused overwrite: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, first) {
		t.Fatalf("stored bytes changed after a refused overwrite: got %q want %q", got, first)
	}
}
