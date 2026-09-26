package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

// `gitdr drill` — restore the backups and prove what came back.
//
// `verify` reads the artifacts back and rechecks their checksums against the signed manifest.
// That answers "is the copy intact", which is SOC 2 A1.2. It does not answer "does it restore",
// which is A1.3, and blurring the two is the thing this product refuses to do. A drill actually
// restores, and compares the result against both the bundle's own header and the ref map the
// source advertised when the copy was made.
//
// It writes a signed report beside the manifest it drilled, through the same create-only path
// as everything else, so the evidence is as immutable as the thing it proves.
//
// With -no-report it writes nothing at all. That is the drill an auditor re-runs from the
// customer's own bucket: a read credential and the public key, no private key, no gitdr account,
// and the same checks.
func runDrill(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	common := registerCommon(fs)
	manifest := fs.String("manifest", "", "manifest object key to drill; defaults to the most recent")
	host := fs.String("host", "", "source host, e.g. github.com; needed when -manifest is not given")
	owner := fs.String("owner", "", "organisation; needed when -manifest is not given")
	sample := fs.Int("sample", 0, "restore at most this many repositories, in slug order; 0 means all")
	workdir := fs.String("workdir", "", "where to restore; defaults to the system temp directory")
	noReport := fs.Bool("no-report", false, "write nothing to the bucket: no report is signed or stored, so only a read credential and the public key are needed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" && (*host == "" || *owner == "") {
		fmt.Fprintln(os.Stderr, "drill: give -manifest, or -host and -owner to drill the most recent one")
		return 2
	}

	cfg, log, err := common.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		log.Error("invalid config", "err", err)
		return 1
	}
	dst, err := buildDest(ctx, cfg, log)
	if err != nil {
		log.Error("destination", "err", err)
		return 1
	}

	pub, signer, failed, err := drillKeys(cfg, *noReport)
	if err != nil {
		log.Error(failed, "err", err)
		return 1
	}

	encKey, err := resolveEncryptionKey(cfg)
	if err != nil {
		log.Error("encryption key", "err", err)
		return 1
	}

	// No signing key is the whole of -no-report. The pipeline stores a report only when it can
	// sign one, so there is one read-only path, not a second one beside it.
	res, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: dst, Git: gitexec.New(log), EncryptionKey: encKey,
		PublicKey: pub, SigningKey: signer,
		ToolVersion: version(), Logger: log,
	}, pipeline.DrillRequest{
		ManifestKey: *manifest, Host: *host, Owner: *owner,
		Sample: *sample, WorkDir: *workdir,
	})

	if res != nil {
		emitDrill(common.output, drillOutputFor(res, *noReport))
	}
	if err != nil {
		log.Error("drill", "err", err)
	}
	return drillExit(err)
}

// drillKeys resolves the keys a drill needs, and names the one that failed for the log line.
//
// The public key is required either way: it verifies the manifest being drilled, and a drill of
// a manifest nobody can attribute to gitdr proves the wrong thing. The private key signs the
// report, so it is required only when there will be one. Under -no-report it is not read at all,
// even when the config names it: the auditor's copy of a config should not have to carry the one
// secret that can forge evidence, and a key that is never loaded cannot leak.
func drillKeys(cfg *config.Config, noReport bool) (ed25519.PublicKey, ed25519.PrivateKey, string, error) {
	pubPEM, err := cfg.ResolveManifestPublicKey()
	if err != nil {
		return nil, nil, "public key", err
	}
	pub, err := crypto.ParsePublicKey(pubPEM)
	if err != nil {
		return nil, nil, "public key", err
	}
	if noReport {
		return pub, nil, "", nil
	}
	privPEM, err := cfg.ResolveManifestSigningKey()
	if err != nil {
		return nil, nil, "signing key", err
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		return nil, nil, "signing key", err
	}
	return pub, signer, "", nil
}

// drillOutput is the JSON shape of a drill: the report's own fields, then where it was stored.
//
// The same split as backupOutput. The report is signed over its exact bytes and must not name its
// own location, so the key sits beside it rather than in it. Embedded, so the report's fields
// stay at the top level in their existing order and a reader written before this sees what it
// saw before.
//
// This is not the signed document. `verify -drill` checks the stored bytes; never check a
// signature against stdout.
type drillOutput struct {
	*pipeline.DrillReport
	// ReportKey is the object key the signed report was stored under, the value `verify -drill`
	// takes. Null, never absent, when no report was written: absent means an engine too old to
	// say, which is a different answer.
	ReportKey *string `json:"reportKey"`
	// ReportNotWritten says why ReportKey is null, and is present only then. One of the two
	// constants below, compared whole.
	ReportNotWritten string `json:"reportNotWritten,omitempty"`
}

