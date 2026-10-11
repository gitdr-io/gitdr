package pipeline_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/pipeline"
)

// verifyKey is where these tests file the manifest they verify.
const verifyKey = "github.com/octo/manifests/20260613T120000Z.manifest.json"

// verifyRun records a run of the named repositories, each with a bundle, an LFS archive, its
// metadata and its checksum sidecar, and stores every artifact in md with the checksum the record
// gives it. It returns the run and the artifact keys in the manifest's order. The manifest itself is
// not stored, so a test can change the record first and then sign it with storeSigned.
func verifyRun(md *memDest, names ...string) (*pipeline.Manifest, []string) {
	at := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	m := &pipeline.Manifest{
		Schema: pipeline.ManifestSchema, RunID: "20260613T120000Z-000000000000",
		Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "test"},
		Source:      pipeline.SourceInfo{Type: "github", Host: "github.com"},
		Destination: pipeline.DestInfo{Type: "s3", Bucket: "test-bucket"},
		StartedAt:   at, FinishedAt: at, Status: pipeline.StatusSuccess,
	}
	var keys []string
	for _, name := range names {
		e := pipeline.RepoEntry{Slug: "octo/" + name, Status: pipeline.StatusSuccess}
		for _, a := range []struct{ kind, ext string }{{"bundle", "bundle"}, {"lfs", "lfs.tar"}, {"meta", "meta.json"}, {"sha256", "sha256"}} {
			key := fmt.Sprintf("github.com/octo/%s/2026-06-13/%s.%s", name, name, a.ext)
			body := []byte("the bytes of " + key)
			md.objs[key] = body
			e.Artifacts = append(e.Artifacts, pipeline.ArtifactInfo{
				Kind: a.kind, Key: key, Size: int64(len(body)), SHA256: crypto.SHA256Bytes(body), RetainUntil: at.AddDate(0, 0, 30),
			})
			keys = append(keys, key)
		}
		m.Repos = append(m.Repos, e)
	}
	return m, keys
}

// storeSigned stores m and its signature at verifyKey, as a backup stores them.
func storeSigned(t *testing.T, md *memDest, signer ed25519.PrivateKey, m *pipeline.Manifest) {
	t.Helper()
	canon, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	md.objs[verifyKey] = canon
	md.objs[verifyKey+".sig"] = []byte(base64.StdEncoding.EncodeToString(crypto.Sign(signer, canon)))
}

// throttledDest is a store that is slow to answer, as one at the end of a long link is, or one behind
// the scale harness's throttling proxy. Each Get waits latency(key) before it answers. It counts the
// reads open at once, from the Get to the Close of what it returned, and keeps the most it saw.
type throttledDest struct {
	dest.Destination
	latency func(key string) time.Duration

	mu         sync.Mutex
	open, peak int
}

func (d *throttledDest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	d.mu.Lock()
	d.open++
	d.peak = max(d.peak, d.open)
	d.mu.Unlock()
	wait := time.NewTimer(d.latency(key))
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		d.closed()
		return nil, ctx.Err()
	}
	rc, err := d.Destination.Get(ctx, key)
	if err != nil {
		d.closed()
		return nil, err
	}
	return &openRead{ReadCloser: rc, done: d.closed}, nil
}

func (d *throttledDest) closed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open--
}

// mostOpen is the most reads that were open at once.
func (d *throttledDest) mostOpen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak
}

// openRead is a read throttledDest counts as open until it is closed.
type openRead struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (r *openRead) Close() error {
	r.once.Do(r.done)
	return r.ReadCloser.Close()
}

