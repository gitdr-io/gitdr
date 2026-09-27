package gitexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake git.
//
// With fakeGitLog set, the test binary is git: it appends one JSON line naming its argv and every
// GIT_* variable in its environment to the file the variable names, and exits 0 having done
// nothing. What a command was started with is otherwise visible only to `ps` and /proc, and it is
// the thing under test here.
const fakeGitLog = "GITDR_TEST_FAKE_GIT_LOG"

func TestMain(m *testing.M) {
	if path := os.Getenv(fakeGitLog); path != "" {
		os.Exit(fakeGit(path))
	}
	os.Exit(m.Run())
}

type invocation struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func fakeGit(logPath string) int {
	inv := invocation{Args: os.Args[1:], Env: map[string]string{}}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "GIT_") {
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

// Every git command gitdr starts carries the low-speed limits, and the ones that talk to the
// source carry the credential beside them, in the environment and nowhere on the command line.
//
// Each method is called against the fake git, which prints nothing, so the ones that parse git's
// output return errors here. Those are ignored: what is under test is what the process was
// started with, not what it said.
func TestEveryGitCommandCarriesTheLowSpeedLimits(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv(fakeGitLog, logPath)

	g := &Git{bin: exe, logger: slog.New(slog.DiscardHandler)}
	ctx := context.Background()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "r.bundle")

	const (
		remote = "https://git.example.test/octo/hello.git"
		cred   = "eC1hY2Nlc3MtdG9rZW46Z2hzX2Zha2VfdG9rZW4=" // base64 of x-access-token:ghs_fake_token
		header = "Authorization: Basic " + cred
	)
	auth := Options{AuthHeader: header}

	for _, tc := range []struct {
		name string
		run  func()
		// Talks to the source, and so is handed the credential, scoped to its host.
		network bool
	}{
		{name: "clone --mirror", network: true, run: func() { _ = g.CloneMirror(ctx, remote, filepath.Join(dir, "m.git"), auth) }},
		{name: "ls-remote", network: true, run: func() { _, _ = g.LsRemote(ctx, remote, auth) }},
		{name: "lfs fetch", network: true, run: func() { _ = g.LFSFetchAll(ctx, dir, remote, auth) }},
		{name: "for-each-ref, HasRefs", run: func() { _, _ = g.HasRefs(ctx, dir) }},
		{name: "for-each-ref, ListRefs", run: func() { _, _ = g.ListRefs(ctx, dir) }},
		{name: "rev-parse", run: func() { _, _ = g.HeadOID(ctx, dir) }},
		{name: "bundle create", run: func() { _ = g.BundleAll(ctx, dir, bundle) }},
		{name: "init and bundle verify", run: func() { _ = g.BundleVerify(ctx, bundle) }},
		{name: "init and bundle list-heads", run: func() { _, _ = g.BundleHeads(ctx, bundle) }},
		{name: "clone from a bundle", run: func() { _ = g.CloneFromBundle(ctx, bundle, filepath.Join(dir, "restored")) }},
		{name: "lfs install", run: func() { _ = g.LFSInstallLocal(ctx, dir) }},
		{name: "lfs checkout", run: func() { _ = g.LFSCheckout(ctx, dir) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			tc.run()
			for _, inv := range readInvocations(t, logPath) {
				cfg := configPairs(t, inv.Env)
				if cfg["http.lowSpeedLimit"] != "1000" || cfg["http.lowSpeedTime"] != "600" {
					t.Errorf("git %v started with lowSpeedLimit %q and lowSpeedTime %q, want 1000 and 600",
						inv.Args, cfg["http.lowSpeedLimit"], cfg["http.lowSpeedTime"])
				}

				const scoped = "http.https://git.example.test/.extraHeader"
				switch got, ok := cfg[scoped]; {
				case tc.network && got != header:
					t.Errorf("git %v: %s = %q, want the credential header", inv.Args, scoped, got)
				case !tc.network && ok:
					t.Errorf("git %v talks to no remote and was handed a credential anyway", inv.Args)
				}

				// The credential is in exactly one place: the config value above.
				for _, a := range inv.Args {
					if strings.Contains(a, cred) || strings.Contains(a, "ghs_fake_token") || strings.Contains(a, "Authorization") {
						t.Errorf("the credential reached argv: %q", a)
					}
				}
				holders := 0
				for _, v := range inv.Env {
					if strings.Contains(v, cred) {
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

// What the environment is made of, in order: the caller's own, without any GIT_CONFIG_* it
// carried, then gitdr's pairs, the limits first. git's GIT_HTTP_LOW_SPEED_* variables survive,
// because they are how an operator overrides the limits.
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
	// Nothing from the machine's own git setup: no credential helper, no proxy, and no askpass
	// program, which an editor's terminal sets and GIT_TERMINAL_PROMPT=0 does not stop.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
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
