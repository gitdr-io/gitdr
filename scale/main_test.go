//go:build scale

// Package scale backs up many and large repositories through the engine and measures what it
// does: a fake GitHub serving thousands of repositories from disk, and MinIO with Object Lock
// behind a proxy that enforces AWS's limits. It is not part of `make ci`. It needs Docker, and
// a full run takes hours. See README.md.
package scale

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
)

var h *harness

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	var err error
	h, err = newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scale:", err)
		return 1
	}
	// An interrupted run still takes its stack down. A run killed outright does not; that is
	// what `make scale-down` is for.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "scale: interrupted; taking the stack down")
		h.close()
		os.Exit(2)
	}()
	code := m.Run()
	h.close()
	return code
}

// profile is the size of the run, from SCALE_* variables.
type profile struct {
	Repos         int    `json:"repos"`
	ChangePercent int    `json:"changePercent"`
	RefsRepos     int    `json:"refsRepos"`
	RefsPerRepo   int    `json:"refsPerRepo"`
	KillRepos     int    `json:"killRepos"`
	RateRepos     int    `json:"rateRepos"`
	RateLimit     int    `json:"rateLimit"`
	RateWindow    string `json:"rateWindow"`
	Big           bool   `json:"big"`
	BigPackBytes  int64  `json:"bigPackBytes"`
	BigLFSBytes   int64  `json:"bigLfsBytes"`
	S3Rate        int64  `json:"s3RateBytesPerSecond"`
	RechunkBytes  int64  `json:"rechunkBytes"`
	// Image is the image a run of the binary uses instead (make scale-image), under MemoryLimit.
	Image       string `json:"image,omitempty"`
	MemoryLimit string `json:"memoryLimit,omitempty"`
	Concurrency int    `json:"concurrency"`
	Keep        bool   `json:"keep"`
	rateWindow  time.Duration
	resultsDir  string
}

func loadProfile(moduleRoot string) (profile, error) {
	p := profile{
		Repos:         envInt("SCALE_REPOS", 2500),
		ChangePercent: envInt("SCALE_CHANGE_PERCENT", 30),
		RefsRepos:     envInt("SCALE_REFS_REPOS", 5),
		RefsPerRepo:   envInt("SCALE_REFS_PER_REPO", 100_000),
		KillRepos:     envInt("SCALE_KILL_REPOS", 12),
		RateRepos:     envInt("SCALE_RATE_REPOS", 16),
		RateLimit:     envInt("SCALE_RATE_LIMIT", 40),
		Big:           os.Getenv("SCALE_BIG") == "1",
		BigPackBytes:  int64(envInt("SCALE_BIG_PACK_BYTES", 6<<30)),
		BigLFSBytes:   int64(envInt("SCALE_BIG_LFS_BYTES", 8<<30)),
		S3Rate:        int64(envInt("SCALE_S3_RATE", 8_000_000)),
		RechunkBytes:  int64(envInt("SCALE_RECHUNK_BYTES", 8<<20)),
		Image:         os.Getenv("SCALE_IMAGE"),
		MemoryLimit:   envStr("SCALE_MEMORY", "4g"),
		Concurrency:   envInt("SCALE_CONCURRENCY", 4),
		Keep:          os.Getenv("SCALE_KEEP") == "1",
		resultsDir:    os.Getenv("SCALE_RESULTS_DIR"),
	}
	w, err := time.ParseDuration(envStr("SCALE_RATE_WINDOW", "10s"))
	if err != nil {
		return p, fmt.Errorf("SCALE_RATE_WINDOW: %w", err)
	}
	p.rateWindow, p.RateWindow = w, w.String()
	if p.resultsDir == "" {
		p.resultsDir = filepath.Join(moduleRoot, "scale-results")
	}
	if p.Repos < 10 || p.KillRepos < 8 || p.RateRepos < 4 || p.RefsRepos < 1 {
		return p, errors.New("SCALE_REPOS must be at least 10, SCALE_KILL_REPOS 8, SCALE_RATE_REPOS 4, SCALE_REFS_REPOS 1")
	}
	return p, nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type harness struct {
	prof       profile
	moduleRoot string
	root       string // everything temporary: fixtures, scratch, certificates, binaries
	runID      string
	reportPath string
	logDir     string
	// origEnv is the environment the test started with. docker and go run with it; git, and the
	// engine, run with the isolated one this process switches to.
	origEnv []string

	signer        ed25519.PrivateKey
	signerKeyPath string

	report *report

	stackOnce sync.Once
	stack     *stack
	stackErr  error

	forgeOnce sync.Once
	forge     *forge
	forgeErr  error

	binOnce  sync.Once
	gitdrBin string
	binErr   error

	closeOnce sync.Once
}

func newHarness() (*harness, error) {
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		return nil, fmt.Errorf("run from the scale directory of the engine module: %w", err)
	}
	prof, err := loadProfile(moduleRoot)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git is not installed")
	}
	root, err := os.MkdirTemp("", "gitdr-scale-")
	if err != nil {
		return nil, err
	}
	h := &harness{
		prof:       prof,
		moduleRoot: moduleRoot,
		root:       root,
		runID:      time.Now().UTC().Format("20060102T150405Z"),
		origEnv:    os.Environ(),
	}
	h.reportPath = filepath.Join(prof.resultsDir, h.runID+".json")
	h.logDir = filepath.Join(prof.resultsDir, h.runID)
	if err := os.MkdirAll(h.logDir, 0o755); err != nil {
		return nil, err
	}
	if err := h.isolate(); err != nil {
		return nil, err
	}

	_, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	if h.signer, err = crypto.ParsePrivateKey(privPEM); err != nil {
		return nil, err
	}
	h.signerKeyPath = filepath.Join(root, "manifest-signing.pem")
	if err := os.WriteFile(h.signerKeyPath, privPEM, 0o600); err != nil {
		return nil, err
	}

	h.report = &report{
		Schema:    reportSchema,
		RunID:     h.runID,
		StartedAt: time.Now().UTC(),
		Engine:    h.engineCommit(),
		Profile:   prof,
		Host: hostInfo{
			OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Go: runtime.Version(),
			Git: commandLine("git", "--version"), GitLFS: commandLine("git", "lfs", "version"),
		},
	}
	fmt.Fprintf(os.Stderr, "scale: run %s, engine %s, %d small repositories, work in %s\n", h.runID, h.report.Engine, prof.Repos, root)
	return h, nil
}

