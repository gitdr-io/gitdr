package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// refusingDest is a memDest whose WORM check answers with status, the way a store that declined
// the lock question does.
type refusingDest struct {
	*memDest
	status dest.WormStatus
}

func (d *refusingDest) VerifyWorm(context.Context) (dest.WormStatus, error) { return d.status, nil }

// askedSource is a fixtureSource that counts what it is asked.
type askedSource struct {
	fixtureSource
	asked atomic.Int32
}

func (s *askedSource) ListRepos(ctx context.Context, f source.Filter) ([]source.Repo, error) {
	s.asked.Add(1)
	return s.fixtureSource.ListRepos(ctx, f)
}

func (s *askedSource) CloneURL(ctx context.Context, r source.Repo) (string, error) {
	s.asked.Add(1)
	return s.fixtureSource.CloneURL(ctx, r)
}

func (s *askedSource) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	s.asked.Add(1)
	return s.fixtureSource.FetchMetadata(ctx, r)
}

// A bucket that does not exist is not a WORM question: there is nothing to write into. So the run
// stops at its WORM check, before it lists a repository or clones one, with worm.require or
// without, and says why. It used to warn that it could not read the bucket's immutability, clone
// every repository, and fail each one at its first upload.
//
// Every other answer keeps its meaning: a refusal that says anything else is a warning, and the
// run goes on, unless worm.require is set.
func TestAMissingBucketStopsTheRunBeforeAnyClone(t *testing.T) {
	repoDir := initFixtureRepo(t)
	_, privPEM, _ := crypto.GenerateKeyPair()
	signer, _ := crypto.ParsePrivateKey(privPEM)

	refusal := func(code string) error {
		return fmt.Errorf(`s3: list "": operation error S3: ListObjectsV2, api error %s`, code)
	}
	missing := dest.WormStatus{
		Verdict: dest.VerdictUnknown,
		Details: "could not verify immutability: the bucket answered NoSuchBucket",
		Refusal: fmt.Errorf("%w: %w", dest.ErrNoSuchBucket, refusal("NoSuchBucket")),
	}
	denied := dest.WormStatus{
		Verdict: dest.VerdictUnknown,
		Details: "could not verify immutability: the bucket answered AccessDenied",
		Refusal: refusal("AccessDenied"),
	}

	for _, tc := range []struct {
		name    string
		status  dest.WormStatus
		require bool
		stops   bool   // the run ends at the WORM check, with nothing asked of the source
		says    string // what its error says, when it stops
		absent  bool   // and that error is dest.ErrNoSuchBucket
	}{
		{"a missing bucket", missing, false, true, "the bucket does not exist", true},
		{"a missing bucket, worm.require", missing, true, true, "the bucket does not exist", true},
		{"another refusal", denied, false, false, "", false},
		{"another refusal, worm.require", denied, true, true, "refusing because worm.require is set", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &askedSource{fixtureSource: fixtureSource{repos: []source.Repo{
				{Host: "github.com", Owner: "octo", Name: "hello", CloneURL: repoDir, DefaultBranch: "main"},
			}}}
			dst := &refusingDest{memDest: newMemDest(false), status: tc.status}
			res, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
				Config: testConfig(), Source: src, Dest: dst, Git: gitexec.New(nil),
				SigningKey: signer, ToolVersion: "test", Now: fixedClock(), RequireWORM: tc.require,
			})

			if !tc.stops {
				if err != nil {
					t.Fatalf("a refusal that is not a missing bucket stopped the run without worm.require: %v", err)
				}
				if src.asked.Load() == 0 || len(dst.objs) == 0 {
					t.Errorf("the run asked the source %d times and wrote %d objects; it should have gone on", src.asked.Load(), len(dst.objs))
				}
				return
			}
			if err == nil {
				t.Fatalf("the run succeeded (manifest %s); want it stopped at the WORM check", res.ManifestKey)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
			if got := errors.Is(err, dest.ErrNoSuchBucket); got != tc.absent {
				t.Errorf("errors.Is(err, dest.ErrNoSuchBucket) = %v, want %v", got, tc.absent)
			}
			if res != nil {
				t.Errorf("a run stopped at the WORM check returned a result: %+v", res)
			}
			if n := src.asked.Load(); n != 0 {
				t.Errorf("the source was asked %d times before the run stopped; it must stop before listing a repository", n)
			}
			if len(dst.objs) != 0 {
				t.Errorf("%d objects written before the run stopped", len(dst.objs))
			}
		})
	}
}
