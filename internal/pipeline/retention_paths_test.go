package pipeline_test

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// partsLockDest is a store that writes an object past threshold in parts, and honours the lock
// headers on a single PUT but drops them when it completes an upload in parts, if dropOnComplete.
// That is the open question about B2, whose CreateMultipartUpload documents no lock headers. It
// answers truthfully what each object holds, and records what it was asked about.
type partsLockDest struct {
	*memDest
	threshold      int64
	dropOnComplete bool

	mu       sync.Mutex
	inParts  map[string]bool
	retained map[string]bool
	asked    []string
}

func (d *partsLockDest) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	res, err := d.memDest.PutImmutable(ctx, key, r, size, ret)
	if err == nil {
		d.mu.Lock()
		d.inParts[key] = size > d.threshold
		dropped := d.inParts[key] && d.dropOnComplete
		d.retained[key] = !ret.Until.IsZero() && !dropped
		d.mu.Unlock()
	}
	return res, err
}

func (d *partsLockDest) ObserveRetention(_ context.Context, key string) (dest.RetentionObservation, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, key)
	if d.retained[key] {
		return dest.RetentionPresent, time.Now().Add(24 * time.Hour), nil
	}
	return dest.RetentionAbsent, time.Time{}, nil
}

// A run asks what retention landed on an object of each write path it used, and a store that
// drops the lock on the objects written in parts reads absent.
//
// The run asked about the first object it wrote only. With a small repository first, that was a
// single PUT, so a store that honoured the lock headers on PutObject and dropped them on every
// upload in parts read present, and --require-worm passed a run whose largest objects nothing
// held. It now asks about the smallest object it wrote and the largest.
func TestRetentionIsObservedOnEachWritePath(t *testing.T) {
	small := initFixtureRepo(t)
	big := initFixtureRepo(t)
	blob := make([]byte, 2<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, big, "note.txt", "big") // takes the blob with it, so the bundle is past the threshold
	repos := []source.Repo{
		{Host: "github.com", Owner: "octo", Name: "a-small", CloneURL: small, DefaultBranch: "main"},
		{Host: "github.com", Owner: "octo", Name: "b-big", CloneURL: big, DefaultBranch: "main"},
	}
	const threshold = 1 << 20

	for _, tc := range []struct {
		name           string
		dropOnComplete bool
		want           dest.RetentionObservation
		wantVerdict    string
	}{
		{"a store that drops the lock on Complete", true, dest.RetentionAbsent, "not-immutable"},
		{"a store that keeps it", false, dest.RetentionPresent, "immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(t *testing.T, requireWORM bool) (*partsLockDest, *pipeline.BackupResult, error) {
				t.Helper()
				t.Chdir(t.TempDir())
				md := &partsLockDest{memDest: newMemDest(true), threshold: threshold, dropOnComplete: tc.dropOnComplete,
					inParts: map[string]bool{}, retained: map[string]bool{}}
				cfg := testConfig()
				cfg.Source.Repo = ""
				res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
					Config: cfg, Source: &fixtureSource{repos: repos}, Dest: md, Git: gitexec.New(nil),
					SigningKey: testSigner(t), ToolVersion: "test", Now: fixedClock(), RequireWORM: requireWORM,
				})
				return md, res, err
			}

			md, res, err := run(t, false)
			if err != nil {
				t.Fatalf("backup: %v", err)
			}
			d := res.Manifest.Destination
			if d.RetentionObserved != string(tc.want) || d.WormVerdict != tc.wantVerdict || d.WormImmutable != (tc.wantVerdict == "immutable") {
				t.Errorf("the manifest says retentionObserved %s, wormVerdict %s, wormImmutable %v; want %s and %s",
					d.RetentionObserved, d.WormVerdict, d.WormImmutable, tc.want, tc.wantVerdict)
			}
			var paths []bool
			for _, key := range md.asked {
				paths = append(paths, md.inParts[key])
			}
			if !slices.Equal(paths, []bool{false, true}) || !strings.HasSuffix(md.asked[1], "b-big.bundle") {
				t.Errorf("the store was asked about %q, want an object written in one PUT and then b-big's bundle, written in parts", md.asked)
			}

			_, _, err = run(t, true)
			refused := err != nil && strings.Contains(err.Error(), "applied no retention")
			switch {
			case tc.want == dest.RetentionAbsent && !refused:
				t.Errorf("--require-worm passed a run whose objects written in parts hold no retention: %v", err)
			case tc.want != dest.RetentionAbsent && err != nil:
				t.Errorf("--require-worm refused a run the store locked on both paths: %v", err)
			}
		})
	}
}
