package gitexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake git.
//
// Started under the name git or git-lfs, through a link fake makes, the test binary is that
// program: it appends one JSON line naming its argv and its whole environment to the log beside
// the link, and exits 0 having done nothing. What a command was started with is otherwise visible
// only to `ps` and /proc, and it is the thing under test here.
//
// It knows it is the fake by its name and not by a variable, because which variables reach git is
// what these tests check, and one that told the test binary to act as git would have to be let
// through for that.
const fakeLog = "invocations.jsonl"

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "git", "git-lfs":
		os.Exit(fakeGit(filepath.Join(filepath.Dir(os.Args[0]), fakeLog)))
	}
	os.Exit(m.Run())
}

// fake links name, git or git-lfs, to this test binary in a directory of its own, and returns the
// link and the log the fake writes to.
func fake(t *testing.T, name string) (bin, logPath string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, name)
	if err := os.Symlink(exe, bin); err != nil {
		t.Fatal(err)
	}
	return bin, filepath.Join(dir, fakeLog)
}

type invocation struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func fakeGit(logPath string) int {
	inv := invocation{Args: os.Args[1:], Env: map[string]string{}}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			inv.Env[k] = v
		}
	}
	line, err := json.Marshal(inv)
	if err != nil {
		return 3
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 3
	}
	return 0
}

func readInvocations(t *testing.T, path string) []invocation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no git command ran: %v", err)
	}
	var out []invocation
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var inv invocation
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			t.Fatalf("unreadable invocation %q: %v", line, err)
		}
		out = append(out, inv)
	}
	return out
}

// configPairs reads the GIT_CONFIG_* configuration a command was started with, the way git does:
// GIT_CONFIG_COUNT pairs, zero-indexed, each key with the value of the same index. A pair with a
// half missing is an error to git, so it is one here.
func configPairs(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	n, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil {
		t.Fatalf("GIT_CONFIG_COUNT = %q: %v", env["GIT_CONFIG_COUNT"], err)
	}
	pairs := map[string]string{}
	for i := 0; i < n; i++ {
		k, kok := env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		v, vok := env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]
		if !kok || !vok {
			t.Fatalf("GIT_CONFIG_COUNT=%d but pair %d is incomplete: key %v, value %v", n, i, kok, vok)
		}
		pairs[k] = v
	}
	return pairs
}

// The source and the credential every fake command is started for.
const (
	fakeRemote = "https://git.example.test/octo/hello.git"
	fakeCred   = "eC1hY2Nlc3MtdG9rZW46Z2hzX2Zha2VfdG9rZW4=" // base64 of x-access-token:ghs_fake_token
	fakeHeader = "Authorization: Basic " + fakeCred
	// The config key the credential travels under, scoped to fakeRemote's host.
	fakeScoped = "http.https://git.example.test/.extraHeader"
)

// gitCommand is one of the ways gitdr starts git.
type gitCommand struct {
	name string
	run  func()
	// Talks to the source, and so is handed the credential, scoped to its host.
	network bool
}

// everyGitCommand calls each method that starts git once, against g, with dir as the repository
// and the source credential on the commands that talk to the source.
//
// Against the fake git, which prints nothing, the methods that parse git's output return errors.
// Those are ignored: what is under test is what the process was started with, not what it said.
func everyGitCommand(g *Git, dir string) []gitCommand {
	ctx := context.Background()
	bundle := filepath.Join(dir, "r.bundle")
	auth := Options{AuthHeader: fakeHeader}
	return []gitCommand{
		{name: "clone --mirror", network: true, run: func() { _ = g.CloneMirror(ctx, fakeRemote, filepath.Join(dir, "m.git"), auth) }},
		{name: "ls-remote", network: true, run: func() { _, _ = g.LsRemote(ctx, fakeRemote, auth) }},
		{name: "lfs fetch", network: true, run: func() { _ = g.LFSFetchAll(ctx, dir, fakeRemote, auth) }},
		{name: "for-each-ref, HasRefs", run: func() { _, _ = g.HasRefs(ctx, dir) }},
		{name: "for-each-ref, ListRefs", run: func() { _, _ = g.ListRefs(ctx, dir) }},
		{name: "rev-parse", run: func() { _, _ = g.HeadOID(ctx, dir) }},
		{name: "bundle create", run: func() { _ = g.BundleAll(ctx, dir, bundle) }},
		{name: "init and bundle verify", run: func() { _ = g.BundleVerify(ctx, bundle) }},
		{name: "init and bundle list-heads", run: func() { _, _ = g.BundleHeads(ctx, bundle) }},
		{name: "clone from a bundle", run: func() { _ = g.CloneFromBundle(ctx, bundle, filepath.Join(dir, "restored")) }},
		{name: "lfs install", run: func() { _ = g.LFSInstallLocal(ctx, dir) }},
		{name: "lfs checkout", run: func() { _ = g.LFSCheckout(ctx, dir) }},
	}
}

