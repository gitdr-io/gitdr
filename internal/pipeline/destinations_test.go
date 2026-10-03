package pipeline_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/fsouza/fake-gcs-server/fakestorage"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/dest"
	azurebackend "gitdr.io/gitdr/internal/dest/azure"
	gcsbackend "gitdr.io/gitdr/internal/dest/gcs"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// The manifest's destination block, written by a run against Cloud Storage and against Azure.
//
// Up to v0.1.20 the block was S3's wherever the copies went. `bucket` named the S3 bucket, so it
// was empty off S3, and `wormMode` was the configured mode, which gitdr sends to S3 alone. A locked
// GCS bucket therefore signed `COMPLIANCE`, a mode Google was never asked for and does not have,
// and an Azure manifest recorded every artifact as 0 bytes.

// gcsPolicy is a bucket retention policy. fake-gcs-server cannot hold one, so gcsEmulator adds it
// to the emulator's answers the way Cloud Storage reports it.
type gcsPolicy struct {
	period time.Duration
	locked bool
}

// gcsEmulator serves fake-gcs-server over HTTP and returns the endpoint gcs.New takes. With a
// policy, the bucket's metadata carries it, and every object carries the retention expiration
// time Cloud Storage gives an object under it: its creation time plus the period.
//
// The emulator keeps the objects and answers every request. Only the JSON it sends back gains the
// two fields, so the SDK parses the policy exactly as it parses Google's.
func gcsEmulator(t *testing.T, bucket string, policy *gcsPolicy) string {
	t.Helper()
	fake, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true, PublicHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Stop)
	fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		fake.HTTPHandler().ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if policy != nil && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			body = withRetention(t, body, *policy)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/storage/v1/"
}

// withRetention adds policy to a JSON answer about the bucket or its objects.
func withRetention(t *testing.T, body []byte, policy gcsPolicy) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return body
	}
	expire := func(obj map[string]any) {
		created, err := time.Parse(time.RFC3339Nano, fmt.Sprint(obj["timeCreated"]))
		if err == nil {
			obj["retentionExpirationTime"] = created.Add(policy.period).UTC().Format(time.RFC3339Nano)
		}
	}
	switch doc["kind"] {
	case "storage#bucket":
		doc["retentionPolicy"] = map[string]any{
			"retentionPeriod": strconv.FormatInt(int64(policy.period/time.Second), 10),
			"isLocked":        policy.locked,
			"effectiveTime":   "2026-01-01T00:00:00Z",
		}
	case "storage#object":
		expire(doc)
	case "storage#objects":
		items, _ := doc["items"].([]any)
		for _, item := range items {
			if obj, ok := item.(map[string]any); ok {
				expire(obj)
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode the emulator's answer: %v", err)
	}
	return out
}

// gcsConfig is the configuration of a run of the fixture repository into a bucket gcsEmulator serves.
func gcsConfig(bucket, endpoint string) *config.Config {
	c := config.Default()
	c.Source.Repo = "octo/hello"
	c.Destination.Type = "gcs"
	c.Destination.GCS.Bucket = bucket
	c.Destination.GCS.Endpoint = endpoint
	return c
}

// The bucket a GCS run wrote to is named in its manifest, and the mode gitdr never sent is not.
func TestAGCSManifestNamesItsBucketAndClaimsNoMode(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	const bucket = "gitdr-locked"
	endpoint := gcsEmulator(t, bucket, &gcsPolicy{period: 24 * time.Hour, locked: true})

	dst, err := gcsbackend.New(ctx, gcsbackend.Options{Bucket: bucket, Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: initFixtureRepo(t), DefaultBranch: "main",
	}}}
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: gcsConfig(bucket, endpoint), Source: src, Dest: dst, Git: gitexec.New(nil),
		SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	d := res.Manifest.Destination
	if d.Bucket != bucket {
		t.Errorf("destination.bucket = %q, want %q: the manifest does not say where its copies are", d.Bucket, bucket)
	}
	if d.WormMode != "" {
		t.Errorf("destination.wormMode = %q on GCS, where gitdr sets no mode on any object", d.WormMode)
	}
	canon, err := res.Manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canon, []byte(`"wormMode"`)) {
		t.Errorf("the signed manifest carries a wormMode on GCS: %s", canon)
	}
	// The run saw what a locked bucket reports, and says it in its own words.
	if d.WormVerdict != "immutable" || d.RetentionObserved != string(dest.RetentionPresent) {
		t.Errorf("wormVerdict %q, retentionObserved %q: the emulated bucket was not read as locked (%s)",
			d.WormVerdict, d.RetentionObserved, d.WormDetails)
	}
	if want := "bucket retention policy Locked, 1 day"; d.WormDetails != want {
		t.Errorf("wormDetails = %q, want %q", d.WormDetails, want)
	}
}

