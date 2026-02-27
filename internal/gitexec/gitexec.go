// Package gitexec wraps the system git binary. gitdr shells out to real git for
// faithful clone/bundle semantics (and later git-lfs). Auth is injected via
// GIT_CONFIG_* env, scoped to the clone host, so tokens never reach argv.
package gitexec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Git runs git subcommands.
type Git struct {
	bin    string
	logger *slog.Logger
}

// New returns a Git runner. A nil logger falls back to slog.Default().
func New(logger *slog.Logger) *Git {
	if logger == nil {
		logger = slog.Default()
	}
	return &Git{bin: "git", logger: logger}
}

// Options configures a git invocation.
type Options struct {
	// AuthHeader, if set, is sent as an HTTP Authorization header (e.g.
	// "Authorization: Basic ...") scoped to the clone host, via env not argv.
	AuthHeader string
}

type gitConfig struct{ key, value string }

// CloneMirror runs `git clone --mirror url dir`.
func (g *Git) CloneMirror(ctx context.Context, repoURL, dir string, opts Options) error {
	var cfg []gitConfig
	if opts.AuthHeader != "" {
		cfg = append(cfg, gitConfig{key: extraHeaderKey(repoURL), value: opts.AuthHeader})
	}
	return g.run(ctx, "", cfg, "clone", "--mirror", "--quiet", "--", repoURL, dir)
}

// BundleAll bundles every ref plus HEAD inside repoDir so `git clone <bundle>` checks
// out the default branch on restore.
func (g *Git) BundleAll(ctx context.Context, repoDir, bundlePath string) error {
	return g.run(ctx, repoDir, nil, "bundle", "create", bundlePath, "--all", "HEAD")
}

// BundleVerify runs `git bundle verify bundlePath`.
func (g *Git) BundleVerify(ctx context.Context, bundlePath string) error {
	return g.run(ctx, "", nil, "bundle", "verify", bundlePath)
}

// CloneFromBundle restores a repo by cloning from a bundle file.
func (g *Git) CloneFromBundle(ctx context.Context, bundlePath, dir string) error {
	return g.run(ctx, "", nil, "clone", "--quiet", "--", bundlePath, dir)
}

// LFSAvailable reports whether the git-lfs binary is installed.
func LFSAvailable() bool {
	_, err := exec.LookPath("git-lfs")
	return err == nil
}

// LFSFetchAll downloads all LFS objects referenced by any ref into repoDir, reusing
// the clone's host-scoped auth.
func (g *Git) LFSFetchAll(ctx context.Context, repoDir, repoURL string, opts Options) error {
	var cfg []gitConfig
	if opts.AuthHeader != "" {
		cfg = append(cfg, gitConfig{key: extraHeaderKey(repoURL), value: opts.AuthHeader})
	}
	return g.run(ctx, repoDir, cfg, "lfs", "fetch", "--all")
}

// LFSCheckout materializes LFS files in the working tree from local objects (no network).
func (g *Git) LFSCheckout(ctx context.Context, repoDir string) error {
	return g.run(ctx, repoDir, nil, "lfs", "checkout")
}

func (g *Git) run(ctx context.Context, workdir string, cfg []gitConfig, args ...string) error {
	// audited: g.bin is the constant "git" and args are an argv array (no shell), so
	// shell injection is impossible; "-"-leading positional args are guarded with "--".
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, g.bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	env := append(baseEnv(),
		"GIT_TERMINAL_PROMPT=0", // never block on a credential prompt
		"GIT_LFS_SKIP_SMUDGE=1", // LFS is fetched explicitly later (M2)
	)
	if len(cfg) > 0 {
		env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
		for i, c := range cfg {
			env = append(env,
				fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, c.key),
				fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, c.value),
			)
		}
	}
	cmd.Env = env

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	g.logger.Debug("git", "args", args, "dir", workdir) // args carry no secrets by construction
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// extraHeaderKey scopes the auth header to the clone host, so the token is never sent
// to another host (e.g. on redirect). Non-URL remotes fall back to a global key.
func extraHeaderKey(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		return fmt.Sprintf("http.%s://%s/.extraHeader", u.Scheme, u.Host)
	}
	return "http.extraHeader"
}

// baseEnv is os.Environ minus any inherited GIT_CONFIG_* so our injected config can't
// collide with the caller's.
func baseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, e := range src {
		if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") ||
			strings.HasPrefix(e, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(e, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, e)
	}
	return out
}