// removeLog clears the fake's log, so what is read next is one command's.
func removeLog(t *testing.T, logPath string) {
	t.Helper()
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// Every git command gitdr starts carries the low-speed limits, and the ones that talk to the
// source carry the credential beside them, in the environment and nowhere on the command line.
func TestEveryGitCommandCarriesTheLowSpeedLimits(t *testing.T) {
	bin, logPath := fake(t, "git")
	g := &Git{bin: bin, logger: slog.New(slog.DiscardHandler)}

	for _, tc := range everyGitCommand(g, t.TempDir()) {
		t.Run(tc.name, func(t *testing.T) {
			removeLog(t, logPath)
			tc.run()
			for _, inv := range readInvocations(t, logPath) {
				cfg := configPairs(t, inv.Env)
				if cfg["http.lowSpeedLimit"] != "1000" || cfg["http.lowSpeedTime"] != "600" {
					t.Errorf("git %v started with lowSpeedLimit %q and lowSpeedTime %q, want 1000 and 600",
						inv.Args, cfg["http.lowSpeedLimit"], cfg["http.lowSpeedTime"])
				}

				switch got, ok := cfg[fakeScoped]; {
				case tc.network && got != fakeHeader:
					t.Errorf("git %v: %s = %q, want the credential header", inv.Args, fakeScoped, got)
				case !tc.network && ok:
					t.Errorf("git %v talks to no remote and was handed a credential anyway", inv.Args)
				}

				// The credential is in exactly one place: the config value above.
				for _, a := range inv.Args {
					if strings.Contains(a, fakeCred) || strings.Contains(a, "ghs_fake_token") || strings.Contains(a, "Authorization") {
						t.Errorf("the credential reached argv: %q", a)
					}
				}
				holders := 0
				for _, v := range inv.Env {
					if strings.Contains(v, fakeCred) {
						holders++
					}
				}
				if want := map[bool]int{true: 1, false: 0}[tc.network]; holders != want {
					t.Errorf("git %v: %d environment variables hold the credential, want %d", inv.Args, holders, want)
				}
			}
		})
	}
}

// runSecrets is what a run keeps in gitdr's environment and git must never see, each with a value
// that cannot occur by accident: what the hosted agent puts there (the destination's keys, the
// manifest signing key, a GitLab token), and what an operator's run can hold besides.
var runSecrets = map[string]string{
	"AWS_ACCESS_KEY_ID":                        "canary-aws-access-key-id",
	"AWS_SECRET_ACCESS_KEY":                    "canary-aws-secret-access-key",
	"AWS_SESSION_TOKEN":                        "canary-aws-session-token",
	"GOOGLE_APPLICATION_CREDENTIALS":           "canary-google-application-credentials",
	"AZURE_CLIENT_SECRET":                      "canary-azure-client-secret",
	"AZURE_STORAGE_KEY":                        "canary-azure-storage-key",
	"GITDR_MANIFEST_SIGNING_KEY":               "canary-manifest-signing-key",
	"GITDR_ENCRYPTION_KEY":                     "canary-encryption-key",
	"GITDR_GITHUB_APP_PRIVATE_KEY":             "canary-github-app-private-key",
	"GITDR_GITLAB_TOKEN":                       "canary-gitlab-token",
	"GITDR_DESTINATION_AZURE_CONNECTIONSTRING": "canary-azure-connection-string",
}

// secretsIn names every variable of env that holds one of runSecrets.
func secretsIn(env map[string]string) []string {
	var found []string
	for k, v := range env {
		for _, secret := range runSecrets {
			if strings.Contains(v, secret) {
				found = append(found, k)
			}
		}
	}
	slices.Sort(found)
	return found
}

// gitdrsOwn matches the variables gitdr itself sets on git, as opposed to passing through.
var gitdrsOwn = regexp.MustCompile(`^(GIT_TERMINAL_PROMPT|GIT_LFS_SKIP_SMUDGE|GIT_CONFIG_COUNT|GIT_CONFIG_(KEY|VALUE)_[0-9]+)$`)

// git is started with the variables it needs from gitdr's environment, and no others.
//
// Every variable git needs is set below with a value of its own, and has to arrive unchanged; so
// is every secret in runSecrets, and every variable gitdr withholds on purpose, and none of those
// may arrive. What arrives is compared whole, so anything else in the environment this test runs
// in, CI or GOPATH, fails it just the same.
func TestGitSeesOnlyTheEnvironmentItNeeds(t *testing.T) {
	bin, logPath := fake(t, "git")
	home, tmp := t.TempDir(), t.TempDir()

	// passedThrough's list, written out again rather than read from it, each with a value of its
	// own. One dropped from there fails this test, and so does one added there and not here,
	// whether or not the machine running the test happens to set it.
	needed := map[string]string{
		"PATH":                     "/usr/local/bin:/usr/bin:/bin",
		"HOME":                     home,
		"TMPDIR":                   tmp,
		"HTTPS_PROXY":              "http://proxy.example.test:3128",
		"https_proxy":              "http://proxy.example.test:3129",
		"HTTP_PROXY":               "http://proxy.example.test:3130",
		"http_proxy":               "http://proxy.example.test:3131",
		"ALL_PROXY":                "socks5://proxy.example.test:1080",
		"all_proxy":                "socks5://proxy.example.test:1081",
		"NO_PROXY":                 ".internal.example.test",
		"no_proxy":                 "localhost",
		"GIT_SSL_CAINFO":           "/etc/gitdr/ca.pem",
		"GIT_SSL_CAPATH":           "/etc/gitdr/ca.d",
		"SSL_CERT_FILE":            "/etc/ssl/gitdr.pem",
		"SSL_CERT_DIR":             "/etc/ssl/gitdr.d",
		"GIT_HTTP_LOW_SPEED_LIMIT": "500",
		"GIT_HTTP_LOW_SPEED_TIME":  "1200",
	}
	if got, want := slices.Sorted(maps.Keys(passedThrough)), slices.Sorted(maps.Keys(needed)); !slices.Equal(got, want) {
		t.Fatalf("passedThrough lets through %v, and this test checks %v", got, want)
	}
	// What gitexec.go leaves out on purpose: each would point git at another configuration,
	// repository, program or library, turn TLS verification off, or change its language.
	withheld := map[string]string{
		"GIT_SSL_NO_VERIFY":     "true",
		"GIT_ASKPASS":           "/canary/askpass",
		"SSH_ASKPASS":           "/canary/ssh-askpass",
		"GIT_CONFIG_GLOBAL":     "/canary/gitconfig",
		"GIT_CONFIG_PARAMETERS": "'canary.key'='canary'",
		"GIT_DIR":               "/canary/git-dir",
		"XDG_CONFIG_HOME":       "/canary/xdg",
		"LD_PRELOAD":            "/canary/preload.so",
		"LANG":                  "canary_LANG.UTF-8",
		"LC_ALL":                "canary_LC_ALL.UTF-8",
	}
	for _, env := range []map[string]string{needed, runSecrets, withheld} {
		for k, v := range env {
			t.Setenv(k, v)
		}
	}

	g := &Git{bin: bin, logger: slog.New(slog.DiscardHandler)}
	for _, tc := range everyGitCommand(g, t.TempDir()) {
		t.Run(tc.name, func(t *testing.T) {
			removeLog(t, logPath)
			tc.run()
			for _, inv := range readInvocations(t, logPath) {
				if found := secretsIn(inv.Env); len(found) > 0 {
					t.Errorf("git %v was started with a run's secrets in %s", inv.Args, strings.Join(found, ", "))
				}
				var unneeded []string
				for k := range inv.Env {
					if _, ok := needed[k]; !ok && !gitdrsOwn.MatchString(k) {
						unneeded = append(unneeded, k)
					}
				}
				if len(unneeded) > 0 {
					slices.Sort(unneeded)
					t.Errorf("git %v was started with %d variables it does not need: %s",
						inv.Args, len(unneeded), strings.Join(unneeded, ", "))
				}
				for k, want := range needed {
					if got, ok := inv.Env[k]; !ok || got != want {
						t.Errorf("git %v: %s = %q (set: %v), want %q", inv.Args, k, got, ok, want)
					}
				}
			}
		})
	}
}

// Withheld names what is in gitdr's environment, would change what git does, and no longer reaches
// it, so an operator who relied on one hears about it. What git does get is not named, and neither
// is the locale, which nearly every environment sets, nor a variable gitdr sets on git itself: the
// image sets GIT_TERMINAL_PROMPT=0, and git gets gitdr's own GIT_TERMINAL_PROMPT=0 regardless.
func TestWithheldNamesWhatGitNoLongerGets(t *testing.T) {
	for k, v := range map[string]string{
		"GIT_TERMINAL_PROMPT":   "0",
		"GIT_LFS_SKIP_SMUDGE":   "1",
		"GIT_CONFIG_COUNT":      "1",
		"GIT_CONFIG_KEY_0":      "core.askPass",
		"GIT_CONFIG_VALUE_0":    "/usr/bin/true",
		"GIT_CONFIG_PARAMETERS": "'http.extraheader'='Authorization: Basic c2VjcmV0'",
		"GIT_PROXY_SSL_CAINFO":  "/etc/proxy-ca.pem",
		"XDG_CONFIG_HOME":       "/home/op/.config",
		"SSH_ASKPASS":           "/usr/bin/ksshaskpass",
		"LD_LIBRARY_PATH":       "/opt/git/lib",
		"GIT_SSL_CAINFO":        "/etc/gitdr/ca.pem",
		"HTTPS_PROXY":           "http://proxy.example.test:3128",
		"LANG":                  "de_DE.UTF-8",
	} {
		t.Setenv(k, v)
	}
	got := Withheld()
	for _, want := range []string{"GIT_CONFIG_PARAMETERS", "GIT_PROXY_SSL_CAINFO", "XDG_CONFIG_HOME", "SSH_ASKPASS", "LD_LIBRARY_PATH"} {
		if !slices.Contains(got, want) {
			t.Errorf("Withheld() = %v, and it does not name %s", got, want)
		}
	}
	for _, passed := range []string{"GIT_SSL_CAINFO", "HTTPS_PROXY", "LANG",
		"GIT_TERMINAL_PROMPT", "GIT_LFS_SKIP_SMUDGE", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if slices.Contains(got, passed) {
			t.Errorf("Withheld() = %v names %s, which it should not", got, passed)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("Withheld() = %v, not sorted", got)
	}
}

// git-lfs is started by git, not by gitdr, so it gets what git passes on. Proven with the real git
// and a git-lfs that writes down its environment: the secrets are set, and none of them reaches
// it. The credential does, in the scoped header git-lfs fetches with, and so does the proxy.
func TestGitLFSSeesNoSecret(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed; in CI the git-lfs environment must be checked, not skipped")
		}
		t.Skip("git is not installed")
	}
	lfs, logPath := fake(t, "git-lfs")
	// First on the PATH git searches, so the git-lfs it runs is the fake. There is no git in that
	// directory, so the git gitdr runs is still the real one.
	t.Setenv("PATH", filepath.Dir(lfs)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	const proxy = "http://proxy.example.test:3128"
	t.Setenv("HTTPS_PROXY", proxy)
	for k, v := range runSecrets {
		t.Setenv(k, v)
	}

	repo := t.TempDir()
	git(t, repo, "init", "--quiet", "--bare", repo)
	if err := New(nil).LFSFetchAll(context.Background(), repo, fakeRemote, Options{AuthHeader: fakeHeader}); err != nil {
		t.Fatalf("git lfs fetch: %v", err)
	}

	for _, inv := range readInvocations(t, logPath) {
		if !slices.Equal(inv.Args, []string{"fetch", "--all"}) {
			t.Errorf("git started git-lfs with %v, want fetch --all", inv.Args)
		}
		if found := secretsIn(inv.Env); len(found) > 0 {
			t.Errorf("git-lfs was started with a run's secrets in %s", strings.Join(found, ", "))
		}
		for k := range inv.Env {
			for _, prefix := range []string{"AWS_", "GOOGLE_", "AZURE_", "GITDR_"} {
				if strings.HasPrefix(k, prefix) {
					t.Errorf("git-lfs was started with %s", k)
				}
			}
		}
		if got := configPairs(t, inv.Env)[fakeScoped]; got != fakeHeader {
			t.Errorf("git-lfs: %s = %q, want the credential header it fetches with", fakeScoped, got)
		}
		if got := inv.Env["HTTPS_PROXY"]; got != proxy {
			t.Errorf("git-lfs: HTTPS_PROXY = %q, want %q", got, proxy)
		}
	}
}

// What the environment is made of, in order: what passedThrough lets through from the caller's
// own, which is never a GIT_CONFIG_* it carried, then gitdr's pairs, the limits first. git's
// GIT_HTTP_LOW_SPEED_* variables survive, because they are how an operator overrides the limits.
func TestCommandEnv(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Basic inherited")
	t.Setenv("GIT_HTTP_LOW_SPEED_LIMIT", "5")
	t.Setenv("GIT_HTTP_LOW_SPEED_TIME", "7")

	env := map[string]string{}
	for _, kv := range commandEnv([]gitConfig{{key: "http.https://h/.extraHeader", value: "Authorization: Basic ours"}}, []string{"GIT_TERMINAL_PROMPT=0"}) {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := env[k]; dup && strings.HasPrefix(k, "GIT_CONFIG_") {
			t.Errorf("%s appears twice", k)
		}
		env[k] = v
	}

	want := map[string]string{
		"GIT_CONFIG_COUNT":         "3",
		"GIT_CONFIG_KEY_0":         "http.lowSpeedLimit",
		"GIT_CONFIG_VALUE_0":       "1000",
		"GIT_CONFIG_KEY_1":         "http.lowSpeedTime",
		"GIT_CONFIG_VALUE_1":       "600",
		"GIT_CONFIG_KEY_2":         "http.https://h/.extraHeader",
		"GIT_CONFIG_VALUE_2":       "Authorization: Basic ours",
		"GIT_TERMINAL_PROMPT":      "0",
		"GIT_HTTP_LOW_SPEED_LIMIT": "5",
		"GIT_HTTP_LOW_SPEED_TIME":  "7",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for k, v := range env {
		if strings.Contains(v, "inherited") {
			t.Errorf("%s=%s: an inherited GIT_CONFIG_* survived", k, v)
		}
	}
}

// The limits take effect: a server that answers and then sends nothing is cut off, where before
// it held the command, and so the run, for good.
//
// git's own GIT_HTTP_LOW_SPEED_TIME shortens the window to one second, which also shows that the
// override works. The limit stays gitdr's, and curl names it in the error: without it, the limit
// is 0, curl never checks, and the command waits until the test's deadline kills it.
func TestAStalledTransferIsAborted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed; in CI the stall must be exercised, not skipped")
		}
		t.Skip("git is not installed")
	}
	// Nothing from the machine's own git setup. Of what isolates git from it, only HOME reaches
	// the git gitdr runs (passedThrough), and no askpass variable does, so an editor's askpass
	// program, which GIT_TERMINAL_PROMPT=0 does not stop, cannot run. The system configuration is
	// still read, as it is in production; the empty credential helper in this HOME's .gitconfig
	// clears one it names, such as osxkeychain. Same as isolateGit in the pipeline tests.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_HTTP_LOW_SPEED_TIME", "1")
	t.Setenv("GIT_HTTP_LOW_SPEED_LIMIT", "")
	if err := os.Unsetenv("GIT_HTTP_LOW_SPEED_LIMIT"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// The stall ends by itself after a while, so a missing limit fails this test at its
		// deadline instead of hanging it: cancelling the command kills git and not the
		// git-remote-http it started, which keeps the connection, and the stderr pipe the
		// command waits on, until the server lets go.
		select {
		case <-r.Context().Done():
		case <-done:
		case <-time.After(25 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) }) // before Close, which waits for the handler

	g := New(nil)
	remote := srv.URL + "/octo/stalled.git"
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{name: "ls-remote", run: func(ctx context.Context) error { _, err := g.LsRemote(ctx, remote, Options{}); return err }},
		{name: "clone --mirror", run: func(ctx context.Context) error {
			return g.CloneMirror(ctx, remote, filepath.Join(t.TempDir(), "m.git"), Options{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			start := time.Now()
			err := tc.run(ctx)
			if ctx.Err() != nil {
				t.Fatalf("still waiting after %s: git applied no low-speed limit", time.Since(start).Round(time.Second))
			}
			if err == nil {
				t.Fatal("a transfer that sent nothing succeeded")
			}
			if !strings.Contains(err.Error(), "1000 bytes") {
				t.Errorf("the error does not name the limit of 1000 bytes a second: %v", err)
			}
		})
	}
}

// Every git process starts in command(), which is what makes the limits and the scoped header
// something no command can be started without. A second exec.Command anywhere in the tree would
// be a second way to start one, with whatever environment its author remembered.
func TestGitIsStartedInOnePlace(t *testing.T) {
	var sites []string
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".cache", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if code, _, _ := strings.Cut(line, "//"); strings.Contains(code, "exec.Command") {
				sites = append(sites, fmt.Sprintf("%s:%d", rel, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], filepath.Join("internal", "gitexec", "gitexec.go")+":") {
		t.Errorf("git must be started in exactly one place, gitexec's command(); found %d:\n  %s",
			len(sites), strings.Join(sites, "\n  "))
	}
}
