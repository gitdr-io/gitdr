package pipeline_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// heldDest is memDest that keeps the retention each object was written with.
type heldDest struct {
	*memDest
	wormErr error // VerifyWorm fails with it when set

	mu   sync.Mutex
	held map[string]dest.Retention
}

func newHeldDest(locked bool) *heldDest {
	return &heldDest{memDest: newMemDest(locked), held: map[string]dest.Retention{}}
}

func (h *heldDest) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	if h.wormErr != nil {
		return dest.WormStatus{}, h.wormErr
	}
	return h.memDest.VerifyWorm(ctx)
}

func (h *heldDest) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, ret dest.Retention) (dest.PutResult, error) {
	res, err := h.memDest.PutImmutable(ctx, key, r, size, ret)
	if err == nil {
		h.mu.Lock()
		h.held[key] = ret
		h.mu.Unlock()
	}
	return res, err
}

func (h *heldDest) retention(key string) dest.Retention {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[key]
}

// A drill report and its signature are locked like the copies they prove: the run's configured
// retention, from when the report is written, on a destination that confirms it is immutable, and
// none where backup sends none either.
//
// They were written with no retention at all. On a bucket with no default retention, Backblaze B2
// as gitdr's own e2e bucket is set up, the evidence that the backups restore could be deleted the
// moment it was written, beside artifacts nobody could touch for thirty days.
func TestADrillReportIsLockedLikeTheCopiesItProves(t *testing.T) {
	backupAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	drillAt := backupAt.Add(26 * time.Hour)
	for _, tc := range []struct {
		name    string
		locked  bool
		wormErr error
		want    dest.Retention // for the report and its signature, as for every artifact
	}{
		{"a destination that confirms it is immutable", true, nil,
			dest.Retention{Mode: dest.RetentionCompliance, Until: drillAt.Add(30 * 24 * time.Hour)}},
		{"a destination with no lock", false, nil, dest.Retention{}},
		{"a destination that will not say", true, errors.New("access denied"), dest.Retention{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			ctx := context.Background()
			src := &fixtureSource{repos: []source.Repo{{
				Host: "github.com", Owner: "octo", Name: "hello", CloneURL: initFixtureRepo(t), DefaultBranch: "main",
			}}}
			md := newHeldDest(tc.locked)
			pub, signer := drillKeys(t)
			cfg := testConfig() // COMPLIANCE, 30 days

			res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
				Config: cfg, Source: src, Dest: md, Git: gitexec.New(nil),
				SigningKey: signer, ToolVersion: "test", Now: func() time.Time { return backupAt },
			})
			if err != nil {
				t.Fatalf("backup: %v", err)
			}
			md.wormErr = tc.wormErr

			drill, err := pipeline.Drill(ctx, pipeline.DrillDeps{
				Dest: md, Git: gitexec.New(nil), PublicKey: pub, SigningKey: signer,
				Retention: cfg.Destination.Retention, ToolVersion: "test",
				Now: func() time.Time { return drillAt },
			}, pipeline.DrillRequest{ManifestKey: res.ManifestKey, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("drill: %v", err)
			}
			if drill.ReportKey == "" {
				t.Fatal("no report was stored")
			}
			for _, key := range []string{drill.ReportKey, drill.ReportKey + ".sig"} {
				if got := md.retention(key); got.Mode != tc.want.Mode || !got.Until.Equal(tc.want.Until) {
					t.Errorf("%s was written with %+v; want %+v", key, got, tc.want)
				}
			}
			// The same terms as the copies it proves, written a day earlier.
			if tc.locked && tc.wormErr == nil {
				for _, k := range keysOf(md.memDest) {
					if strings.HasSuffix(k, ".bundle") {
						if got := md.retention(k); got.Mode != tc.want.Mode || !got.Until.Equal(backupAt.Add(30*24*time.Hour)) {
							t.Errorf("the bundle %s was written with %+v", k, got)
						}
					}
				}
			}
		})
	}
}
