// Package cli wires the gitdr subcommands (backup, restore, verify, doctor) over the
// stdlib flag package. Logs go to stderr; machine-readable output goes to stdout.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	azurebackend "gitdr.io/gitdr/internal/dest/azure"
	gcsbackend "gitdr.io/gitdr/internal/dest/gcs"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/logging"
	"gitdr.io/gitdr/internal/source"
	ghsrc "gitdr.io/gitdr/internal/source/github"
	glsrc "gitdr.io/gitdr/internal/source/gitlab"
)

const usage = `gitdr, disaster recovery for Git, to immutable storage.

Usage:
  gitdr <command> [flags]

Commands:
  backup    Back up repositories to immutable storage
  restore   Restore a repository from a backup bundle
  verify    Verify a run-manifest signature and artifact checksums
  drill     Restore the backups and prove they carry the history they claim
  doctor    Check config, credentials, connectivity, and the WORM lock

Run "gitdr <command> -h" for command-specific flags.`

// Run dispatches a subcommand and returns a process exit code (non-zero on any error).
func Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch cmd := args[0]; cmd {
	case "backup":
		return runBackup(ctx, args[1:])
	case "restore":
		return runRestore(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	case "drill":
		return runDrill(ctx, args[1:])
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "version", "--version", "-version":
		fmt.Println(version())
		return 0
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stderr, usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
}

type commonOpts struct {
	configPath string
	logLevel   string
	logFormat  string
	output     string
	// gitless is set by a command, or a mode of one, that runs no git: nothing is said about
	// the variables git would not get from gitdr's environment.
	gitless bool
}

func registerCommon(fs *flag.FlagSet) *commonOpts {
	o := &commonOpts{}
	fs.StringVar(&o.configPath, "config", os.Getenv("GITDR_CONFIG"), "path to config YAML")
	fs.StringVar(&o.logLevel, "log-level", "", "log level: debug|info|warn|error")
	fs.StringVar(&o.logFormat, "log-format", "", "log format: json|text")
	fs.StringVar(&o.output, "output", "text", "result output: text|json")
	return o
}

// withheldWarning is logged once per run, with the names, when gitdr's environment holds variables
// that would change what git does and that git no longer gets (SPEC §6).
const withheldWarning = "git does not get these variables from gitdr's environment; configure git in /etc/gitconfig"

func (o *commonOpts) load() (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, nil, err
	}
	if o.logLevel != "" {
		cfg.Log.Level = o.logLevel
	}
	if o.logFormat != "" {
		cfg.Log.Format = o.logFormat
	}
	log := logging.Default(cfg.Log.Level, cfg.Log.Format)
	if names := gitexec.Withheld(); len(names) > 0 && !o.gitless {
		log.Warn(withheldWarning, "vars", names)
	}
	return cfg, log, nil
}

func buildSource(cfg *config.Config, log *slog.Logger) (source.Source, error) {
	switch cfg.Source.Type {
	case "github":
		// Which credential, decided here and nowhere earlier: backup and doctor are the only
		// commands that build a source, so they are the only ones that refuse a config with
		// both a token file and an App key, and the refusal comes before any network call.
		tokenPath, err := cfg.GitHubTokenFile()
		if err != nil {
			return nil, err
		}
		if tokenPath != "" {
			// appID and installationID are ignored: the token belongs to one installation.
			return ghsrc.New(ghsrc.Options{
				BaseURL: cfg.Source.BaseURL, TokenPath: tokenPath,
				MaxRateLimitWait: cfg.Source.GitHub.MaxRateLimitWait,
			}, log)
		}
		key, err := cfg.ResolveGitHubPrivateKey()
		if err != nil {
			return nil, err
		}
		return ghsrc.New(ghsrc.Options{
			BaseURL:          cfg.Source.BaseURL,
			AppID:            cfg.Source.GitHub.AppID,
			InstallationID:   cfg.Source.GitHub.InstallationID,
			PrivateKeyPEM:    key,
			MaxRateLimitWait: cfg.Source.GitHub.MaxRateLimitWait,
		}, log)
	case "gitlab":
		return glsrc.New(glsrc.Options{
			BaseURL: cfg.Source.BaseURL,
			Token:   cfg.Source.GitLab.Token.Reveal(),
		}, log)
	default:
		return nil, fmt.Errorf("unsupported source type %q", cfg.Source.Type)
	}
}

// resolveEncryptionKey returns the parsed 32-byte key when encryption is enabled, or
// nil when disabled.
func resolveEncryptionKey(cfg *config.Config) ([]byte, error) {
	mat, err := cfg.EncryptionKeyMaterial()
	if err != nil || mat == nil {
		return nil, err
	}
	return crypto.ParseEncryptionKey(mat)
}

func buildDest(ctx context.Context, cfg *config.Config, log *slog.Logger) (dest.Destination, error) {
	switch cfg.Destination.Type {
	case "s3":
		return s3backend.New(ctx, s3backend.Options{
			Bucket:             cfg.Destination.S3.Bucket,
			Region:             cfg.Destination.S3.Region,
			Endpoint:           cfg.Destination.S3.Endpoint,
			UsePathStyle:       cfg.Destination.S3.UsePathStyle,
			MultipartThreshold: cfg.Destination.S3.MultipartThreshold,
			PartSize:           cfg.Destination.S3.PartSize,
		}, log)
	case "gcs":
		return gcsbackend.New(ctx, gcsbackend.Options{
			Bucket:   cfg.Destination.GCS.Bucket,
			Endpoint: cfg.Destination.GCS.Endpoint,
		}, log)
	case "azure":
		return azurebackend.New(ctx, azurebackend.Options{
			Account:          cfg.Destination.Azure.Account,
			Container:        cfg.Destination.Azure.Container,
			Endpoint:         cfg.Destination.Azure.Endpoint,
			ConnectionString: cfg.Destination.Azure.ConnectionString.Reveal(),
			SubscriptionID:   cfg.Destination.Azure.SubscriptionID,
			ResourceGroup:    cfg.Destination.Azure.ResourceGroup,
		}, log)
	default:
		return nil, fmt.Errorf("unsupported destination type %q", cfg.Destination.Type)
	}
}