const (
	// reportNotRequested: the drill ran with -no-report. It read the bucket, wrote nothing to it
	// and signed nothing, so stdout is the only record and it is not signed.
	reportNotRequested = "no-report"
	// reportStoreFailed: the destination did not accept the report. Exit 3 when every
	// repository came back, 1 when one did not.
	reportStoreFailed = "store-failed"
)

// drillOutputFor says where the report went, or why it went nowhere.
//
// The pipeline returns a key only for a report it stored. Without one there are two causes: this
// command withheld the signing key because of -no-report, or the destination refused the write.
func drillOutputFor(res *pipeline.DrillResult, noReport bool) drillOutput {
	out := drillOutput{DrillReport: res.Report}
	switch {
	case res.ReportKey != "":
		key := res.ReportKey
		out.ReportKey = &key
	case noReport:
		out.ReportNotWritten = reportNotRequested
	default:
		out.ReportNotWritten = reportStoreFailed
	}
	return out
}

// The drill's error, as a process exit code.
//
// A repository that did not come back outranks a report that was not filed, so exit 3 always
// means the restores passed and only the evidence is missing. Both can be true at once: the
// pipeline joins them rather than returning the first, because returning the store failure
// early reported a broken backup as a storage problem.
//
// Kept pure and separate so the mapping is testable without a destination, keys and a config,
// and so the pipeline never learns what a process exit code is.
func drillExit(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, pipeline.ErrDrillFailures):
		return 1
	case errors.Is(err, pipeline.ErrReportNotStored):
		return 3
	default:
		return 1
	}
}

func emitDrill(output string, out drillOutput) {
	if output == "json" {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}
	printDrill(out)
}

// The human summary. One line per repository and one for the whole run, because a drill of a
// thousand repositories is read by scrolling to the end.
func printDrill(out drillOutput) {
	r := out.DrillReport
	for _, repo := range r.Repos {
		if repo.Status != pipeline.StatusSuccess {
			fmt.Printf("FAIL %s: %s\n", repo.Slug, repo.Error)
			for _, m := range repo.Mismatches {
				fmt.Printf("       %s\n", m)
			}
		}
	}
	fmt.Printf("drill %s: %s restored from %s\n", r.Status, drillScope(cameBack(r.Repos), r.Drilled, r.Eligible), r.ManifestKey)
	if !r.ManifestSigned {
		fmt.Println("note: the manifest's signature was not checked, so this proves these artifacts restore, not that gitdr wrote them")
	}
	switch {
	case out.ReportKey != nil:
		fmt.Printf("report: %s\n", *out.ReportKey)
	case out.ReportNotWritten == reportNotRequested:
		fmt.Println("note: no report was written, because -no-report was given. This drill only read the bucket, wrote nothing to it and signed nothing, so the output above is the only record of it")
	default:
		// Without this, a clean run ends "all 214 repositories restored" and then exits non-zero
		// with nothing on screen to explain it.
		fmt.Println("note: this report was not stored, so the only copy of it is the output above")
	}
}

// cameBack is how many repositories restored, which is not how many were drilled.
//
// The summary used the drilled count and called it restored, so a drill that restored nothing
// ended "drill failed: all 1 repositories restored". A repository counts once `Restore` has
// returned cleanly: its status is success, or the source comparison ran - that comparison only
// happens after a clean restore, so a mismatch there is a repository that came back and does
// not match, not one that failed to come back. The control plane counts it the same way.
func cameBack(repos []pipeline.DrillRepo) int {
	n := 0
	for _, r := range repos {
		if r.Status == pipeline.StatusSuccess || r.SourceMatch != nil {
			n++
		}
	}
	return n
}

// drillScope is the part of the summary that says how much came back.
//
// The sample is named in the same breath as the result. A ten-repository drill of a
// thousand-repository organisation must not read like a thousand-repository guarantee. When
// something did not come back, the sentence counts against what was drilled, because that is
// the population the failure belongs to.
func drillScope(restored, drilled, eligible int) string {
	switch {
	case restored == drilled && drilled == eligible:
		return fmt.Sprintf("all %d repositories", eligible)
	case restored == drilled:
		return fmt.Sprintf("%d of %d repositories", drilled, eligible)
	default:
		return fmt.Sprintf("%d of %d drilled repositories", restored, drilled)
	}
}
