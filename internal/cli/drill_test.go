package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/pipeline"
)

// The exit code is the whole contract for `drill`.
//
// The agent that runs this in the control plane takes the exit code as the verdict and refuses
// to read the report beside it, deliberately: a report claiming success next to a non-zero exit
// is a broken contract, and guessing which half to believe is how a reader ends up trusting the
// convenient one. That refusal only works if the code itself carries the distinction.
func TestTheDrillExitCodeSeparatesTheTwoFailures(t *testing.T) {
	stored := fmt.Errorf("%w: %w", pipeline.ErrReportNotStored, errors.New("access denied"))

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a clean drill", nil, 0},
		{"a repository did not come back", pipeline.ErrDrillFailures, 1},
		{"everything restored, the report was not filed", stored, 3},
		// Both at once. The restore failure wins, because exit 3 is a promise that the restores
		// passed and it has to hold every time it is issued.
		{"both", errors.Join(pipeline.ErrDrillFailures, stored), 1},
		// Anything else is a failure nobody has classified, and an unclassified failure is not
		// allowed to borrow exit 3's promise.
		{"an unrecognised failure", errors.New("locate manifest: no such key"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := drillExit(tc.err); got != tc.want {
				t.Errorf("drillExit(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// The summary line counts what came back, not what was attempted.
//
// It counted drilled repositories and called them restored, so a drill in which the only
// repository never restored ended "drill failed: all 1 repositories restored" - found on a
// real bucket, where a missing workdir failed every repository before a byte was read. The
// status word was right and the sentence after it said the opposite, on the line a person
// scrolls to the end of a thousand-repository drill to read.
func TestTheDrillSummaryCountsWhatCameBack(t *testing.T) {
	no := false
	failedEarly := pipeline.DrillRepo{Slug: "acme/api", Status: pipeline.StatusFailed, Error: "workdir: no such file"}
	cameBackUnmatched := pipeline.DrillRepo{Slug: "acme/web", Status: pipeline.StatusFailed, SourceMatch: &no,
		Error: "the restored repository does not carry the refs the source advertised when it was copied"}
	ok := func(slug string) pipeline.DrillRepo {
		return pipeline.DrillRepo{Slug: slug, Status: pipeline.StatusSuccess}
	}

	cases := []struct {
		name    string
		report  pipeline.DrillReport
		want    string
		mustNot string
	}{
		{
			name:    "nothing came back",
			report:  pipeline.DrillReport{Status: "failed", Eligible: 1, Drilled: 1, Repos: []pipeline.DrillRepo{failedEarly}},
			want:    "0 of 1 drilled repositories restored",
			mustNot: "all 1 repositories restored",
		},
		{
			name:   "some came back",
			report: pipeline.DrillReport{Status: "failed", Eligible: 3, Drilled: 3, Repos: []pipeline.DrillRepo{ok("a/1"), failedEarly, ok("a/3")}},
			want:   "2 of 3 drilled repositories restored",
		},
		{
			// The source comparison runs only after a clean restore, so a mismatch there is a
			// repository that came back and does not match. Counting it as not restored would
			// be the opposite mistake, and the control plane already made it once.
			name:   "came back but does not match its source",
			report: pipeline.DrillReport{Status: "failed", Eligible: 1, Drilled: 1, Repos: []pipeline.DrillRepo{cameBackUnmatched}},
			want:   "all 1 repositories restored",
		},
		{
			name:   "everything, unchanged",
			report: pipeline.DrillReport{Status: "success", Eligible: 3, Drilled: 3, Repos: []pipeline.DrillRepo{ok("a/1"), ok("a/2"), ok("a/3")}},
			want:   "all 3 repositories restored",
		},
		{
			// Still named as a sample. Ten of a thousand must not read as a thousand.
			name:   "a clean sample, unchanged",
			report: pipeline.DrillReport{Status: "success", Eligible: 1000, Drilled: 2, Repos: []pipeline.DrillRepo{ok("a/1"), ok("a/2")}},
			want:   "2 of 1000 repositories restored",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.report.ManifestKey, tc.report.ManifestSigned = "gitlab.com/acme/manifests/m.json", true
			out := captureStdout(t, func() { printDrill(&tc.report, true) })
			if !strings.Contains(out, tc.want) {
				t.Errorf("summary does not say %q:\n%s", tc.want, out)
			}
			if tc.mustNot != "" && strings.Contains(out, tc.mustNot) {
				t.Errorf("summary claims %q:\n%s", tc.mustNot, out)
			}
		})
	}
}

// The same sentence on the verify side, where only each repository's verdict is known.
func TestTheVerifiedDrillSummaryCountsWhatPassed(t *testing.T) {
	cases := []struct {
		name, want, mustNot string
		res                 pipeline.VerifyDrillResult
	}{
		{
			name:    "every repository failed",
			res:     pipeline.VerifyDrillResult{Status: "failed", Eligible: 1, Drilled: 1, Failures: []string{"acme/api: workdir"}},
			want:    "0 of 1 drilled repositories passed",
			mustNot: "restored",
		},
		{
			name: "clean, unchanged",
			res:  pipeline.VerifyDrillResult{Status: "success", Eligible: 4, Drilled: 4},
			want: "all 4 repositories restored",
		},
		{
			name: "a clean sample, unchanged",
			res:  pipeline.VerifyDrillResult{Status: "success", Eligible: 1000, Drilled: 10},
			want: "10 of 1000 repositories restored",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.res.ManifestKey = "gitlab.com/acme/manifests/m.json"
			got := drillVerifySummary(&tc.res)
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to say %q", got, tc.want)
			}
			if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
				t.Errorf("got %q, which claims %q", got, tc.mustNot)
			}
		})
	}
}
