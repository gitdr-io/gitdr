package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/redact"
)

func TestEnvOverrides(t *testing.T) {
	t.Setenv("GITDR_DESTINATION_S3_BUCKET", "env-bucket")
	t.Setenv("GITDR_DESTINATION_RETENTION_DAYS", "7")
	t.Setenv("GITDR_SOURCE_GITHUB_APPID", "42")
	t.Setenv("GITDR_DESTINATION_S3_USEPATHSTYLE", "true")
	t.Setenv("GITDR_GITHUB_APP_PRIVATE_KEY", "secret-pem")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Destination.S3.Bucket != "env-bucket" {
		t.Errorf("bucket = %q", c.Destination.S3.Bucket)
	}
	if c.Destination.Retention.Days != 7 {
		t.Errorf("days = %d", c.Destination.Retention.Days)
	}
	if c.Source.GitHub.AppID != 42 {
		t.Errorf("appID = %d", c.Source.GitHub.AppID)
	}
	if !c.Destination.S3.UsePathStyle {
		t.Error("usePathStyle should be true")
	}
	if c.Source.GitHub.PrivateKey.Reveal() != "secret-pem" {
		t.Error("private key not injected from env")
	}
}

// A secret leaves the environment as soon as gitdr has it. A process the cloud SDKs start for
// credentials, an AWS credential_process or the Azure CLI, inherits gitdr's environment, and has no
// use for gitdr's own keys. Everything else stays where it is: the SDKs read their own variables
// after Load, and the other GITDR_* settings are not secrets.
func TestLoadTakesSecretsOutOfTheEnvironment(t *testing.T) {
	secrets := map[string]string{
		"GITDR_GITHUB_APP_PRIVATE_KEY":             "canary-app-key",
		"GITDR_GITLAB_TOKEN":                       "canary-gitlab-token",
		"GITDR_DESTINATION_AZURE_CONNECTIONSTRING": "canary-connection-string",
		"GITDR_MANIFEST_SIGNING_KEY":               "canary-signing-key",
		"GITDR_ENCRYPTION_KEY":                     "canary-encryption-key",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("GITDR_DESTINATION_S3_BUCKET", "a-bucket")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "the-sdk-reads-this-itself")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]redact.Secret{
		"GITDR_GITHUB_APP_PRIVATE_KEY":             c.Source.GitHub.PrivateKey,
		"GITDR_GITLAB_TOKEN":                       c.Source.GitLab.Token,
		"GITDR_DESTINATION_AZURE_CONNECTIONSTRING": c.Destination.Azure.ConnectionString,
		"GITDR_MANIFEST_SIGNING_KEY":               c.Manifest.SigningKey,
		"GITDR_ENCRYPTION_KEY":                     c.Encryption.Key,
	}
	for k, want := range secrets {
		if got := held[k].Reveal(); got != want {
			t.Errorf("%s: the config holds %q, want %q", k, got, want)
		}
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still in the environment after Load", k)
		}
	}
	for _, k := range []string{"GITDR_DESTINATION_S3_BUCKET", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := os.LookupEnv(k); !ok {
			t.Errorf("Load took %s out of the environment; only gitdr's own secrets go", k)
		}
	}
}

