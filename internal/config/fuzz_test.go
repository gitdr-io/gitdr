package config

import (
	"os"
	"path/filepath"
	"testing"

	"gitdr.io/gitdr/internal/redact"
)

// FuzzConfigLoad runs arbitrary YAML through the full Load path. Two claims: an
// operator-supplied file can never panic the parser, and — the one this package
// documents — secrets cannot be set from YAML, whatever keys the document tries.
func FuzzConfigLoad(f *testing.F) {
	// Ambient credentials would legitimately populate the secret fields and drown the
	// assertion below, so drop them for this process.
	for _, k := range []string{
		"GITDR_GITHUB_APP_PRIVATE_KEY", "GITDR_GITLAB_TOKEN",
		"GITDR_DESTINATION_AZURE_CONNECTIONSTRING",
		"GITDR_MANIFEST_SIGNING_KEY", "GITDR_ENCRYPTION_KEY",
	} {
		_ = os.Unsetenv(k)
	}

	if b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml")); err == nil {
		f.Add(b)
	}
	f.Add([]byte("source:\n  type: gitlab\n"))
	f.Add([]byte("source:\n  github:\n    privateKey: sneaky\n    appID: 7\n"))
	f.Add([]byte("manifest:\n  signingKey: sneaky\nencryption:\n  key: sneaky\n"))
	f.Add([]byte("destination:\n  azure:\n    connectionString: sneaky\n"))
	f.Add([]byte("defaults: &d {level: debug}\nlog:\n  <<: *d\n"))
	f.Add([]byte("a: &a\n  github: {appID: 1}\nsource: *a\n"))
	f.Add([]byte("source: !!binary aGk=\n"))
	f.Add([]byte("[]"))
	f.Add([]byte("\t"))
	f.Add([]byte{0xff, 0xfe, 0x00})
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 { // config files are KiBs; alias expansion makes big docs slow
			return
		}
		path := filepath.Join(t.TempDir(), "gitdr.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			return // rejecting input is fine; panicking or smuggling secrets is not
		}
		for name, s := range map[string]redact.Secret{
			"source.github.privateKey":           c.Source.GitHub.PrivateKey,
			"source.gitlab.token":                c.Source.GitLab.Token,
			"destination.azure.connectionString": c.Destination.Azure.ConnectionString,
			"manifest.signingKey":                c.Manifest.SigningKey,
			"encryption.key":                     c.Encryption.Key,
		} {
			if !s.IsZero() {
				t.Fatalf("%s was set from YAML", name)
			}
		}
		_ = c.Validate() // structured error or nil, never a panic
	})
}
