package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
)

func runRestore(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	common := registerCommon(fs)
	repo := fs.String("repo", "", "owner/name to restore, or group/subgroup/name for a GitLab project in a subgroup")
	host := fs.String("host", "github.com", "source host")
	date := fs.String("date", "", "backup date (YYYY-MM-DD)")
	manifest := fs.String("manifest", "", "run-manifest object key, as backup printed it (manifestKey)")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	req, err := restoreRequest(fs, *repo, *host, *date, *manifest, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
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
	// -manifest is the verified form and has no other. Reading a manifest without checking its
	// signature would take the host, date and checksums from a document anybody could have written.
	if req.ManifestKey != "" && pub == nil {
		log.Error("public key", "err", "restore -manifest needs the public key: set manifest.publicKeyPath (GITDR_MANIFEST_PUBLICKEYPATH) to the key the backup was signed with")
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
	res, err := pipeline.Restore(ctx, pipeline.RestoreDeps{Dest: dst, Git: gitexec.New(log), EncryptionKey: encKey, PublicKey: pub, Logger: log}, req)
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

// restoreRequest reads restore's command line into a request, or says what is wrong with it.
//
// Two forms. -manifest names the run by the key backup printed, and -repo picks the repository in
// it; the host and date are the manifest's, so giving either as well is refused rather than
// ignored or compared. Without it, -host and -date say where to look.
func restoreRequest(fs *flag.FlagSet, repo, host, date, manifest, out string) (pipeline.RestoreRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return pipeline.RestoreRequest{}, err
	}
	if out == "" {
		return pipeline.RestoreRequest{}, errors.New("restore: -out is required")
	}
	if manifest != "" {
		// Set explicitly, even to -host's default: fs.Visit sees only flags given on the line.
		var given []string
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "host" || f.Name == "date" {
				given = append(given, "-"+f.Name)
			}
		})
		if len(given) > 0 {
			return pipeline.RestoreRequest{}, fmt.Errorf("restore: -manifest names the run, and the host and date come from it; drop %s", strings.Join(given, " and "))
		}
		return pipeline.RestoreRequest{ManifestKey: manifest, Owner: owner, Name: name, OutDir: out}, nil
	}
	if date == "" {
		return pipeline.RestoreRequest{}, errors.New("restore: give -date (YYYY-MM-DD), or -manifest with the manifestKey the backup printed")
	}
	if d, err := time.Parse("2006-01-02", date); err != nil || d.Format("2006-01-02") != date {
		return pipeline.RestoreRequest{}, fmt.Errorf("restore: -date %q is not a date in the form YYYY-MM-DD", date)
	}
	return pipeline.RestoreRequest{Host: host, Owner: owner, Name: name, Date: date, OutDir: out}, nil
}

// splitRepo splits -repo at its last slash.
//
// A repository's name has no slash in it and its owner can: a GitLab project in a subgroup is
// acme/platform/api, the project api in the group acme/platform. Splitting at the first slash made
// that owner acme and name platform/api, and such a project could not be restored at all.
func splitRepo(repo string) (owner, name string, err error) {
	i := strings.LastIndex(repo, "/")
	bad := i < 0
	for _, seg := range strings.Split(repo, "/") {
		if seg == "" {
			bad = true
		}
	}
	if bad {
		return "", "", errors.New("restore: -repo must be owner/name, or group/subgroup/name for a GitLab project in a subgroup")
	}
	return repo[:i], repo[i+1:], nil
}
