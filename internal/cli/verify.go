package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/pipeline"
)

func runVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	common := registerCommon(fs)
	manifest := fs.String("manifest", "", "manifest object key to verify")
	// A drill report is signed evidence and had no way to be checked: `verify -manifest` on one
	// reported "0 of 0 artifacts ok" and exited zero, because a drill report unmarshals into a
	// Manifest with no artifacts. This is the check that document deserves.
	drill := fs.String("drill", "", "drill report object key to verify, instead of -manifest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*manifest == "") == (*drill == "") {
		fmt.Fprintln(os.Stderr, "verify: give exactly one of -manifest or -drill")
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
	deps := pipeline.VerifyDeps{Dest: dst, PublicKey: pub, Logger: log}

	if *drill != "" {
		return verifyDrill(ctx, deps, *drill, common.output, log)
	}

	res, err := pipeline.Verify(ctx, deps, *manifest)
	if res != nil {
		if common.output == "json" {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Printf("signature valid: %v, artifacts %d/%d ok\n", res.SignatureValid, res.ArtifactsOK, res.ArtifactsChecked)
			for _, f := range res.Failures {
				fmt.Printf("  FAIL %s\n", f)
			}
		}
	}
	if err != nil {
		log.Error("verify failed", "err", err)
		return 1
	}
	return 0
}

// The drill half of `verify`. Its own function because it answers a different question and must
// not borrow the manifest result's field names: this reads nothing back out of the bucket, so
// there is no artifact count to report and inventing one would be the whole problem again.
func verifyDrill(ctx context.Context, deps pipeline.VerifyDeps, key, output string, log *slog.Logger) int {
	res, err := pipeline.VerifyDrill(ctx, deps, key)
	if res != nil {
		if output == "json" {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
		} else if res.SignatureValid {
			scope := fmt.Sprintf("%d of %d repositories", res.Drilled, res.Eligible)
			if res.Drilled == res.Eligible {
				scope = fmt.Sprintf("all %d repositories", res.Eligible)
			}
			fmt.Printf("signature valid, signed by the key in this config\n")
			fmt.Printf("the report says: %s, %s restored from %s\n", res.Status, scope, res.ManifestKey)
			for _, f := range res.Failures {
				fmt.Printf("  FAIL %s\n", f)
			}
			// Said out loud, because a reader who has just been told a signature is valid is one
			// step from believing more than that.
			if !res.ManifestSigned {
				fmt.Println("note: this drill did not check the manifest's own signature, so it proves those artifacts restore, not that gitdr wrote them")
			}
			fmt.Println("note: this checks the report is authentic and unaltered. It does not re-run the drill.")
		}
	}
	if err != nil {
		log.Error("verify drill failed", "err", err)
		return 1
	}
	return 0
}
