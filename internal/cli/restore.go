package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

func runRestore(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	common := registerCommon(fs)
	repo := fs.String("repo", "", "owner/name to restore")
	host := fs.String("host", "github.com", "source host")
	date := fs.String("date", "", "backup date (YYYY-MM-DD)")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" {
		fmt.Fprintln(os.Stderr, "restore: -repo must be owner/name")
		return 2
	}
	if *date == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "restore: -date and -out are required")
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
	encKey, err := resolveEncryptionKey(cfg)
	if err != nil {
		log.Error("encryption key", "err", err)
		return 1
	}
	// The public key is optional here, unlike verify: a config without one must keep
	// restoring. When a path is configured it is resolved exactly the way verify
	// resolves it, and any problem with it is a failure, not a quiet fall back to an
	// unverified restore.
	var pub ed25519.PublicKey
	if strings.TrimSpace(cfg.Manifest.PublicKeyPath) != "" {
		pubPEM, err := cfg.ResolveManifestPublicKey()
		if err != nil {
			log.Error("public key", "err", err)
			return 1
		}
		pub, err = crypto.ParsePublicKey(pubPEM)
		if err != nil {
			log.Error("public key", "err", err)
			return 1
		}
	}
	res, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: dst, Git: gitexec.New(log), EncryptionKey: encKey, PublicKey: pub, Logger: log}, pipeline.RestoreRequest{
		Host: *host, Owner: owner, Name: name, Date: *date, OutDir: *out,
	})
	if err != nil {
		log.Error("restore failed", "err", err)
		return 1
	}
	if common.output == "json" {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("restored %s -> %s (sha256 %s)\n", res.BundleKey, res.OutDir, res.SHA256[:12])
		// The counts on their own line, because this is the line that goes into an audit
		// file. SOC 2 A1.3.2, CIS 11.5, ISO 27001 A.8.13 and NIS2 (EU) 2024/2690 4.2.3 all
		// want a tested restore with a documented result, and "8 of 8 refs" is that result.
		fmt.Printf("refs: %d of %d declared by the bundle present at the same commit\n",
			res.Refs.Matched, res.Refs.Declared)
		fmt.Println(res.Verification)
	}
	return 0
}