// Verify reads up to Concurrency artifacts at once, and never more.
//
// It read them one after another, so it took the sum of every read, and against a store at the end
// of a slow link that sum is mostly waiting: a run that copied thousands of repositories was read
// back one artifact at a time, waiting on each in turn.
func TestVerifyReadsUpToConcurrencyArtifactsAtOnce(t *testing.T) {
	ctx := context.Background()
	signer := testSigner(t)
	pub := signer.Public().(ed25519.PublicKey)

	// Sixteen artifacts, each answered latency after it is asked for. The manifest and its signature
	// are answered at once: they are read before any artifact, one after the other.
	slowly := func(latency time.Duration) func(string) time.Duration {
		return func(key string) time.Duration {
			if strings.HasPrefix(key, verifyKey) {
				return 0
			}
			return latency
		}
	}
	verify := func(t *testing.T, latency time.Duration, concurrency int) (*throttledDest, time.Duration) {
		t.Helper()
		md := newMemDest(true)
		m, keys := verifyRun(md, "a", "b", "c", "d")
		storeSigned(t, md, signer, m)
		store := &throttledDest{Destination: md, latency: slowly(latency)}

		start := time.Now()
		res, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: store, PublicKey: pub, Concurrency: concurrency}, verifyKey)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if res.ArtifactsChecked != len(keys) || res.ArtifactsOK != len(keys) {
			t.Fatalf("artifacts %d/%d ok, want %d/%d", res.ArtifactsOK, res.ArtifactsChecked, len(keys), len(keys))
		}
		return store, took
	}

	for _, tc := range []struct {
		name        string
		concurrency int
		want        int // the most reads open at once
	}{
		// What a caller that sets nothing gets: the reads verify always made, one at a time.
		{"unset", 0, 1},
		{"two", 2, 2},
		{"the default backup.concurrency", 4, 4},
		// More than there are artifacts reads every one of them at once.
		{"more than the artifacts", 64, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := verify(t, 20*time.Millisecond, tc.concurrency)
			if got := store.mostOpen(); got != tc.want {
				t.Errorf("%d reads open at once, want %d", got, tc.want)
			}
		})
	}

	// The time, against a store that takes 250 ms to answer each read. Waiting is all a read does
	// here, so one at a time cannot take less than sixteen of those, and four at a time takes about
	// four. A third of sixteen leaves room for a slow machine, and none for reading three at a time,
	// which takes six.
	//
	// The room is a third of a read for each of the four turns. A CI pod held to its CPU limit is
	// paused for the rest of each 100 ms period once it has spent its quota there, so a read takes
	// 250 ms, which leaves 83 ms a turn for a pause; at 100 ms it would leave 33.
	t.Run("four at a time is four times faster", func(t *testing.T) {
		const latency = 250 * time.Millisecond
		oneAtATime := 16 * latency
		_, took := verify(t, latency, 4)
		t.Logf("16 reads of %s each, 4 at a time: %s; one at a time takes at least %s, %.1f times as long",
			latency, took.Round(time.Millisecond), oneAtATime, float64(oneAtATime)/float64(took))
		if took > oneAtATime/3 {
			t.Errorf("took %s, want under %s: a third of the %s the reads take one at a time", took, oneAtATime/3, oneAtATime)
		}
	})
}

// Verify reports what it found in the manifest's order, whatever order the reads finish in. So its
// result, and the JSON `verify --output json` prints from it, is the one it gave when it read one
// artifact at a time; want below is what it printed then.
func TestVerifyReportsInTheManifestsOrder(t *testing.T) {
	ctx := context.Background()
	signer := testSigner(t)
	pub := signer.Public().(ed25519.PublicKey)

	md := newMemDest(true)
	m, keys := verifyRun(md, "a", "b", "c")
	// Four failures, the first artifact and the last among them, and a checksum recorded in
	// capitals, which matches.
	m.Repos[0].Artifacts[0].SHA256 = strings.Repeat("0", 64)                         // a.bundle: not what was stored
	delete(md.objs, keys[2])                                                         // a.meta.json: not there
	m.Repos[1].Artifacts[1].SHA256 = strings.Repeat("f", 64)                         // b.lfs.tar: not what was stored
	m.Repos[2].Artifacts[2].SHA256 = strings.ToUpper(m.Repos[2].Artifacts[2].SHA256) // c.meta.json: matches
	delete(md.objs, keys[11])                                                        // c.sha256: not there
	storeSigned(t, md, signer, m)

	// Each artifact's read takes 5 ms less than the one before it, so of any reads that run
	// together the later one in the manifest finishes first: from three at a time, a.meta.json's
	// before a.bundle's.
	wait := map[string]time.Duration{}
	for i, k := range keys {
		wait[k] = time.Duration(len(keys)-i) * 5 * time.Millisecond
	}

	const want = `{
  "manifestKey": "github.com/octo/manifests/20260613T120000Z.manifest.json",
  "signatureValid": true,
  "artifactsChecked": 12,
  "artifactsOk": 8,
  "failures": [
    "github.com/octo/a/2026-06-13/a.bundle: checksum mismatch",
    "github.com/octo/a/2026-06-13/a.meta.json: not found: github.com/octo/a/2026-06-13/a.meta.json",
    "github.com/octo/b/2026-06-13/b.lfs.tar: checksum mismatch",
    "github.com/octo/c/2026-06-13/c.sha256: not found: github.com/octo/c/2026-06-13/c.sha256"
  ]
}`
	for _, concurrency := range []int{1, 3, 4, len(keys)} {
		t.Run(fmt.Sprint(concurrency, " at a time"), func(t *testing.T) {
			store := &throttledDest{Destination: md, latency: func(k string) time.Duration { return wait[k] }}
			res, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: store, PublicKey: pub, Concurrency: concurrency}, verifyKey)
			if err == nil || err.Error() != "verify: 4 artifact failure(s)" {
				t.Errorf("err = %v, want verify: 4 artifact failure(s)", err)
			}
			// As `verify --output json` prints it.
			got, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("verify printed:\n%s\nwant, as it printed reading one artifact at a time:\n%s", got, want)
			}
		})
	}
}

