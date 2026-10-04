//go:build integration

package pipeline_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// TestMinIODrillReportIsLocked runs a backup and a drill against a real Object Lock store and
// reads back the retention the drill report and its signature hold.
//
// The bucket has Object Lock and no default retention, as a Backblaze B2 bucket has unless one is
// set, and gitdr's own e2e bucket there has none. An object written without lock headers holds
// nothing on such a bucket, so the report is locked only if gitdr locks it. On the bucket the
// other tests use, with a default rule, the store would lock it either way and this would prove
// nothing.
func TestMinIODrillReportIsLocked(t *testing.T) {
	endpoint := s3Endpoint(t)
	bucket := envOr("GITDR_TEST_S3_UNRULED_BUCKET", "gitdr-itest-unruled")
	region := envOr("AWS_REGION", "us-east-1")
	ctx := context.Background()
	t.Chdir(t.TempDir()) // the drill restores, and git must not find this repository around it

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	raw := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) { o.BaseEndpoint, o.UsePathStyle = aws.String(endpoint), true })
	if _, err := raw.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil && !isAlreadyExists(err) {
		t.Fatalf("create bucket: %v", err)
	}
	lock, err := raw.GetObjectLockConfiguration(ctx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("lock configuration: %v", err)
	}
	if c := lock.ObjectLockConfiguration; c == nil || c.ObjectLockEnabled != s3types.ObjectLockEnabledEnabled || c.Rule != nil {
		t.Fatalf("bucket %s must have Object Lock and no default retention, or this proves nothing: %+v", bucket, c)
	}

	dst, err := s3backend.New(ctx, s3backend.Options{Bucket: bucket, Region: region, Endpoint: endpoint, UsePathStyle: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unique: nothing here can be deleted, so the keys of one run must never meet another's.
	repo := source.Repo{
		Host: "github.com", Owner: fmt.Sprintf("drill-%d", time.Now().UnixNano()), Name: "hello",
		CloneURL: initFixtureRepo(t), DefaultBranch: "main",
	}
	pubPEM, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := crypto.ParsePrivateKey(privPEM)
	pub, _ := crypto.ParsePublicKey(pubPEM)

	conf := config.Default()
	conf.Destination.S3.Bucket = bucket
	conf.Destination.Retention = config.RetentionConfig{Mode: "COMPLIANCE", Days: 1}
	conf.Source.Repo = repo.Slug()

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: conf, Source: &fixtureSource{repos: []source.Repo{repo}}, Dest: dst, Git: gitexec.New(nil),
		SigningKey: signer, ToolVersion: "itest", Now: time.Now,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	drill, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: dst, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
		Retention: conf.Destination.Retention, ToolVersion: "itest",
	}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if drill.ReportKey == "" {
		t.Fatal("no report was stored")
	}

	held := func(key string) (s3types.ObjectLockRetentionMode, time.Time, bool) {
		out, err := raw.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Errorf("%s holds no retention: %v", key, err)
			return "", time.Time{}, false
		}
		return out.Retention.Mode, aws.ToTime(out.Retention.RetainUntilDate), true
	}
	// The copy the report proves, as the baseline: COMPLIANCE, a day from the backup.
	for _, a := range res.Manifest.Repos[0].Artifacts {
		if mode, _, ok := held(a.Key); ok && mode != s3types.ObjectLockRetentionModeCompliance {
			t.Errorf("the artifact %s is held %s, want COMPLIANCE", a.Key, mode)
		}
	}
	want := drill.Report.FinishedAt.Add(24 * time.Hour)
	for _, key := range []string{drill.ReportKey, drill.ReportKey + ".sig"} {
		mode, until, ok := held(key)
		if !ok {
			continue
		}
		if mode != s3types.ObjectLockRetentionModeCompliance || until.Before(want.Add(-2*time.Second)) || until.After(want.Add(2*time.Second)) {
			t.Errorf("%s is held %s until %s; want COMPLIANCE until a day after the drill finished, %s", key, mode, until, want)
		}
	}
}
