package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/config"
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
	deadlineFlag := fs.String("deadline", "", "RFC 3339 time the run's work stops by; the manifest is still written (default $GITDR_DEADLINE)")
	recopy := recopyFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, log, err := common.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	deadline, err := runDeadline(*deadlineFlag, os.Getenv("GITDR_DEADLINE"), time.Now())
	if err != nil {
		log.Error("deadline", "err", err)
		return 1
	}
	if *repo != "" {
		cfg.Source.Repo = *repo
	}
	cfg.Backup.Recopy = recopyNames(*recopy, cfg.Backup.Recopy)
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
		Deadline:      deadline,
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

// runDeadline is when the run's work stops: --deadline, or GITDR_DEADLINE when the flag is not
// given, as an RFC 3339 time. Zero when neither is set. A value that is not such a time, and a
// deadline already past, are errors: a run that cannot tell when to stop must not start as if it
// had been told never to.
func runDeadline(flagValue, envValue string, now time.Time) (time.Time, error) {
	value, from := strings.TrimSpace(flagValue), "--deadline"
	if value == "" {
		value, from = strings.TrimSpace(envValue), "GITDR_DEADLINE"
	}
	if value == "" {
		return time.Time{}, nil
	}
	deadline, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not an RFC 3339 time: %w", from, value, err)
	}
	if !deadline.After(now) {
		return time.Time{}, fmt.Errorf("%s %s has already passed", from, deadline.UTC().Format(time.RFC3339))
	}
	return deadline, nil
}

// recopyFlag registers --recopy on fs and returns every value it is given, in order. Every one, and
// not the last as a string flag keeps: a caller that passes one flag per repository would have the
// others go on relying on the copy that failed, with nothing said.
func recopyFlag(fs *flag.FlagSet) *[]string {
	var given []string
	fs.Func("recopy", "comma-separated repositories, named as the manifest names them, to copy even when unchanged; repeatable (overrides config backup.recopy)", func(v string) error {
		given = append(given, v)
		return nil
	})
	return &given
}

// recopyNames is the repositories to copy even when unchanged: the names --recopy was given, each
// value a comma-separated list, when it names any, and otherwise backup.recopy, which
// GITDR_BACKUP_RECOPY overrides in the config. The flag replaces the configured list rather than
// adding to it, as --repo replaces source.repo.
func recopyNames(given, configured []string) []string {
	var names []string
	for _, v := range given {
		names = append(names, config.SplitList(v)...)
	}
	if len(names) == 0 {
		return configured
	}
	return names
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