// A verify stopped part way fails, counts nothing it did not read as verified, and names every
// artifact, in the manifest's order, as it did reading one at a time: each read asked for after the
// stop fails at once against a store that honours it.
func TestVerifyStoppedNamesEveryArtifactItDidNotRead(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	signer := testSigner(t)
	pub := signer.Public().(ed25519.PublicKey)

	md := newMemDest(true)
	m, keys := verifyRun(md, "a", "b", "c", "d")
	storeSigned(t, md, signer, m)
	// The first artifact asked for stops the verify, and no read answers before the stop.
	store := &throttledDest{Destination: md, latency: func(key string) time.Duration {
		if strings.HasPrefix(key, verifyKey) {
			return 0
		}
		stop()
		return time.Hour
	}}

	res, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: store, PublicKey: pub, Concurrency: 4}, verifyKey)
	if err == nil {
		t.Error("a stopped verify passed")
	}
	if res.ArtifactsChecked != len(keys) || res.ArtifactsOK != 0 {
		t.Errorf("artifacts %d/%d ok, want 0/%d: none was read", res.ArtifactsOK, res.ArtifactsChecked, len(keys))
	}
	want := make([]string, len(keys))
	for i, k := range keys {
		want[i] = k + ": " + context.Canceled.Error()
	}
	if !slices.Equal(res.Failures, want) {
		t.Errorf("failures:\n%s\nwant every artifact, in the manifest's order:\n%s",
			strings.Join(res.Failures, "\n"), strings.Join(want, "\n"))
	}
}

// pattern is what every generated artifact holds, repeated to its size.
var pattern = func() []byte {
	b := make([]byte, 64<<10)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}()

// generated is size bytes of pattern, made as they are read, so a test can serve an artifact of
// any size without holding it.
type generated struct{ off, size int64 }

func (g *generated) Read(p []byte) (int, error) {
	if g.off >= g.size {
		return 0, io.EOF
	}
	n := copy(p[:min(int64(len(p)), g.size-g.off)], pattern[g.off%int64(len(pattern)):])
	g.off += int64(n)
	return n, nil
}

// generatedDest serves each key in sizes as that many bytes of pattern, and every other key from
// the store it wraps.
type generatedDest struct {
	dest.Destination
	sizes map[string]int64
}

func (d *generatedDest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if n, ok := d.sizes[key]; ok {
		return io.NopCloser(&generated{size: n}), nil
	}
	return d.Destination.Get(ctx, key)
}

// Verify holds no artifact in memory, however many it reads at once. Each read streams the object
// through SHA-256, so what verify allocates is a copy buffer for each read and not the artifact:
// eight artifacts of 32 MiB, four at a time, take less than one of them would.
func TestVerifyHoldsNoArtifactInMemory(t *testing.T) {
	ctx := context.Background()
	signer := testSigner(t)
	pub := signer.Public().(ed25519.PublicKey)

	const size = 32 << 20
	sum, _, err := crypto.SHA256Hex(&generated{size: size})
	if err != nil {
		t.Fatal(err)
	}
	md := newMemDest(true)
	m, keys := verifyRun(md, "a", "b")
	sizes := map[string]int64{}
	for i := range m.Repos {
		for j := range m.Repos[i].Artifacts {
			a := &m.Repos[i].Artifacts[j]
			a.Size, a.SHA256 = size, sum
			sizes[a.Key] = size
		}
	}
	storeSigned(t, md, signer, m)
	store := &generatedDest{Destination: md, sizes: sizes}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: store, PublicKey: pub, Concurrency: 4}, verifyKey)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.ArtifactsOK != len(keys) {
		t.Fatalf("artifacts %d/%d ok, want %d", res.ArtifactsOK, res.ArtifactsChecked, len(keys))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("verify allocated %.1f MiB reading %d artifacts of %d MiB", float64(allocated)/(1<<20), len(keys), size>>20)
	if allocated >= size {
		t.Errorf("verify allocated %.1f MiB reading %d artifacts of %d MiB: at least one of them was held whole",
			float64(allocated)/(1<<20), len(keys), size>>20)
	}
}
