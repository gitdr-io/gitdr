package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/metrics"
	"gitdr.io/gitdr/internal/pipeline"
)

func runBackup(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	common := registerCommon(fs)
	requireWORM := fs.Bool("require-worm", false, "fail if the destination is not WORM-immutable (default: warn and proceed)")
	repo := fs.String("repo", "", "owner/name to back up (overrides config source.repo)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, log, err := common.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if *repo != "" {
		cfg.Source.Repo = *repo
	}
	if err := cfg.Validate(); err != nil {
		log.Error("invalid config", "err", err)
		return 1
	}
	src, err := buildSource(cfg, log)
	if err != nil {
		log.Error("source", "err", err)
		return 1
	}
	dst, err := buildDest(ctx, cfg, log)
	if err != nil {
		log.Error("destination", "err", err)
		return 1
	}
	signingPEM, err := cfg.ResolveManifestSigningKey()
	if err != nil {
		log.Error("signing key", "err", err)
		return 1
	}
	signer, err := crypto.ParsePrivateKey(signingPEM)
	if err != nil {
		log.Error("signing key", "err", err)
		return 1
	}
	encKey, err := resolveEncryptionKey(cfg)
	if err != nil {
		log.Error("encryption key", "err", err)
		return 1
	}

	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config:        cfg,
		Source:        src,
		Dest:          dst,
		Git:           gitexec.New(log),
		SigningKey:    signer,
		EncryptionKey: encKey,
		ToolVersion:   version(),
		Logger:        log,
		RequireWORM:   *requireWORM || cfg.WORM.Require,
	})
	if res != nil && res.Manifest != nil {
		emitBackup(common.output, res)
	}
	if err != nil {
		log.Error("backup failed", "err", err)
		return 1
	}
	if werr := metrics.New(cfg.Metrics.TextfilePath).WriteSuccess(okRepoCount(res.Manifest)); werr != nil {
		log.Warn("metrics write failed", "err", werr)
	}
	return 0
}

// okRepoCount counts repos that ended up protected (backed up now or already present).
func okRepoCount(m *pipeline.Manifest) int {
	n := 0
	for _, r := range m.Repos {
		if r.Status != pipeline.StatusFailed {
			n++
		}
	}
	return n
}

// backupOutput is the JSON shape of a backup run: the manifest's own fields, plus the key
// the manifest was stored under.
//
// The key is not part of the manifest and must not become part of it — the manifest is signed,
// and an object cannot name its own location without changing every time it moves. But without
// it there is no supported way to learn what to pass to `gitdr verify -manifest`, which left
// consumers scraping it out of a log line.
//
// Embedded, so the manifest's fields stay at the top level in their existing order and
// `manifestKey` is appended. Existing readers see exactly what they saw before.
//
// Note for anyone verifying: this is not the signed document. The signature covers the
// canonical bytes stored in the destination, which is what `verify` fetches. Never check a
// signature against stdout.
type backupOutput struct {
	*pipeline.Manifest
	ManifestKey string `json:"manifestKey"`
}

func emitBackup(output string, res *pipeline.BackupResult) {
	if output == "json" {
		b, _ := json.MarshalIndent(backupOutput{Manifest: res.Manifest, ManifestKey: res.ManifestKey}, "", "  ")
		fmt.Println(string(b))
		return
	}
	m := res.Manifest
	fmt.Printf("run %s, %s\n", m.RunID, m.Status)
	for _, r := range m.Repos {
		fmt.Printf("  %s: %s\n", r.Slug, r.Status)
		if r.Error != "" {
			fmt.Printf("    error: %s\n", r.Error)
		}
	}
	fmt.Printf("manifest: %s\n", res.ManifestKey)
}
