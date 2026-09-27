package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/source"
)

const bothGitHubCredentials = "source.github: both a token file (source.github.tokenPath) and an App private key " +
	"(GITDR_GITHUB_APP_PRIVATE_KEY or source.github.privateKeyPath) are set; set exactly one"

// captureStderr runs fn with os.Stderr going to a pipe and returns what was written, which is
// where the logger writes.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	var out []byte
	var readErr error
	done := make(chan struct{})
	go func() { out, readErr = io.ReadAll(r); close(done) }()

	fn()
	os.Stderr = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out)
}

// countingServer counts every request it is sent and answers 404.
type countingServer struct {
	*httptest.Server
	mu sync.Mutex
	n  int
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	c := &countingServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *countingServer) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// githubConfig writes a config whose GitHub API and S3 endpoint are both local servers that
// count what reaches them, and sets the environment a run would read credentials from.
func githubConfig(t *testing.T, github, storage *countingServer, env map[string]string) string {
	t.Helper()
	for _, k := range []string{
		"GITDR_SOURCE_GITHUB_TOKENPATH", "GITDR_GITHUB_APP_PRIVATE_KEY", "GITDR_SOURCE_GITHUB_PRIVATEKEYPATH",
		"GITDR_MANIFEST_PUBLICKEYPATH", "GITDR_MANIFEST_SIGNING_KEY", "GITDR_MANIFEST_SIGNINGKEYPATH",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for k, v := range env {
		t.Setenv(k, v)
	}
	doc := "source:\n  type: github\n  baseURL: " + github.URL + "\n" +
		"destination:\n  type: s3\n  s3:\n    bucket: b\n    region: us-east-1\n    endpoint: " + storage.URL + "\n    usePathStyle: true\n"
	path := filepath.Join(t.TempDir(), "gitdr.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A config with a token file and an App key is refused, by backup and by doctor, with the one
// message that says what to do, and before gitdr talks to anything.
func TestBothGitHubCredentialsAreRefusedBeforeAnyNetworkCall(t *testing.T) {
	ctx := context.Background()

	for _, keyEnv := range []map[string]string{
		{"GITDR_GITHUB_APP_PRIVATE_KEY": "-----BEGIN RSA PRIVATE KEY-----"},
		{"GITDR_SOURCE_GITHUB_PRIVATEKEYPATH": "/run/secrets/github-app.pem"},
	} {
		for name := range keyEnv {
			t.Run("backup, key from "+name, func(t *testing.T) {
				github, storage := newCountingServer(t), newCountingServer(t)
				// Everything else a backup needs, so that without the refusal it would go on to
				// check the destination and list repositories, and the count below would see it.
				_, signingKey, err := crypto.GenerateKeyPair()
				if err != nil {
					t.Fatal(err)
				}
				env := map[string]string{
					"GITDR_SOURCE_GITHUB_TOKENPATH": writeTokenFile(t, "ghs_token"),
					"GITDR_MANIFEST_SIGNING_KEY":    string(signingKey),
				}
				for k, v := range keyEnv {
					env[k] = v
				}
				cfg := githubConfig(t, github, storage, env)

				var code int
				logs := captureStderr(t, func() { code = Run(ctx, []string{"backup", "-config", cfg}) })
				if code != 1 {
					t.Errorf("exit %d, want 1", code)
				}
				if !strings.Contains(logs, bothGitHubCredentials) {
					t.Errorf("the refusal is not in the log:\n%s", logs)
				}
				if n := github.requests() + storage.requests(); n != 0 {
					t.Errorf("%d requests went out before the refusal", n)
				}
			})
		}
	}

	t.Run("doctor", func(t *testing.T) {
		github, storage := newCountingServer(t), newCountingServer(t)
		cfg := githubConfig(t, github, storage, map[string]string{
			"GITDR_SOURCE_GITHUB_TOKENPATH": writeTokenFile(t, "ghs_token"),
			"GITDR_GITHUB_APP_PRIVATE_KEY":  "-----BEGIN RSA PRIVATE KEY-----",
		})

		var code int
		out := captureStdout(t, func() {
			_ = captureStderr(t, func() { code = Run(ctx, []string{"doctor", "-config", cfg, "-output", "json"}) })
		})
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		check := doctorCheck(t, out, "source")
		if check.OK || check.Detail != bothGitHubCredentials {
			t.Errorf("source check = %+v, want it failed with the refusal", check)
		}
		// doctor goes on to check the destination, as it always has; GitHub it never asks.
		if n := github.requests(); n != 0 {
			t.Errorf("%d requests reached GitHub", n)
		}
	})
}

func doctorCheck(t *testing.T, out, name string) checkResult {
	t.Helper()
	var doc struct {
		OK     bool          `json:"ok"`
		Checks []checkResult `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, out)
	}
	for _, c := range doc.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("doctor reported no %q check:\n%s", name, out)
	return checkResult{}
}

// A token file that cannot be read fails doctor's source check, naming the setting, before any
// request to GitHub: the source reads the file once when it is built.
func TestDoctorFailsOnAnUnreadableTokenFile(t *testing.T) {
	github, storage := newCountingServer(t), newCountingServer(t)
	missing := filepath.Join(t.TempDir(), "github-token")
	cfg := githubConfig(t, github, storage, map[string]string{"GITDR_SOURCE_GITHUB_TOKENPATH": missing})

	var code int
	out := captureStdout(t, func() {
		_ = captureStderr(t, func() { code = Run(context.Background(), []string{"doctor", "-config", cfg, "-output", "json"}) })
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	check := doctorCheck(t, out, "source")
	if check.OK || !strings.Contains(check.Detail, "source.github.tokenPath") || !strings.Contains(check.Detail, missing) {
		t.Errorf("source check = %+v, want it failed naming source.github.tokenPath and the file", check)
	}
	if n := github.requests(); n != 0 {
		t.Errorf("%d requests reached GitHub", n)
	}
}

// verify and drill never build a source, so a config that backup would refuse for its GitHub
// credential does not stop them. They fail here for want of a public key, which is how this
// knows they got past the point where the credential would have been read.
func TestVerifyAndDrillNeverReadTheGitHubCredential(t *testing.T) {
	for _, args := range [][]string{
		{"verify", "-manifest", "github.com/octo/manifests/20260613T120000Z.manifest.json"},
		{"drill", "-no-report", "-manifest", "github.com/octo/manifests/20260613T120000Z.manifest.json"},
	} {
		t.Run(args[0], func(t *testing.T) {
			github, storage := newCountingServer(t), newCountingServer(t)
			cfg := githubConfig(t, github, storage, map[string]string{
				"GITDR_SOURCE_GITHUB_TOKENPATH": filepath.Join(t.TempDir(), "no-such-dir", "github-token"),
				"GITDR_GITHUB_APP_PRIVATE_KEY":  "-----BEGIN RSA PRIVATE KEY-----",
			})
			var code int
			logs := captureStderr(t, func() { code = Run(context.Background(), append(args, "-config", cfg)) })
			if code != 1 || !strings.Contains(logs, "public key") {
				t.Fatalf("exit %d, and the log does not show the public key was reached:\n%s", code, logs)
			}
			if strings.Contains(logs, "source.github") {
				t.Errorf("%s looked at the GitHub credential:\n%s", args[0], logs)
			}
		})
	}
}

// Sources for checkSource.
type builtSource struct{}

func (builtSource) ListRepos(context.Context, source.Filter) ([]source.Repo, error) { return nil, nil }
func (builtSource) CloneURL(context.Context, source.Repo) (string, error)           { return "", nil }
func (builtSource) FetchMetadata(context.Context, source.Repo) ([]byte, error)      { return nil, nil }

type mintingSource struct {
	builtSource
	err error
}

func (m mintingSource) GitAuthHeader(context.Context) (string, error) {
	return "Authorization: Basic x", m.err
}

type tokenFileSource struct {
	mintingSource
	usesFile bool
	checkErr error
	checked  *int
}

func (s tokenFileSource) UsesTokenFile() bool { return s.usesFile }
func (s tokenFileSource) CheckToken(context.Context) error {
	*s.checked++
	return s.checkErr
}

// doctor tests a token file by using it, once, and says so. In App mode nothing changes: it
// mints, and says that.
func TestDoctorChecksTheSourceCredential(t *testing.T) {
	var checked int
	for _, tc := range []struct {
		name        string
		src         source.Source
		want        checkResult
		wantChecked int
	}{
		{
			name: "a token file GitHub accepts",
			src:  tokenFileSource{usesFile: true, checked: &checked},
			want: checkResult{Name: "source auth", OK: true, Detail: "token file read and accepted by GitHub"}, wantChecked: 1,
		},
		{
			name: "a token file GitHub refuses",
			src:  tokenFileSource{usesFile: true, checkErr: errors.New("401 Bad credentials"), checked: &checked},
			want: checkResult{Name: "source auth", OK: false, Detail: "401 Bad credentials"}, wantChecked: 1,
		},
		{
			name: "App mode mints, and never checks a token file",
			src:  tokenFileSource{usesFile: false, checked: &checked},
			want: checkResult{Name: "source auth", OK: true, Detail: "installation token minted"},
		},
		{
			name: "App mode that cannot mint",
			src:  mintingSource{err: errors.New("could not refresh installation id 1's token")},
			want: checkResult{Name: "source auth", OK: false, Detail: "could not refresh installation id 1's token"},
		},
		{
			name: "a source with no credential",
			src:  builtSource{},
			want: checkResult{Name: "source", OK: true, Detail: "built"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked = 0
			if got := checkSource(context.Background(), tc.src); got != tc.want {
				t.Errorf("checkSource = %+v, want %+v", got, tc.want)
			}
			if checked != tc.wantChecked {
				t.Errorf("CheckToken called %d times, want %d", checked, tc.wantChecked)
			}
		})
	}
}