// Where the storage account lives in Resource Manager arrives like any other non-secret field,
// from YAML and then GITDR_* env. It decides whether the Azure WORM check can read a lock at all,
// so a value that silently failed to arrive would turn every locked container into an unknown one.
func TestAzureResourceManagerLocation(t *testing.T) {
	const yaml = "destination:\n  type: azure\n  azure:\n    container: c\n    subscriptionID: sub-yaml\n    resourceGroup: rg-yaml\n"
	for _, tc := range []struct {
		name, yaml      string
		env             map[string]string
		wantSub, wantRG string
	}{
		{name: "from YAML", yaml: yaml, wantSub: "sub-yaml", wantRG: "rg-yaml"},
		{
			name: "env over YAML", yaml: yaml,
			env:     map[string]string{"GITDR_DESTINATION_AZURE_SUBSCRIPTIONID": "sub-env", "GITDR_DESTINATION_AZURE_RESOURCEGROUP": "rg-env"},
			wantSub: "sub-env", wantRG: "rg-env",
		},
		{name: "unset", yaml: "destination:\n  type: azure\n  azure:\n    container: c\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"GITDR_DESTINATION_AZURE_SUBSCRIPTIONID", "GITDR_DESTINATION_AZURE_RESOURCEGROUP"} {
				t.Setenv(k, "") // restored after the test
				_ = os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "gitdr.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Destination.Azure.SubscriptionID != tc.wantSub || c.Destination.Azure.ResourceGroup != tc.wantRG {
				t.Errorf("subscriptionID=%q resourceGroup=%q, want %q and %q",
					c.Destination.Azure.SubscriptionID, c.Destination.Azure.ResourceGroup, tc.wantSub, tc.wantRG)
			}
		})
	}
}

// The Azure account becomes a hostname, <account>.blob.core.windows.net, and the credential goes
// to whatever host that names. So it has to be a storage account name: 3 to 24 lowercase letters
// and digits, Azure's own rule. "x@example.org/" made the blob URL
// https://x@example.org/.blob.core.windows.net/, whose host is example.org.
func TestAzureAccountIsAStorageAccountName(t *testing.T) {
	for _, tc := range []struct {
		account string
		ok      bool
	}{
		{account: "", ok: true}, // a connection string or an endpoint names it instead
		{account: "acme", ok: true},
		{account: "devstoreaccount1", ok: true},
		{account: "abc", ok: true},
		{account: strings.Repeat("a", 24), ok: true},
		{account: "x@example.org/"},
		{account: "acme.evil.com"},
		{account: "Acme"},
		{account: "ab"},
		{account: strings.Repeat("a", 25)},
		{account: "acme-gitdr"},
		{account: " acme"},
		{account: "acme\n"},
	} {
		c := Default()
		c.Destination.Type = "azure"
		c.Destination.Azure.Container = "gitdr"
		c.Destination.Azure.Account = tc.account
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("account %q refused: %v", tc.account, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("account %q validates", tc.account)
				continue
			}
			if !strings.Contains(err.Error(), "destination.azure.account") {
				t.Errorf("the refusal of %q does not name the setting: %v", tc.account, err)
			}
			// The value is not quoted back: a mistake here can be a connection string.
			if strings.ContainsAny(tc.account, ".@") && strings.Contains(err.Error(), tc.account) {
				t.Errorf("the refusal quotes the value: %v", err)
			}
		}
	}
}

