//go:build scale

package scale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"gitdr.io/gitdr/internal/config"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/pipeline"
	ghsrc "gitdr.io/gitdr/internal/source/github"
)

// Two ways to run the engine. In this process, through pipeline.Backup with an injected clock,
// which is the only way to cross a simulated day: the binary has no flag that moves its date and
// must never get one. Or as the gitdr binary built from this tree, which is what a signal can
// stop and what has a resident set of its own to measure.

type runSpec struct {
	name   string // the phase, as the report names it
	org    *org
	bucket string
	repo   string    // a single-repository run of owner/name; "" for the whole installation
	at     time.Time // simulated start; zero for the real clock
	// process runs the gitdr binary instead of the pipeline in this process.
	process bool
	timeout time.Duration
}

type runResult struct {
	manifest    *pipeline.Manifest
	manifestKey string
	err         error
	exitCode    int
	phase       *phase
}

// entries is the manifest's repository entries, or none when the run filed no manifest.
func (r *runResult) entries() []pipeline.RepoEntry {
	if r.manifest == nil {
		return nil
	}
	return r.manifest.Repos
}

func (r *runResult) entry(slug string) *pipeline.RepoEntry {
	for i, e := range r.entries() {
		if e.Slug == slug {
			return &r.entries()[i]
		}
	}
	return nil
}

func (h *harness) engineConfig(spec runSpec) *config.Config {
	f, st := h.forge, h.stack
	cfg := config.Default()
	cfg.Source.Type = "github"
	cfg.Source.BaseURL = f.apiURL()
	cfg.Source.Repo = spec.repo
	cfg.Source.GitHub = config.GitHubConfig{AppID: f.appID, InstallationID: spec.org.install, PrivateKeyPath: f.appKeyPath}
	cfg.Destination.Type = "s3"
	cfg.Destination.S3 = config.S3Config{Bucket: spec.bucket, Region: "us-east-1", Endpoint: st.s3URL, UsePathStyle: true}
	cfg.Destination.Retention = config.RetentionConfig{Mode: "COMPLIANCE", Days: 30}
	cfg.Backup = config.BackupConfig{Concurrency: h.prof.Concurrency, Resume: true, LFS: true}
	cfg.Manifest.SigningKeyPath = h.signerKeyPath
	cfg.Log = config.LogConfig{Level: "info", Format: "json"}
	return cfg
}

func (h *harness) scratchDir(t *testing.T, sc *scenario, spec runSpec) string {
	t.Helper()
	dir := filepath.Join(h.root, "scratch", sc.ID+"-"+slugify(spec.name))
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func slugify(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return '-'
	}, s)
}

// backup runs the engine once and records the phase in the scenario.
func (h *harness) backup(t *testing.T, sc *scenario, spec runSpec) *runResult {
	t.Helper()
	if spec.process {
		if h.prof.Image != "" {
			return h.startContainer(t, sc, spec).wait(t, spec.timeout)
		}
		return h.startProcess(t, sc, spec).wait(t, spec.timeout)
	}
	return h.runInProcess(t, sc, spec)
}

func (h *harness) runInProcess(t *testing.T, sc *scenario, spec runSpec) *runResult {
	t.Helper()
	st, f := h.needStack(t), h.needForge(t)
	scratch := h.scratchDir(t, sc, spec)
	t.Setenv("TMPDIR", scratch)
	logPath := filepath.Join(h.logDir, sc.ID+"-"+slugify(spec.name)+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	log := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), cmp(spec.timeout, 3*time.Hour))
	defer cancel()
	src, err := ghsrc.New(ghsrc.Options{BaseURL: f.apiURL(), AppID: f.appID, InstallationID: spec.org.install, PrivateKeyPEM: f.appKeyPEM}, log)
	if err != nil {
		t.Fatalf("harness: github source: %v", err)
	}
	dst, err := s3backend.New(ctx, s3backend.Options{Bucket: spec.bucket, Region: "us-east-1", Endpoint: st.s3URL, UsePathStyle: true}, log)
	if err != nil {
		t.Fatalf("harness: s3 destination: %v", err)
	}

	m := h.startMeasure(t, scratch)
	start := time.Now()
	now := time.Now
	if !spec.at.IsZero() {
		now = func() time.Time { return spec.at.Add(time.Since(start)) }
	}
	res, err := pipeline.Backup(ctx, pipeline.BackupDeps{
		Config: h.engineConfig(spec), Source: src, Dest: dst, Git: gitexec.New(log),
		SigningKey: h.signer, ToolVersion: "scale", Logger: log, Now: now,
	})
	r := &runResult{err: err}
	if err != nil {
		r.exitCode = 1
	}
	if res != nil {
		r.manifest, r.manifestKey = res.Manifest, res.ManifestKey
	}
	p := m.finish(t, r, time.Since(start))
	p.Name, p.Mode, p.LogFile = spec.name, "in-process", logPath
	if !spec.at.IsZero() {
		p.SimulatedAt = spec.at.UTC().Format(time.RFC3339)
	}
	r.phase = p
	sc.addPhase(p)
	t.Logf("%s: %s", spec.name, describe(r))
	return r
}

