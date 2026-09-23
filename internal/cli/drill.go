package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

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
func runDrill(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	common := registerCommon(fs)
	manifest := fs.String("manifest", "", "manifest object key to drill; defaults to the most recent")
	host := fs.String("host", "", "source host, e.g. github.com; needed when -manifest is not given")
	owner := fs.String("owner", "", "organisation; needed when -manifest is not given")
	sample := fs.Int("sample", 0, "restore at most this many repositories, in slug order; 0 means all")
	workdir := fs.String("workdir", "", "where to restore; defaults to the system temp directory")
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

	// The public half verifies the manifest being drilled; the private half signs the report.
	// Both are required: a drill of an unattributable manifest proves the wrong thing, and an
	// unsigned report is a text file anybody can write.
	pubPEM, err := cfg.ResolveManifestPublicKey()
	if err != nil {
		log.Error("public key", "err", err)
		return 1
	}
	pub, err := crypto.ParsePublicKey(pubPEM)
	if err != nil {
		log.Error("public key", "err", err)
		return 1
	}
	privPEM, err := cfg.ResolveManifestSigningKey()
	if err != nil {
		log.Error("signing key", "err", err)
		return 1
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		log.Error("signing key", "err", err)
		return 1
	}

	encKey, err := resolveEncryptionKey(cfg)
	if err != nil {
		log.Error("encryption key", "err", err)
		return 1
	}

	report, err := pipeline.Drill(ctx, pipeline.DrillDeps{
		Dest: dst, Git: gitexec.New(log), EncryptionKey: encKey,
		PublicKey: pub, SigningKey: signer,
		ToolVersion: version(), Logger: log,
	}, pipeline.DrillRequest{
		ManifestKey: *manifest, Host: *host, Owner: *owner,
		Sample: *sample, WorkDir: *workdir,
	})

	if report != nil {
		if common.output == "json" {
			b, _ := json.MarshalIndent(report, "", "  ")
			fmt.Println(string(b))
		} else {
			printDrill(report, !errors.Is(err, pipeline.ErrReportNotStored))
		}
	}
	if err != nil {
		log.Error("drill", "err", err)
	}
	return drillExit(err)
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

// The human summary. One line per repository and one for the whole run, because a drill of a
// thousand repositories is read by scrolling to the end.
func printDrill(r *pipeline.DrillReport, stored bool) {
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
	// Without this, a clean run ends "all 214 repositories restored" and then exits non-zero
	// with nothing on screen to explain it.
	if !stored {
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
