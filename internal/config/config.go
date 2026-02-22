// Package config loads configuration with precedence defaults < YAML < GITDR_* env.
// Secrets never come from YAML, only env or a mounted file by path, and are wrapped
// in redact.Secret so they can't leak into logs.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"gitdr.io/gitdr/internal/redact"
)

// Config is the full gitdr configuration.
type Config struct {
	Source      SourceConfig      `yaml:"source"`
	Destination DestinationConfig `yaml:"destination"`
	Backup      BackupConfig      `yaml:"backup"`
	Manifest    ManifestConfig    `yaml:"manifest"`
	Metrics     MetricsConfig     `yaml:"metrics"`
	Encryption  EncryptionConfig  `yaml:"encryption"`
	WORM        WORMConfig        `yaml:"worm"`
	Log         LogConfig         `yaml:"log"`
}

// SourceConfig configures the VCS source.
type SourceConfig struct {
	Type    string       `yaml:"type"`    // "github" (only backend in this build)
	BaseURL string       `yaml:"baseURL"` // empty = github.com; GHES sets https://host/api/v3 (M2+)
	Repo    string       `yaml:"repo"`    // single-repo selector "owner/name" (walking skeleton)
	Include []string     `yaml:"include"`
	Exclude []string     `yaml:"exclude"`
	GitHub  GitHubConfig `yaml:"github"`
	GitLab  GitLabConfig `yaml:"gitlab"`
}

// GitLabConfig holds the GitLab read-scoped access token (env only; never YAML).
type GitLabConfig struct {
	Token redact.Secret `yaml:"-"`
}

// GitHubConfig holds GitHub App installation credentials. The private key itself is
// supplied via env (GITDR_GITHUB_APP_PRIVATE_KEY) or a file at PrivateKeyPath.
type GitHubConfig struct {
	AppID          int64         `yaml:"appID"`
	InstallationID int64         `yaml:"installationID"`
	PrivateKeyPath string        `yaml:"privateKeyPath"`
	PrivateKey     redact.Secret `yaml:"-"` // injected from env only; never from YAML
}

// DestinationConfig configures the storage destination.
type DestinationConfig struct {
	Type      string          `yaml:"type"` // "s3" (AWS + S3-compatible) | "gcs" | "azure"
	S3        S3Config        `yaml:"s3"`
	GCS       GCSConfig       `yaml:"gcs"`
	Azure     AzureConfig     `yaml:"azure"`
	Retention RetentionConfig `yaml:"retention"`
}

// GCSConfig configures the Google Cloud Storage backend.
type GCSConfig struct {
	Bucket   string `yaml:"bucket"`
	Endpoint string `yaml:"endpoint"` // empty = real GCS; set for an emulator
}

// AzureConfig configures the Azure Blob backend. The connection string is a secret
// (env only); real Azure uses DefaultAzureCredential with account/endpoint.
type AzureConfig struct {
	Account          string        `yaml:"account"`
	Container        string        `yaml:"container"`
	Endpoint         string        `yaml:"endpoint"`
	ConnectionString redact.Secret `yaml:"-"`
}

// S3Config configures the S3 (and S3-compatible) backend.
type S3Config struct {
	Bucket       string `yaml:"bucket"`
	Region       string `yaml:"region"`
	Endpoint     string `yaml:"endpoint"`     // empty = AWS; MinIO/Wasabi/B2 set their endpoint
	UsePathStyle bool   `yaml:"usePathStyle"` // true for MinIO and most S3-compatible stores
}

// RetentionConfig configures object-lock retention applied to every write.
type RetentionConfig struct {
	Mode string `yaml:"mode"` // COMPLIANCE (default) | GOVERNANCE
	Days int    `yaml:"days"` // retain-until = now + Days
}

// BackupConfig configures fan-out across repositories.
type BackupConfig struct {
	Concurrency int  `yaml:"concurrency"` // parallel repos; default 4
	Resume      bool `yaml:"resume"`      // skip repos already backed up for the run date
	LFS         bool `yaml:"lfs"`         // fetch Git LFS objects; default true
}

// ManifestConfig configures run-manifest signing/verification keys. The signing key
// is supplied via env (GITDR_MANIFEST_SIGNING_KEY) or a file at SigningKeyPath.
type ManifestConfig struct {
	SigningKeyPath string        `yaml:"signingKeyPath"`
	PublicKeyPath  string        `yaml:"publicKeyPath"`
	SigningKey     redact.Secret `yaml:"-"` // injected from env only; never from YAML
}