// engineProc is one gitdr binary running.
type engineProc struct {
	t       *testing.T
	sc      *scenario
	spec    runSpec
	cmd     *exec.Cmd
	stdout  bytes.Buffer
	logPath string
	measure *measure
	start   time.Time
	done    chan struct{}
	signal  string
	// A run of the image (make scale-image): the container and its scratch volume.
	h                 *harness
	container, volume string
}

func (h *harness) startProcess(t *testing.T, sc *scenario, spec runSpec) *engineProc {
	t.Helper()
	bin := h.needBinary(t)
	h.needStack(t)
	h.needForge(t)
	scratch := h.scratchDir(t, sc, spec)
	cfgYAML, err := yaml.Marshal(h.engineConfig(spec))
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := scratch + ".yaml"
	if err := os.WriteFile(cfgPath, cfgYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &engineProc{t: t, sc: sc, spec: spec, done: make(chan struct{})}
	p.logPath = filepath.Join(h.logDir, sc.ID+"-"+slugify(spec.name)+".log")
	logFile, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command(bin, "backup", "-config", cfgPath, "-output", "json")
	p.cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = logFile
	// Its own process group, so a kill can take git and git-lfs with it, as the end of a
	// container does.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.measure = h.startMeasure(t, scratch)
	p.start = time.Now()
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("harness: starting gitdr: %v", err)
	}
	go func() {
		_ = p.cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	return p
}

// stop sends sig to the engine, and to its whole process group for SIGKILL.
func (p *engineProc) stop(sig syscall.Signal) {
	p.signal = sig.String()
	if p.container != "" {
		// cgroupwatch passes SIGTERM on to the engine; SIGKILL ends the container, all of it.
		_, _ = p.h.docker("kill", "--signal", dockerSignal(sig), p.container)
		return
	}
	if sig == syscall.SIGKILL {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		return
	}
	_ = p.cmd.Process.Signal(sig)
}

// exited waits up to limit for the engine to exit by itself.
func (p *engineProc) exited(limit time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(limit):
		return false
	}
}

func (p *engineProc) wait(t *testing.T, limit time.Duration) *runResult {
	t.Helper()
	if !p.exited(cmp(limit, 3*time.Hour)) {
		t.Errorf("harness: gitdr was still running after %s; killed", cmp(limit, 3*time.Hour))
		if p.container != "" {
			_, _ = p.h.docker("kill", p.container)
		}
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
	// Whatever it left running goes too, as it would when its container ends.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	wall := time.Since(p.start)

	r := &runResult{exitCode: p.cmd.ProcessState.ExitCode()}
	var out struct {
		pipeline.Manifest
		ManifestKey string `json:"manifestKey"`
	}
	if p.stdout.Len() > 0 {
		if err := json.Unmarshal(p.stdout.Bytes(), &out); err != nil {
			t.Errorf("harness: gitdr printed something that is not its JSON result: %v", err)
		} else {
			m := out.Manifest
			r.manifest, r.manifestKey = &m, out.ManifestKey
		}
	}
	if r.exitCode != 0 {
		r.err = fmt.Errorf("gitdr exited %d%s: %s", r.exitCode, signalNote(p.signal), lastError(p.logPath))
	}
	ph := p.measure.finish(t, r, wall)
	ph.Name, ph.Mode, ph.LogFile, ph.Signal = p.spec.name, "process", p.logPath, p.signal
	if p.container != "" {
		ph.Mode = "image"
		p.finishContainer(t, ph)
	} else if ru, ok := p.cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		ph.MaxRSSBytes = ru.Maxrss
		if runtime.GOOS == "linux" {
			ph.MaxRSSBytes *= 1024 // kilobytes there, bytes on darwin
		}
	}
	r.phase = ph
	p.sc.addPhase(ph)
	t.Logf("%s: %s", p.spec.name, describe(r))
	return r
}

// dockerSignal is a signal's name as docker kill takes it: TERM, where sig.String() says
// "terminated".
func dockerSignal(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGINT:
		return "INT"
	default:
		return "TERM"
	}
}

func signalNote(sig string) string {
	if sig == "" {
		return ""
	}
	return " after " + sig
}

