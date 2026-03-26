package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/pipeline"
)

func runVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	common := registerCommon(fs)
	manifest := fs.String("manifest", "", "manifest object key to verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" {
		fmt.Fprintln(os.Stderr, "verify: -manifest is required")
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
	res, err := pipeline.Verify(ctx, pipeline.VerifyDeps{Dest: dst, PublicKey: pub, Logger: log}, *manifest)
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