// MetricsConfig configures Prometheus textfile-collector output.
type MetricsConfig struct {
	// TextfilePath is the .prom file node_exporter's textfile collector scrapes.
	// Empty disables metrics output.
	TextfilePath string `yaml:"textfilePath"`
}

// EncryptionConfig configures optional client-side envelope encryption. The key is a
// 32-byte AES-256 KEK supplied via env (GITDR_ENCRYPTION_KEY), never YAML.
type EncryptionConfig struct {
	Enabled bool          `yaml:"enabled"`
	Key     redact.Secret `yaml:"-"`
}

// WORMConfig configures the immutability check. WORM is recommended, not required.
type WORMConfig struct {
	// Require maps to --require-worm. Default false: if the destination is not provably
	// immutable, gitdr warns loudly and proceeds. Set true to fail closed instead.
	Require bool `yaml:"require"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // json|text
}

// Default returns the built-in default configuration.
func Default() *Config {
	return &Config{
		Source: SourceConfig{Type: "github"},
		Destination: DestinationConfig{
			Type:      "s3",
			Retention: RetentionConfig{Mode: "COMPLIANCE", Days: 30},
		},
		Backup: BackupConfig{Concurrency: 4, Resume: true, LFS: true},
		WORM:   WORMConfig{Require: false},
		Log:    LogConfig{Level: "info", Format: "json"},
	}
}

// Load reads config from path (may be empty for "defaults + env only"), then applies
// environment overrides.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	applyEnvOverrides(c)
	return c, nil
}

const envPrefix = "GITDR_"

func applyEnvOverrides(c *Config) {
	envStr(&c.Source.Type, "SOURCE_TYPE")
	envStr(&c.Source.BaseURL, "SOURCE_BASEURL")
	envStr(&c.Source.Repo, "SOURCE_REPO")
	envInt64(&c.Source.GitHub.AppID, "SOURCE_GITHUB_APPID")
	envInt64(&c.Source.GitHub.InstallationID, "SOURCE_GITHUB_INSTALLATIONID")
	envStr(&c.Source.GitHub.PrivateKeyPath, "SOURCE_GITHUB_PRIVATEKEYPATH")
	envSecret(&c.Source.GitHub.PrivateKey, "GITHUB_APP_PRIVATE_KEY")
	envSecret(&c.Source.GitLab.Token, "GITLAB_TOKEN")

	envStr(&c.Destination.Type, "DESTINATION_TYPE")
	envStr(&c.Destination.S3.Bucket, "DESTINATION_S3_BUCKET")
	envStr(&c.Destination.S3.Region, "DESTINATION_S3_REGION")
	envStr(&c.Destination.S3.Endpoint, "DESTINATION_S3_ENDPOINT")
	envBool(&c.Destination.S3.UsePathStyle, "DESTINATION_S3_USEPATHSTYLE")
	envStr(&c.Destination.GCS.Bucket, "DESTINATION_GCS_BUCKET")
	envStr(&c.Destination.GCS.Endpoint, "DESTINATION_GCS_ENDPOINT")
	envStr(&c.Destination.Azure.Account, "DESTINATION_AZURE_ACCOUNT")
	envStr(&c.Destination.Azure.Container, "DESTINATION_AZURE_CONTAINER")
	envStr(&c.Destination.Azure.Endpoint, "DESTINATION_AZURE_ENDPOINT")
	envSecret(&c.Destination.Azure.ConnectionString, "DESTINATION_AZURE_CONNECTIONSTRING")
	envStr(&c.Destination.Retention.Mode, "DESTINATION_RETENTION_MODE")
	envInt(&c.Destination.Retention.Days, "DESTINATION_RETENTION_DAYS")

	envInt(&c.Backup.Concurrency, "BACKUP_CONCURRENCY")
	envBool(&c.Backup.Resume, "BACKUP_RESUME")
	envBool(&c.Backup.LFS, "BACKUP_LFS")

	envStr(&c.Manifest.SigningKeyPath, "MANIFEST_SIGNINGKEYPATH")
	envStr(&c.Manifest.PublicKeyPath, "MANIFEST_PUBLICKEYPATH")
	envSecret(&c.Manifest.SigningKey, "MANIFEST_SIGNING_KEY")

	envStr(&c.Metrics.TextfilePath, "METRICS_TEXTFILEPATH")
	envBool(&c.Encryption.Enabled, "ENCRYPTION_ENABLED")
	envSecret(&c.Encryption.Key, "ENCRYPTION_KEY")

	envBool(&c.WORM.Require, "WORM_REQUIRE")
	envStr(&c.Log.Level, "LOG_LEVEL")
	envStr(&c.Log.Format, "LOG_FORMAT")
}

// Validate checks structural validity. Credential presence is checked by the source
// and destination constructors at the point of use.
func (c *Config) Validate() error {
	switch c.Source.Type {
	case "github", "gitlab":
	default:
		return fmt.Errorf("source.type %q unsupported (github | gitlab)", c.Source.Type)
	}
	switch c.Destination.Type {
	case "s3":
		if strings.TrimSpace(c.Destination.S3.Bucket) == "" {
			return fmt.Errorf("destination.s3.bucket is required")
		}
	case "gcs":
		if strings.TrimSpace(c.Destination.GCS.Bucket) == "" {
			return fmt.Errorf("destination.gcs.bucket is required")
		}
	case "azure":
		if strings.TrimSpace(c.Destination.Azure.Container) == "" {
			return fmt.Errorf("destination.azure.container is required")
		}
	default:
		return fmt.Errorf("destination.type %q unsupported (s3 | gcs | azure)", c.Destination.Type)
	}
	switch strings.ToUpper(strings.TrimSpace(c.Destination.Retention.Mode)) {
	case "COMPLIANCE", "GOVERNANCE":
	default:
		return fmt.Errorf("destination.retention.mode %q must be COMPLIANCE or GOVERNANCE", c.Destination.Retention.Mode)
	}
	if c.Destination.Retention.Days <= 0 {
		return fmt.Errorf("destination.retention.days must be > 0 (got %d)", c.Destination.Retention.Days)
	}
	return nil
}

// ResolveGitHubPrivateKey returns the GitHub App private key PEM from env (preferred)
// or the configured file path.
func (c *Config) ResolveGitHubPrivateKey() ([]byte, error) {
	if !c.Source.GitHub.PrivateKey.IsZero() {
		return []byte(c.Source.GitHub.PrivateKey.Reveal()), nil
	}
	if p := strings.TrimSpace(c.Source.GitHub.PrivateKeyPath); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read github app private key: %w", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("no GitHub App private key: set GITDR_GITHUB_APP_PRIVATE_KEY or source.github.privateKeyPath")
}

// ResolveManifestSigningKey returns the manifest signing key from env (preferred) or
// the configured file path.
func (c *Config) ResolveManifestSigningKey() ([]byte, error) {
	if !c.Manifest.SigningKey.IsZero() {
		return []byte(c.Manifest.SigningKey.Reveal()), nil
	}
	if p := strings.TrimSpace(c.Manifest.SigningKeyPath); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read manifest signing key: %w", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("no manifest signing key: set GITDR_MANIFEST_SIGNING_KEY or manifest.signingKeyPath")
}

// EncryptionKeyMaterial returns the raw encryption key material (from env) when
// encryption is enabled, or nil when disabled. The crypto package parses/validates it.
func (c *Config) EncryptionKeyMaterial() ([]byte, error) {
	if !c.Encryption.Enabled {
		return nil, nil
	}
	if c.Encryption.Key.IsZero() {
		return nil, fmt.Errorf("encryption enabled but GITDR_ENCRYPTION_KEY is not set")
	}
	return []byte(c.Encryption.Key.Reveal()), nil
}

// ResolveManifestPublicKey returns the manifest public key from the configured path.
func (c *Config) ResolveManifestPublicKey() ([]byte, error) {
	if p := strings.TrimSpace(c.Manifest.PublicKeyPath); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read manifest public key: %w", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("no manifest public key: set manifest.publicKeyPath")
}

func envStr(dst *string, key string) {
	if v, ok := os.LookupEnv(envPrefix + key); ok {
		*dst = v
	}
}

func envSecret(dst *redact.Secret, key string) {
	if v, ok := os.LookupEnv(envPrefix + key); ok {
		*dst = redact.Secret(v)
	}
}

func envInt64(dst *int64, key string) {
	if v, ok := os.LookupEnv(envPrefix + key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			*dst = n
		}
	}
}

func envInt(dst *int, key string) {
	if v, ok := os.LookupEnv(envPrefix + key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = n
		}
	}
}

func envBool(dst *bool, key string) {
	if v, ok := os.LookupEnv(envPrefix + key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			*dst = b
		}
	}
}