func TestDefaultsRetained(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Destination.Retention.Mode != "COMPLIANCE" || c.Destination.Retention.Days != 30 {
		t.Errorf("defaults lost: %+v", c.Destination.Retention)
	}
	if c.WORM.Require {
		t.Error("worm.require must default false")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Destination.S3.Bucket = "b"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.Destination.Retention.Mode = "BOGUS"
	if err := c.Validate(); err == nil {
		t.Error("bad retention mode accepted")
	}
	c2 := Default() // missing bucket
	if err := c2.Validate(); err == nil {
		t.Error("missing bucket accepted")
	}
}

// The multipart settings take S3's own bounds on a part, 5 MiB to 5 GiB, or 0 for the default.
func TestMultipartSettingsStayInsideS3sBounds(t *testing.T) {
	for _, tc := range []struct {
		threshold, part int64
		ok              bool
	}{
		{0, 0, true},
		{8 << 20, 5 << 20, true},
		{5 << 30, 5 << 30, true},
		{5<<20 - 1, 0, false},
		{0, 5<<30 + 1, false},
		{-1, 0, false},
	} {
		c := Default()
		c.Destination.S3.Bucket = "b"
		c.Destination.S3.MultipartThreshold, c.Destination.S3.PartSize = tc.threshold, tc.part
		if err := c.Validate(); (err == nil) != tc.ok {
			t.Errorf("threshold %d, part %d: err = %v, want ok %v", tc.threshold, tc.part, err, tc.ok)
		}
	}
	t.Setenv("GITDR_DESTINATION_S3_MULTIPARTTHRESHOLD", "8388608")
	t.Setenv("GITDR_DESTINATION_S3_PARTSIZE", "5242880")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Destination.S3.MultipartThreshold != 8<<20 || c.Destination.S3.PartSize != 5<<20 {
		t.Errorf("from the environment: threshold %d, part %d", c.Destination.S3.MultipartThreshold, c.Destination.S3.PartSize)
	}
}

func TestSecretNeverFormatted(t *testing.T) {
	c := Default()
	c.Source.GitHub.PrivateKey = redact.Secret("super-secret-key")
	if s := fmt.Sprintf("%v %+v", c.Source.GitHub, c.Source); strings.Contains(s, "super-secret-key") {
		t.Fatalf("secret leaked in formatted output: %s", s)
	}
}

// Which GitHub credential a run uses is decided from the config alone. The token file is never
// opened to decide it: every path here names a file that does not exist.
func TestGitHubTokenFileChoosesExactlyOneCredential(t *testing.T) {
	const both = "source.github: both a token file (source.github.tokenPath) and an App private key " +
		"(GITDR_GITHUB_APP_PRIVATE_KEY or source.github.privateKeyPath) are set; set exactly one"
	missing := filepath.Join(t.TempDir(), "never-written")

	for _, tc := range []struct {
		name     string
		gh       GitHubConfig
		wantPath string
		wantErr  string
	}{
		{name: "a token file", gh: GitHubConfig{TokenPath: missing}, wantPath: missing},
		{name: "a token file, with whitespace around the path", gh: GitHubConfig{TokenPath: " " + missing + "\n"}, wantPath: missing},
		{name: "a token file, appID and installationID ignored", gh: GitHubConfig{TokenPath: missing, AppID: 7, InstallationID: 9}, wantPath: missing},
		{name: "a token file, and a key that is only whitespace", gh: GitHubConfig{TokenPath: missing, PrivateKey: " \n"}, wantPath: missing},
		{name: "an App key from env", gh: GitHubConfig{PrivateKey: "pem"}},
		{name: "an App key from a file", gh: GitHubConfig{PrivateKeyPath: missing}},
		{name: "a blank token path is not a token file", gh: GitHubConfig{TokenPath: "  ", PrivateKey: "pem"}},
		{name: "a token file and a key from env", gh: GitHubConfig{TokenPath: missing, PrivateKey: "pem"}, wantErr: both},
		{name: "a token file and a key file", gh: GitHubConfig{TokenPath: missing, PrivateKeyPath: missing}, wantErr: both},
		{name: "a token file and both kinds of key", gh: GitHubConfig{TokenPath: missing, PrivateKey: "pem", PrivateKeyPath: missing}, wantErr: both},
		{name: "neither", gh: GitHubConfig{AppID: 7, InstallationID: 9},
			wantErr: "no GitHub App private key: set GITDR_GITHUB_APP_PRIVATE_KEY or source.github.privateKeyPath, " +
				"or set source.github.tokenPath to a file holding an installation token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Source.GitHub = tc.gh
			got, err := c.GitHubTokenFile()
			switch {
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("err = %v, want exactly %q", err, tc.wantErr)
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantPath {
				t.Errorf("token file = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// The key's own resolver names the token file too, since either would have done.
func TestNoGitHubAppKeyNamesTheTokenFile(t *testing.T) {
	_, err := Default().ResolveGitHubPrivateKey()
	if err == nil || !strings.Contains(err.Error(), "source.github.tokenPath") {
		t.Errorf("err = %v, want it to name source.github.tokenPath", err)
	}
}

// tokenPath arrives from YAML and from GITDR_SOURCE_GITHUB_TOKENPATH, env winning, like every
// other path. It is a path, so YAML may set it; the token itself never comes from config.
func TestGitHubTokenPathFromYAMLAndEnv(t *testing.T) {
	const yamlDoc = "source:\n  type: github\n  github:\n    tokenPath: /run/yaml/github-token\n"
	for _, tc := range []struct {
		name, env, want string
	}{
		{name: "from YAML", want: "/run/yaml/github-token"},
		{name: "env over YAML", env: "/run/env/github-token", want: "/run/env/github-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITDR_SOURCE_GITHUB_TOKENPATH", "")
			if err := os.Unsetenv("GITDR_SOURCE_GITHUB_TOKENPATH"); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("GITDR_SOURCE_GITHUB_TOKENPATH", tc.env)
			}
			path := filepath.Join(t.TempDir(), "gitdr.yaml")
			if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Source.GitHub.TokenPath != tc.want {
				t.Errorf("tokenPath = %q, want %q", c.Source.GitHub.TokenPath, tc.want)
			}
		})
	}
}

// How long a run may wait for GitHub's rate limits arrives like any other field, from YAML and
// then env, as a Go duration. Unset is zero, which the source reads as its default of an hour. A
// negative wait means nothing and is refused rather than read as one.
func TestMaxRateLimitWaitFromYAMLAndEnv(t *testing.T) {
	const yamlDoc = "source:\n  type: github\n  github:\n    maxRateLimitWait: 90m\n"
	for _, tc := range []struct {
		name, yaml, env string
		want            time.Duration
		wantErr         bool
	}{
		{name: "unset", yaml: "source:\n  type: github\n"},
		{name: "from YAML", yaml: yamlDoc, want: 90 * time.Minute},
		{name: "env over YAML", yaml: yamlDoc, env: "2h", want: 2 * time.Hour},
		{name: "negative", yaml: yamlDoc, env: "-1m", want: -time.Minute, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITDR_SOURCE_GITHUB_MAXRATELIMITWAIT", "")
			if err := os.Unsetenv("GITDR_SOURCE_GITHUB_MAXRATELIMITWAIT"); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("GITDR_SOURCE_GITHUB_MAXRATELIMITWAIT", tc.env)
			}
			path := filepath.Join(t.TempDir(), "gitdr.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml+"destination:\n  s3:\n    bucket: b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Source.GitHub.MaxRateLimitWait != tc.want {
				t.Errorf("maxRateLimitWait = %s, want %s", c.Source.GitHub.MaxRateLimitWait, tc.want)
			}
			if err := c.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// Validate never looks at the token file. verify, restore and drill call it and nothing more, so
// this is what keeps them from needing, or touching, a GitHub credential.
func TestValidateDoesNotTouchTheTokenFile(t *testing.T) {
	c := Default()
	c.Destination.S3.Bucket = "b"
	c.Source.GitHub.TokenPath = filepath.Join(t.TempDir(), "no-such-dir", "github-token")
	c.Source.GitHub.PrivateKey = "pem" // both set: refused by backup and doctor, not here
	if err := c.Validate(); err != nil {
		t.Errorf("Validate read the GitHub credential: %v", err)
	}
}

// ValidateDestination checks the destination block and nothing about the source, which is how
// `doctor -only destination` checks a bucket before anything is connected to back up into it.
// Validate still refuses a source it does not know, for every other command.
func TestValidateDestinationLeavesTheSourceAlone(t *testing.T) {
	c := Default()
	c.Source.Type = ""
	c.Destination.S3.Bucket = "b"
	if err := c.ValidateDestination(); err != nil {
		t.Errorf("a destination with no source rejected: %v", err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "source.type") {
		t.Errorf("Validate = %v, want the missing source refused", err)
	}

	for _, broken := range []func(*Config){
		func(c *Config) { c.Destination.S3.Bucket = "" },
		func(c *Config) { c.Destination.Type = "ftp" },
		func(c *Config) { c.Destination.Retention.Mode = "BOGUS" },
		func(c *Config) { c.Destination.Retention.Days = 0 },
	} {
		c := Default()
		c.Source.Type = ""
		c.Destination.S3.Bucket = "b"
		broken(c)
		if err := c.ValidateDestination(); err == nil {
			t.Errorf("a broken destination accepted: %+v", c.Destination)
		}
	}
}