// lastError is the last error line gitdr logged, for a message.
func lastError(logPath string) string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Err   string `json:"err"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) == nil && rec.Level == "ERROR" {
			return strings.TrimSpace(rec.Msg + ": " + rec.Err)
		}
	}
	return ""
}

func describe(r *runResult) string {
	p := r.phase
	s := fmt.Sprintf("%.1fs, %d repositories: %d copied, %d skipped, %d failed", p.WallSeconds, p.Repos, p.Copied, p.Skipped, p.Failed)
	if r.err != nil {
		s += "; " + r.err.Error()
	}
	return s
}

func cmp(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// measure takes the counters before a run and the difference after it.
type measure struct {
	h           *harness
	s3Before    s3Stats
	forgeBefore forgeStats
	scratch     *sampler
}

func (h *harness) startMeasure(t *testing.T, scratch string) *measure {
	t.Helper()
	st, err := h.stack.stats()
	if err != nil {
		t.Fatalf("harness: proxy stats: %v", err)
	}
	return &measure{h: h, s3Before: st, forgeBefore: h.forge.stats(), scratch: startSampler(scratch)}
}

func (m *measure) finish(t *testing.T, r *runResult, wall time.Duration) *phase {
	t.Helper()
	p := &phase{WallSeconds: wall.Seconds(), ExitCode: r.exitCode, PeakScratchBytes: m.scratch.stop()}
	if r.err != nil {
		p.Error = r.err.Error()
	}
	for _, e := range r.entries() {
		p.Repos++
		switch e.Status {
		case pipeline.StatusFailed:
			p.Failed++
		case pipeline.StatusSuccess:
			p.Copied++
		default:
			p.Skipped++
		}
	}
	if r.manifest != nil && r.manifestKey != "" {
		p.ManifestKey = r.manifestKey
		if b, err := r.manifest.Canonical(); err == nil {
			p.ManifestBytes = int64(len(b))
		}
	}
	after, err := m.h.stack.stats()
	if err != nil {
		t.Errorf("harness: proxy stats: %v", err)
		return p
	}
	p.S3Requests, p.S3Refused = map[string]int64{}, map[string]int64{}
	for op, s := range after.Ops {
		if d := s.Count - m.s3Before.Ops[op].Count; d > 0 {
			p.S3Requests[op] = d
		}
		p.S3BytesIn += s.BytesIn - m.s3Before.Ops[op].BytesIn
		p.S3BytesOut += s.BytesOut - m.s3Before.Ops[op].BytesOut
	}
	for code, n := range after.Refused {
		if d := n - m.s3Before.Refused[code]; d > 0 {
			p.S3Refused[code] = d
		}
	}
	p.S3OpenUploads = after.OpenUploads
	p.BytesSentTwice = after.BytesResent - m.s3Before.BytesResent
	fa := m.h.forge.stats()
	p.APIRequests = map[string]int64{}
	for k, n := range fa.Requests {
		if d := n - m.forgeBefore.Requests[k]; d > 0 {
			p.APIRequests[k] = d
		}
	}
	p.APIRateLimited = fa.RateLimited - m.forgeBefore.RateLimited
	p.APIBudgetSpent = fa.BudgetSpent - m.forgeBefore.BudgetSpent
	return p
}

// sampler watches a directory's size and keeps the peak.
type sampler struct {
	dir  string
	peak atomic.Int64
	quit chan struct{}
	done chan struct{}
}

func startSampler(dir string) *sampler {
	s := &sampler{dir: dir, quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			if n := dirSize(s.dir); n > s.peak.Load() {
				s.peak.Store(n)
			}
			select {
			case <-s.quit:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *sampler) stop() int64 {
	close(s.quit)
	<-s.done
	return s.peak.Load()
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // gone between the listing and the stat: the engine cleans up as it goes
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// ---- reading back what a run wrote ----

// bucketManifests reads every manifest filed directly in dir, oldest first.
func (h *harness) bucketManifests(t *testing.T, bucket, dir string) map[string]*pipeline.Manifest {
	t.Helper()
	ctx := context.Background()
	d, err := h.stack.dest(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := d.List(ctx, dir+"/")
	if err != nil {
		t.Fatalf("harness: list %s: %v", dir, err)
	}
	out := map[string]*pipeline.Manifest{}
	for _, o := range objs {
		if path.Dir(o.Key) != dir || !strings.HasSuffix(o.Key, ".manifest.json") {
			continue
		}
		rc, err := d.Get(ctx, o.Key)
		if err != nil {
			t.Fatalf("harness: get %s: %v", o.Key, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("harness: read %s: %v", o.Key, err)
		}
		var m pipeline.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("harness: %s is not a manifest: %v", o.Key, err)
		}
		out[o.Key] = &m
	}
	return out
}

// keysUnder lists the object keys under prefix.
func (h *harness) keysUnder(t *testing.T, bucket, prefix string) []string {
	t.Helper()
	ctx := context.Background()
	d, err := h.stack.dest(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := d.List(ctx, prefix)
	if err != nil {
		t.Fatalf("harness: list %s: %v", prefix, err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	return keys
}
