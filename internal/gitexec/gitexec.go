// Package gitexec wraps the system git binary. gitdr shells out to real git for
// faithful clone/bundle semantics (and later git-lfs). Auth is injected via
// GIT_CONFIG_* env, scoped to the clone host, so tokens never reach argv.
package gitexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// HasRefs reports whether repoDir contains at least one ref.
//
// A repository created and never pushed to has none, and `git bundle create` refuses to write
// an empty bundle: "fatal: Refusing to create empty bundle." Without this check that refusal
// reads as a failed repository, and one unused project in an organisation is enough to make
// every backup of it fail for ever.
//
// Asked of the local mirror rather than the provider's API, because it is the state that
// actually matters: a clone that produced no refs is what bundling has to cope with, whatever
// the API said. `git clone --mirror` fails loudly on a partial fetch, so zero refs after a
// successful clone means the remote genuinely has none.
func (g *Git) HasRefs(ctx context.Context, repoDir string) (bool, error) {
	out, err := g.output(ctx, repoDir, "for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// BundleAll bundles every ref plus HEAD inside repoDir so `git clone <bundle>` checks
// out the default branch on restore.
func (g *Git) BundleAll(ctx context.Context, repoDir, bundlePath string) error {
	return g.run(ctx, repoDir, nil, "bundle", "create", bundlePath, "--all", "HEAD")
}

// scratchBundleRepo creates an empty repository to read a bundle from, and returns it with
// an absolute path to the bundle and a cleanup function.
//
// `git bundle verify` refuses to run outside a repository — "need a repository to verify a
// bundle" — because it checks the bundle's prerequisites against an object store, and wants
// one even when, as here, the bundle is self-contained and there is nothing to check
// against. Calling it with no working directory, and so with the process's own, is the bug
// that broke every restore gitdr shipped before v0.1.11: the tests ran inside this
// repository and passed, the container runs at / and failed.
//
// `git bundle list-heads` does not need a repository. It reads the header and never touches
// the object store; checked against git 2.32, 2.36, 2.45, 2.54 and 2.55, and against
// builtin/bundle.c, where the have_repository guard is on verify alone. It is routed through
// here anyway so that reading a bundle has one rule rather than two. The cost is a temporary
// directory and a `git init --bare`; the alternative is a restore path whose correctness
// depends on which subcommand happens to require setup_git_directory, re-established by hand
// on every git upgrade.
//
// The bundle path is made absolute because these commands run with the scratch repository as
// their working directory, where a relative path would resolve somewhere else entirely.
func (g *Git) scratchBundleRepo(ctx context.Context, bundlePath string) (dir, abs string, cleanup func(), err error) {
	scratch, err := os.MkdirTemp("", "gitdr-bundle-")
	if err != nil {
		return "", "", nil, fmt.Errorf("scratch repo: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(scratch) }
	if err := g.run(ctx, "", nil, "init", "--quiet", "--bare", scratch); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("scratch repo: %w", err)
	}
	abs, err = filepath.Abs(bundlePath)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("bundle path: %w", err)
	}
	return scratch, abs, cleanup, nil
}

// BundleVerify runs `git bundle verify bundlePath` from a scratch repository.
//
// It reads the bundle's header — format, prerequisites, refs — and stops, so it is a
// structural check and not an integrity one. Integrity is the SHA-256 the restore path
// compares against the sidecar and the signed manifest before git is asked anything.
func (g *Git) BundleVerify(ctx context.Context, bundlePath string) error {
	scratch, abs, cleanup, err := g.scratchBundleRepo(ctx, bundlePath)
	if err != nil {
		return err
	}
	defer cleanup()
	return g.run(ctx, scratch, nil, "bundle", "verify", abs)
}

// BundleRef is one entry from a bundle's header: a name the bundle declares and the object
// it points at.
type BundleRef struct {
	// Name is a full ref name such as "refs/heads/main" or "refs/tags/v1", or the literal
	// "HEAD". A bundle written with `bundle create --all HEAD` carries a HEAD entry, which
	// is not a ref any repository stores under refs/; callers normalise it.
	Name string
	// OID is the object the bundle declares for Name, exactly as recorded. For an annotated
	// tag this is the tag object, not the commit it peels to.
	OID string
}

// BundleHeads returns the ref-to-object map the bundle itself declares.
//
// This is the other half of the restore proof. The signed manifest fixes the bundle's bytes;
// this says which refs those bytes claim to carry. Because git is content-addressed, a
// commit id transitively covers its tree, its blobs and its whole ancestry, so comparing
// these against the refs a restored repository actually has is not a sample of the history,
// it is exact equality of it.
func (g *Git) BundleHeads(ctx context.Context, bundlePath string) ([]BundleRef, error) {
	scratch, abs, cleanup, err := g.scratchBundleRepo(ctx, bundlePath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out, err := g.output(ctx, scratch, "bundle", "list-heads", abs)
	if err != nil {
		return nil, fmt.Errorf("list bundle heads: %w", err)
	}
	return parseBundleHeads(out)
}

// parseBundleHeads reads `git bundle list-heads` output: one "<oid> <name>" per line.
//
// A line it cannot read is an error rather than a line to skip. Skipping would drop a ref
// from the declared set, and a ref that is never declared is a ref the comparison can never
// find missing — silently narrowing the proof to whatever happened to parse.
func parseBundleHeads(out string) ([]BundleRef, error) {
	var refs []BundleRef
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		oid, name, err := parseOIDName(line)
		if err != nil {
			return nil, fmt.Errorf("unreadable bundle header line: %w", err)
		}
		refs = append(refs, BundleRef{Name: name, OID: oid})
	}
	return refs, nil
}

// parseOIDName splits one "<oid> <name>" line, as both `bundle list-heads` and the
// for-each-ref format below produce.
//
// Exactly two fields, because a ref name can hold neither whitespace nor a control character
// — git check-ref-format forbids both — so a third field means the line is not the line it
// was taken for. Splitting on the first space alone would turn "aa11 refs/heads/main junk"
// into a ref named "refs/heads/main junk", inventing a ref that no repository can have and
// then reporting it missing.
//
// Control characters are refused for a second reason: these names are printed to a terminal
// and put into error messages, and with no public key configured the bundle they came from
// is unauthenticated. An escape sequence in a ref name is a way to write on top of the line
// that says how many refs matched.
func parseOIDName(line string) (oid, name string, err error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("want %q, got %d fields in %q", "<oid> <ref>", len(fields), line)
	}
	oid, name = fields[0], fields[1]
	if !isHex(oid) {
		return "", "", fmt.Errorf("object id %q is not hexadecimal", oid)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", "", fmt.Errorf("ref name %q contains a control character", name)
		}
	}
	return oid, name, nil
}

