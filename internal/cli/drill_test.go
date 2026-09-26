package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
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
			key := "gitlab.com/acme/drills/d.drill.json"
			out := captureStdout(t, func() { printDrill(drillOutput{DrillReport: &tc.report, ReportKey: &key}) })
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

// sampleDrill is a finished drill of one repository, as the pipeline hands it back.
func sampleDrill(reportKey string) *pipeline.DrillResult {
	ts := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	match := true
	return &pipeline.DrillResult{
		Report: &pipeline.DrillReport{
			Schema: pipeline.DrillSchema, DrillID: "20260925T120000Z-a1b2c3d4e5f6",
			Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "test"},
			ManifestKey: "gitlab.com/acme/manifests/20260925T110000Z.manifest.json", ManifestSigned: true,
			StartedAt: ts, FinishedAt: ts.Add(time.Minute), Status: pipeline.StatusSuccess,
			Eligible: 1, Drilled: 1,
			Repos: []pipeline.DrillRepo{{
				Slug: "acme/api", Status: pipeline.StatusSuccess,
				SourceRefs: 3, BundleRefs: 3, RestoredRefs: 3, SourceMatch: &match,
			}},
		},
		ReportKey: reportKey,
	}
}

// Pins `drill --output json`.
//
// Every field the report already had stays at the top level, in order, where consumers read it.
// Two are appended: where the signed report was stored, which the hosted agent has been scraping
// out of the "drill report written" log line, and, when nothing was stored, why. `reportKey` is
// in every document, as null when there is no report, because absent is what an older engine
// prints and a consumer has to be able to tell the two apart.
func TestDrillJSONSaysWhereTheReportIs(t *testing.T) {
	const stored = "gitlab.com/acme/drills/20260925T120100Z.drill.json"
	for _, tc := range []struct {
		name       string
		res        *pipeline.DrillResult
		noReport   bool
		wantKey    any // the key, or nil for JSON null
		wantReason any // the reason, or nil for absent
	}{
		{name: "stored", res: sampleDrill(stored), wantKey: stored},
		{name: "-no-report", res: sampleDrill(""), noReport: true, wantReason: "no-report"},
		{name: "the destination refused the report", res: sampleDrill(""), wantReason: "store-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { emitDrill("json", drillOutputFor(tc.res, tc.noReport)) })
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, out)
			}
			for _, field := range []string{
				"schema", "drillId", "tool", "manifestKey", "manifestSigned",
				"startedAt", "finishedAt", "status", "eligible", "drilled", "repos",
			} {
				if _, ok := got[field]; !ok {
					t.Errorf("missing %q from drill --output json; that is a contract change", field)
				}
			}
			if got["schema"] != pipeline.DrillSchema {
				t.Errorf("schema = %v, want %v: the report is unchanged, so its version is too", got["schema"], pipeline.DrillSchema)
			}
			if strings.Index(out, `"reportKey"`) < strings.Index(out, `"repos"`) {
				t.Errorf("reportKey is not after the report's own fields:\n%s", out)
			}

			key, present := got["reportKey"]
			if !present {
				t.Fatal("reportKey is absent; it is the key, or null when there is no report")
			}
			if key != tc.wantKey {
				t.Errorf("reportKey = %v, want %v", key, tc.wantKey)
			}
			reason, present := got["reportNotWritten"]
			switch {
			case tc.wantReason == nil && present:
				t.Errorf("reportNotWritten = %v beside a stored report", reason)
			case tc.wantReason != nil && reason != tc.wantReason:
				t.Errorf("reportNotWritten = %v, want %v", reason, tc.wantReason)
			}
		})
	}
}

// The report is signed over its own JSON. If the key ever lands in those bytes, the signature
// covers a location instead of a document, and `verify -drill` fails on every copy of it.
func TestReportKeyStaysOutOfTheSignedReport(t *testing.T) {
	canon, err := json.Marshal(sampleDrill("gitlab.com/acme/drills/x.drill.json").Report)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"reportKey", "reportNotWritten"} {
		if strings.Contains(string(canon), field) {
			t.Errorf("%s is in the report's signed bytes:\n%s", field, canon)
		}
	}
}

// The human summary says where the evidence went, or that there is none and why.
func TestTheDrillSummarySaysWhereTheReportWent(t *testing.T) {
	const stored = "gitlab.com/acme/drills/20260925T120100Z.drill.json"
	for _, tc := range []struct {
		name          string
		res           *pipeline.DrillResult
		noReport      bool
		want, mustNot []string
	}{
		{
			name: "stored", res: sampleDrill(stored),
			want:    []string{"report: " + stored},
			mustNot: []string{"not stored", "no report was written"},
		},
		{
			name: "-no-report", res: sampleDrill(""), noReport: true,
			want:    []string{"no report was written", "-no-report", "wrote nothing"},
			mustNot: []string{"report: "},
		},
		{
			name: "the destination refused the report", res: sampleDrill(""),
			want:    []string{"this report was not stored"},
			mustNot: []string{"report: ", "-no-report"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { emitDrill("text", drillOutputFor(tc.res, tc.noReport)) })
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("summary does not say %q:\n%s", w, out)
				}
			}
			for _, m := range tc.mustNot {
				if strings.Contains(out, m) {
					t.Errorf("summary says %q:\n%s", m, out)
				}
			}
		})
	}
}

// -no-report needs the public key and never loads the private one.
//
// The auditor re-running a drill has the public key and a read credential. Requiring the signing
// key would mean handing them the one secret that can forge evidence; reading it when it happens
// to be configured would put it in memory for a command that has no use for it.
func TestADrillWithoutAReportNeverLoadsTheSigningKey(t *testing.T) {
	pubPEM, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pub, priv, missing := filepath.Join(dir, "public.pem"), filepath.Join(dir, "signing.pem"), filepath.Join(dir, "absent.pem")
	if err := os.WriteFile(pub, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priv, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		noReport   bool
		pub, priv  string // configured paths; empty is not configured
		wantFailed string // the key named in the log line; empty is no error
		wantSigner bool
	}{
		{name: "-no-report with only the public key", noReport: true, pub: pub},
		{name: "-no-report does not read a configured signing key", noReport: true, pub: pub, priv: priv},
		{name: "-no-report does not trip over a missing one", noReport: true, pub: pub, priv: missing},
		{name: "-no-report still needs the public key", noReport: true, priv: priv, wantFailed: "public key"},
		{name: "a report needs the signing key", pub: pub, wantFailed: "signing key"},
		{name: "a report with both keys", pub: pub, priv: priv, wantSigner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Manifest.PublicKeyPath, cfg.Manifest.SigningKeyPath = tc.pub, tc.priv

			gotPub, signer, failed, err := drillKeys(cfg, tc.noReport)
			if tc.wantFailed != "" {
				if err == nil || failed != tc.wantFailed {
					t.Fatalf("failed = %q, err = %v; want a %s error", failed, err, tc.wantFailed)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", failed, err)
			}
			if gotPub == nil {
				t.Error("no public key, so the manifest's signature would go unchecked")
			}
			if (signer != nil) != tc.wantSigner {
				t.Errorf("signing key loaded: %v, want %v", signer != nil, tc.wantSigner)
			}
		})
	}
}
