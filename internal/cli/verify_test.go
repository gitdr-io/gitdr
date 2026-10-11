package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/pipeline"
)

// slowBucket is an S3 endpoint that answers GetObject, path-style, from a map of objects, and takes
// latency to answer each artifact. It counts the artifact reads it is answering at once and keeps the
// most it saw. The manifest and its signature are answered at once.
type slowBucket struct {
	*httptest.Server
	latency time.Duration
	objects map[string][]byte

	mu         sync.Mutex
	open, peak int
}

func newSlowBucket(t *testing.T, latency time.Duration, objects map[string][]byte) *slowBucket {
	t.Helper()
	b := &slowBucket{latency: latency, objects: objects}
	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Close)
	return b
}

func (b *slowBucket) serve(w http.ResponseWriter, r *http.Request) {
	key, inBucket := strings.CutPrefix(r.URL.Path, "/b/")
	body, ok := b.objects[key]
	if r.Method != http.MethodGet || !inBucket || !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
		return
	}
	if !strings.Contains(key, "/manifests/") {
		b.mu.Lock()
		b.open++
		b.peak = max(b.peak, b.open)
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			b.open--
			b.mu.Unlock()
		}()
		time.Sleep(b.latency)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// mostOpen is the most artifact reads the bucket answered at once.
func (b *slowBucket) mostOpen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// verifyConfig writes doctor's bucket config for verify against the S3 endpoint at url, with the
// public key at pubPath and extra appended, and leaves doctor's environment: static AWS keys and
// nothing else of AWS's or gitdr's, so neither the machine's AWS profile nor a GITDR_ variable
// changes the run.
func verifyConfig(t *testing.T, url, pubPath, extra string) string {
	t.Helper()
	doctorEnv(t)
	return bucketConfig(t, url, "manifest:\n  publicKeyPath: "+pubPath+"\n"+extra)
}

// verify reads as many artifacts at once as backup.concurrency lets a backup copy repositories, the
// setting an operator already sized for the store and the machine, and prints what it printed when
// it read one artifact at a time.
func TestVerifyReadsBackupConcurrencyArtifactsAtOnce(t *testing.T) {
	pubPEM, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(t.TempDir(), "manifest-signing.pub")
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// Twelve artifacts, and one whose stored bytes are not the ones the manifest signed.
	at := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	m := pipeline.Manifest{
		Schema: pipeline.ManifestSchema, RunID: "20260613T120000Z-000000000000",
		Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "test"},
		Source:      pipeline.SourceInfo{Type: "github", Host: "github.com"},
		Destination: pipeline.DestInfo{Type: "s3", Bucket: "b"},
		StartedAt:   at, FinishedAt: at, Status: pipeline.StatusSuccess,
	}
	objects := map[string][]byte{}
	for _, name := range []string{"a", "b", "c"} {
		e := pipeline.RepoEntry{Slug: "octo/" + name, Status: pipeline.StatusSuccess}
		for _, a := range []struct{ kind, ext string }{{"bundle", "bundle"}, {"lfs", "lfs.tar"}, {"meta", "meta.json"}, {"sha256", "sha256"}} {
			key := fmt.Sprintf("github.com/octo/%s/2026-06-13/%s.%s", name, name, a.ext)
			objects[key] = []byte("the bytes of " + key)
			e.Artifacts = append(e.Artifacts, pipeline.ArtifactInfo{
				Kind: a.kind, Key: key, Size: int64(len(objects[key])), SHA256: crypto.SHA256Bytes(objects[key]),
			})
		}
		m.Repos = append(m.Repos, e)
	}
	m.Repos[1].Artifacts[0].SHA256 = strings.Repeat("0", 64)
	canon, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	const key = "github.com/octo/manifests/20260613T120000Z.manifest.json"
	objects[key] = canon
	objects[key+".sig"] = []byte(base64.StdEncoding.EncodeToString(crypto.Sign(signer, canon)))

	const want = `{
  "manifestKey": "github.com/octo/manifests/20260613T120000Z.manifest.json",
  "signatureValid": true,
  "artifactsChecked": 12,
  "artifactsOk": 11,
  "failures": [
    "github.com/octo/b/2026-06-13/b.bundle: checksum mismatch"
  ]
}
`
	for _, tc := range []struct {
		name   string
		backup string // the config's backup block
		want   int    // the most artifacts read at once
	}{
		{"the default", "", 4},
		{"backup.concurrency 3", "backup:\n  concurrency: 3\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bucket := newSlowBucket(t, 50*time.Millisecond, objects)
			cfg := verifyConfig(t, bucket.URL, pubPath, tc.backup)
			var code int
			out := captureStdout(t, func() {
				_ = captureStderr(t, func() {
					code = Run(context.Background(), []string{"verify", "-manifest", key, "-config", cfg, "-output", "json"})
				})
			})
			if code != 1 {
				t.Errorf("exit %d, want 1 for the artifact that does not match", code)
			}
			if out != want {
				t.Errorf("verify printed:\n%s\nwant, as it printed reading one artifact at a time:\n%s", out, want)
			}
			if got := bucket.mostOpen(); got != tc.want {
				t.Errorf("%d artifacts read at once, want %d", got, tc.want)
			}
		})
	}
}