// azuriteConnectionString locates an Azurite to run against, or skips, the way the Azure backend's
// own tests do: in CI a missing emulator fails instead, because a skipped test reads as a pass.
func azuriteConnectionString(t *testing.T) string {
	t.Helper()
	if cs := os.Getenv("AZURE_STORAGE_CONNECTION_STRING"); cs != "" {
		return cs
	}
	if endpoint := os.Getenv("AZURITE_BLOB_ENDPOINT"); endpoint != "" {
		// Azurite's well-known development account, published in its own documentation. It
		// authenticates nothing but a local emulator.
		return "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;" +
			"AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;" +
			"BlobEndpoint=" + strings.TrimSuffix(endpoint, "/") + "/devstoreaccount1;"
	}
	if os.Getenv("CI") != "" {
		t.Fatal("no Azurite endpoint set; in CI the Azure tests must run, not skip")
	}
	t.Skip("set AZURITE_BLOB_ENDPOINT (e.g. http://127.0.0.1:10000) to run the Azure integration tests")
	return ""
}

// An Azure run names its container, and records each blob at the size it was stored.
//
// A container of its own for every run: the keys are fixed by the run's date, the destination
// never deletes, and Azurite keeps what earlier runs wrote.
func TestAnAzureManifestNamesItsContainerAndTheSizesItStored(t *testing.T) {
	cs := azuriteConnectionString(t)
	t.Chdir(t.TempDir())
	ctx := context.Background()

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	container := "gitdr-manifest-" + hex.EncodeToString(suffix[:])
	raw, err := azblob.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.CreateContainer(ctx, container, nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	dst, err := azurebackend.New(ctx, azurebackend.Options{Container: container, ConnectionString: cs}, nil)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Source.Repo = "octo/hello"
	cfg.Destination.Type = "azure"
	cfg.Destination.Azure.Container = container
	src := &fixtureSource{repos: []source.Repo{{
		Host: "github.com", Owner: "octo", Name: "hello", CloneURL: initFixtureRepo(t), DefaultBranch: "main",
	}}}
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: cfg, Source: src, Dest: dst, Git: gitexec.New(nil),
		SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	d := res.Manifest.Destination
	if d.Bucket != container {
		t.Errorf("destination.bucket = %q, want the container %q", d.Bucket, container)
	}
	if d.WormMode != "" {
		t.Errorf("destination.wormMode = %q on Azure, where gitdr sets no mode on any blob", d.WormMode)
	}

	stored, err := dst.List(ctx, "github.com/octo/hello/")
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, o := range stored {
		sizes[o.Key] = o.Size
	}
	artifacts := res.Manifest.Repos[0].Artifacts
	if len(artifacts) == 0 {
		t.Fatal("the run recorded no artifacts")
	}
	for _, a := range artifacts {
		if a.Size <= 0 || a.Size != sizes[a.Key] {
			t.Errorf("%s: the manifest says %d bytes, Azure stored %d", a.Key, a.Size, sizes[a.Key])
		}
	}
}