// isHex reports whether s is a plausible object id: non-empty and hex throughout. Length is
// not pinned, so sha256-object-format repositories are read the same way.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// ListRefs returns every ref in repoDir mapped to the object it points at.
//
// %(objectname) is the ref's own target and not the peeled one. A bundle header records an
// annotated tag as its tag object, so the two compare directly; peeling here
// (%(*objectname)) would compare a tag against the commit under it and accept a repository
// whose tag object had been replaced with a different one over the same commit.
func (g *Git) ListRefs(ctx context.Context, repoDir string) (map[string]string, error) {
	out, err := g.output(ctx, repoDir, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		oid, name, err := parseOIDName(line)
		if err != nil {
			return nil, fmt.Errorf("unreadable ref line: %w", err)
		}
		refs[name] = oid
	}
	return refs, nil
}

// HeadOID resolves repoDir's HEAD to the object it points at. It fails on an unborn HEAD,
// which for a repository cloned from a non-empty bundle cannot happen.
func (g *Git) HeadOID(ctx context.Context, repoDir string) (string, error) {
	out, err := g.output(ctx, repoDir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	oid := strings.TrimSpace(out)
	if !isHex(oid) {
		return "", fmt.Errorf("unreadable HEAD %q", oid)
	}
	return oid, nil
}

// CloneFromBundle restores a repo by cloning from a bundle file.
//
// --origin is pinned rather than left to default. `clone.defaultRemoteName` in an operator's
// ~/.gitconfig renames the remote, and with it the whole refs/remotes/<name>/* namespace that
// a clone files every branch under. That would make the shape of a restore depend on the
// machine it ran on, and the ref comparison — which looks for a declared branch under
// refs/heads/X or refs/remotes/origin/X — report a perfectly good restore as missing every
// branch but one. Same reasoning as the local LFS filters: in a disaster the machine is new,
// and a restore must not depend on how it happens to be configured.
func (g *Git) CloneFromBundle(ctx context.Context, bundlePath, dir string) error {
	return g.run(ctx, "", nil, "clone", "--quiet", "--origin", "origin", "--", bundlePath, dir)
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

// LFSInstallLocal writes the lfs clean/smudge filters into repoDir's own config.
//
// A repository cloned from a bundle has no filter.lfs.* configuration, and without it
// "git lfs checkout" exits 0 and does nothing — the working tree keeps 130-byte pointer
// files. That makes a successful restore depend on whether the operator had already run
// "git lfs install" on the machine, which in a disaster is exactly the machine that is new.
//
// --skip-repo, because a restored copy needs the filters, not the pre-push hook.
func (g *Git) LFSInstallLocal(ctx context.Context, repoDir string) error {
	return g.run(ctx, repoDir, nil, "lfs", "install", "--local", "--skip-repo")
}

// LFSCheckout materializes LFS files in the working tree from local objects (no network).
func (g *Git) LFSCheckout(ctx context.Context, repoDir string) error {
	return g.run(ctx, repoDir, nil, "lfs", "checkout")
}

// LFSPointersRemaining lists tracked paths that are still pointer files.
//
// Checked rather than assumed: "git lfs checkout" reports success whether or not it replaced
// anything, so its exit code says a command ran, not that the bytes are there. This reads the
// working tree back and is what lets restore fail instead of handing over pointers.
func (g *Git) LFSPointersRemaining(ctx context.Context, repoDir string) ([]string, error) {
	out, err := g.output(ctx, repoDir, "lfs", "ls-files", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("list lfs files: %w", err)
	}

	var stillPointers []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// A pointer file is a few lines of text beginning with a fixed version line.
		f, err := os.Open(filepath.Join(repoDir, name))
		if err != nil {
			// Not on disk at all is a different failure, and not one this check owns.
			continue
		}
		head := make([]byte, len(lfsPointerMagic))
		n, _ := io.ReadFull(f, head)
		if cerr := f.Close(); cerr != nil {
			return nil, fmt.Errorf("close %s: %w", name, cerr)
		}
		if n == len(lfsPointerMagic) && string(head) == lfsPointerMagic {
			stillPointers = append(stillPointers, name)
		}
	}
	return stillPointers, nil
}

// The first bytes of every git-lfs pointer file, per the v1 spec.
const lfsPointerMagic = "version https://git-lfs.github.com/spec/v1"

// output runs git and returns stdout. Same construction and environment as run.
func (g *Git) output(ctx context.Context, workdir string, args ...string) (string, error) {
	// audited: g.bin is the constant "git" and args are an argv array (no shell).
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, g.bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Env = append(baseEnv(), "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	g.logger.Debug("git", "args", args, "dir", workdir)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
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