// isolate keeps the machine's own git and AWS setup out of the run: no credential helper, no
// proxy, no profile, no instance metadata. Git reads HOME; the AWS SDK reads its config files.
func (h *harness) isolate() error {
	home := filepath.Join(h.root, "home")
	aws := filepath.Join(h.root, "aws")
	for _, d := range []string{home, filepath.Join(home, ".config"), aws} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{"config", "credentials"} {
		if err := os.WriteFile(filepath.Join(aws, f), nil, 0o600); err != nil {
			return err
		}
	}
	// The variables below keep the system config and the askpass programs away from the harness's
	// own git. The engine's git may not get them: a gitdr that passes git only an allowlist of its
	// environment, HOME and not these, leaves that git reading the system config, and Homebrew's
	// sets credential.helper=osxkeychain. An empty helper in HOME's config empties the list of
	// helpers read before it, so no helper of the machine's is ever asked for the forge's
	// credentials.
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper =\n"), 0o600); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"HOME":                        home,
		"XDG_CONFIG_HOME":             filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM":         "1",
		"GIT_TERMINAL_PROMPT":         "0",
		"GIT_ASKPASS":                 "",
		"SSH_ASKPASS":                 "",
		"AWS_CONFIG_FILE":             filepath.Join(aws, "config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(aws, "credentials"),
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_REGION":                  "us-east-1",
	} {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CA_BUNDLE", "GIT_DIR", "GIT_WORK_TREE"} {
		_ = os.Unsetenv(k)
	}
	return nil
}

// engineCommit names the engine under test: the module's HEAD, marked when the tree is dirty.
func (h *harness) engineCommit() string {
	out, err := exec.Command("git", "-C", h.moduleRoot, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "-C", h.moduleRoot, "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(st) > 0 {
		commit += "-dirty"
	}
	return commit
}

func commandLine(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// close writes the report and takes down what the run started. Safe to call twice.
func (h *harness) close() {
	h.closeOnce.Do(func() {
		h.writeReport()
		if h.forge != nil {
			h.forge.close()
		}
		if h.stack != nil {
			if h.prof.Keep {
				fmt.Fprintf(os.Stderr, "scale: SCALE_KEEP=1, leaving stack %s up and %s in place\n", h.stack.project, h.root)
				return
			}
			h.stack.down()
		}
		if !h.prof.Keep {
			_ = os.RemoveAll(h.root)
		}
	})
}

// needStack starts the compose stack on first use.
func (h *harness) needStack(t *testing.T) *stack {
	t.Helper()
	h.stackOnce.Do(func() { h.stack, h.stackErr = startStack(h) })
	if h.stackErr != nil {
		t.Fatalf("harness: the object store stack did not start: %v", h.stackErr)
	}
	return h.stack
}

// needForge starts the fake GitHub on first use.
func (h *harness) needForge(t *testing.T) *forge {
	t.Helper()
	h.forgeOnce.Do(func() { h.forge, h.forgeErr = startForge(h) })
	if h.forgeErr != nil {
		t.Fatalf("harness: the fake forge did not start: %v", h.forgeErr)
	}
	return h.forge
}

// needBinary builds the gitdr binary under test on first use.
func (h *harness) needBinary(t *testing.T) string {
	t.Helper()
	h.binOnce.Do(func() {
		out := filepath.Join(h.root, "bin", "gitdr")
		h.binErr = h.goBuild(out, "", "", "./cmd/gitdr")
		h.gitdrBin = out
	})
	if h.binErr != nil {
		t.Fatalf("harness: building gitdr: %v", h.binErr)
	}
	return h.gitdrBin
}

// goBuild builds pkg into out with the environment the test started with, so the build cache is
// the developer's own and not a cold one under the isolated HOME. SCALE_GO names the go command,
// as make's GO does.
func (h *harness) goBuild(out, goos, goarch, pkg string, extra ...string) error {
	args := append([]string{"build", "-trimpath", "-o", out}, extra...)
	cmd := exec.Command(envStr("SCALE_GO", "go"), append(args, pkg)...)
	cmd.Dir = h.moduleRoot
	cmd.Env = h.origEnv
	if goos != "" {
		cmd.Env = append(cmd.Env, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %w: %s", pkg, err, b)
	}
	return nil
}
