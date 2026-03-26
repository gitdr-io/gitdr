package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

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
	res, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: dst, Git: gitexec.New(log), EncryptionKey: encKey, Logger: log}, pipeline.RestoreRequest{
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
		fmt.Printf("restored %s -> %s (verified, sha256 %s)\n", res.BundleKey, res.OutDir, res.SHA256[:12])
	}
	return 0
}
