//go:build scale

package scale

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// make scale-image: the image built from this tree runs as a container under a memory limit, the
// way the engine runs in production, with cgroupwatch as its entrypoint reading the container's
// cgroup from inside. SCALE_IMAGE names the image and SCALE_MEMORY the limit.
//
// The engine reaches the stack's proxy on the compose network, and the fake forge, which runs in
// this process, at host.docker.internal.

// alpineImage is the image compose.yaml runs s3limits in, used here to prepare a volume.
const alpineImage = "alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"

// cgroupSummary is the line cgroupwatch prints; see scale/cgroupwatch.
type cgroupSummary struct {
	Limit         int64        `json:"limit"`
	Peak          int64        `json:"peak"`
	OOM           int64        `json:"oom"`
	OOMKill       int64        `json:"oomKill"`
	MaxCurrent    int64        `json:"maxCurrent"`
	MaxAnon       int64        `json:"maxAnon"`
	MaxFile       int64        `json:"maxFile"`
	MaxFileMapped int64        `json:"maxFileMapped"`
	MaxResident   int64        `json:"maxResident"`
	MaxWorkingSet int64        `json:"maxWorkingSet"`
	MaxScratch    int64        `json:"maxScratch"`
	Processes     []procMemory `json:"processes"`
}

type procMemory struct {
	Cmd     string `json:"cmd"`
	HWM     int64  `json:"hwm"`
	MaxAnon int64  `json:"maxAnon"`
	MaxFile int64  `json:"maxFile"`
}

func (h *harness) startContainer(t *testing.T, sc *scenario, spec runSpec) *engineProc {
	t.Helper()
	st, f := h.needStack(t), h.needForge(t)
	name := strings.ToLower("gitdr-scale-" + h.runID + "-" + sc.ID + "-" + slugify(spec.name))
	volume := name + "-scratch"
	// The image runs as 65532, and a fresh volume belongs to root.
	for _, args := range [][]string{
		{"volume", "create", volume},
		{"run", "--rm", "-v", volume + ":/scratch", alpineImage, "chown", "65532:65532", "/scratch"},
	} {
		if out, err := h.docker(args...); err != nil {
			t.Fatalf("harness: docker %s: %v: %s", args[0], err, out)
		}
	}

	u, err := url.Parse(f.base)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.engineConfig(spec)
	plain := map[string]string{
		"TMPDIR":                             "/scratch",
		"AWS_CA_BUNDLE":                      "/certs/ca.pem",
		"AWS_REGION":                         "us-east-1",
		"AWS_EC2_METADATA_DISABLED":          "true",
		"GITDR_SOURCE_TYPE":                  "github",
		"GITDR_SOURCE_BASEURL":               "http://host.docker.internal:" + u.Port() + "/api/v3",
		"GITDR_SOURCE_REPO":                  spec.repo,
		"GITDR_SOURCE_GITHUB_APPID":          strconv.FormatInt(cfg.Source.GitHub.AppID, 10),
		"GITDR_SOURCE_GITHUB_INSTALLATIONID": strconv.FormatInt(cfg.Source.GitHub.InstallationID, 10),
		"GITDR_DESTINATION_TYPE":             "s3",
		"GITDR_DESTINATION_S3_BUCKET":        spec.bucket,
		"GITDR_DESTINATION_S3_REGION":        "us-east-1",
		"GITDR_DESTINATION_S3_ENDPOINT":      "https://s3limits:9443",
		"GITDR_DESTINATION_S3_USEPATHSTYLE":  "true",
		"GITDR_DESTINATION_RETENTION_MODE":   cfg.Destination.Retention.Mode,
		"GITDR_DESTINATION_RETENTION_DAYS":   strconv.Itoa(cfg.Destination.Retention.Days),
		"GITDR_BACKUP_CONCURRENCY":           strconv.Itoa(cfg.Backup.Concurrency),
		"GITDR_BACKUP_RESUME":                "true",
		"GITDR_BACKUP_LFS":                   "true",
		"GITDR_LOG_FORMAT":                   "json",
		"GITDR_LOG_LEVEL":                    "info",
	}
	signingKey, err := os.ReadFile(h.signerKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Passed by name, so the values travel in docker's environment and never on its command line.
	secrets := map[string]string{
		"AWS_ACCESS_KEY_ID":            st.user,
		"AWS_SECRET_ACCESS_KEY":        st.pass,
		"GITDR_GITHUB_APP_PRIVATE_KEY": string(f.appKeyPEM),
		"GITDR_MANIFEST_SIGNING_KEY":   string(signingKey),
	}
	args := []string{"run", "--name", name,
		"--network", st.project + "_default", "--add-host", "host.docker.internal:host-gateway",
		"--memory", h.prof.MemoryLimit, "--memory-swap", h.prof.MemoryLimit,
		"-v", st.binDir + ":/opt/scale:ro", "-v", filepath.Join(h.root, "certs") + ":/certs:ro", "-v", volume + ":/scratch",
		"--entrypoint", "/opt/scale/cgroupwatch",
	}
	for _, k := range sortedKeys(plain) {
		args = append(args, "-e", k+"="+plain[k])
	}
	env := slices.Clone(h.origEnv)
	for _, k := range sortedKeys(secrets) {
		args = append(args, "-e", k)
		env = append(env, k+"="+secrets[k])
	}
	args = append(args, h.prof.Image, "-du", "/scratch", "/usr/bin/gitdr", "backup", "-output", "json")

	p := &engineProc{t: t, sc: sc, spec: spec, done: make(chan struct{}), h: h, container: name, volume: volume}
	p.logPath = filepath.Join(h.logDir, sc.ID+"-"+slugify(spec.name)+".log")
	logFile, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command("docker", args...)
	p.cmd.Env = env
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = logFile
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.measure = h.startMeasure(t, h.scratchDir(t, sc, spec)) // the scratch is the volume; cgroupwatch measures it
	p.start = time.Now()
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("harness: starting the image: %v", err)
	}
	go func() {
		_ = p.cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	return p
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// finishContainer reads what the container left behind into the phase, then removes it.
func (p *engineProc) finishContainer(t *testing.T, ph *phase) {
	t.Helper()
	if out, err := p.h.docker("inspect", "-f", "{{.State.OOMKilled}}", p.container); err == nil {
		ph.OOMKilled = strings.TrimSpace(out) == "true"
	}
	_, _ = p.h.docker("rm", "-f", p.container)
	_, _ = p.h.docker("volume", "rm", "-f", p.volume)

	raw, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Errorf("harness: %v", err)
		return
	}
	var cg *cgroupSummary
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "cgroupwatch "); ok {
			var s cgroupSummary
			if json.Unmarshal([]byte(rest), &s) == nil {
				cg = &s
			}
		}
	}
	if cg == nil {
		t.Errorf("harness: the container printed no cgroupwatch summary; see %s", p.logPath)
		return
	}
	ph.MemoryLimitBytes, ph.MemoryPeakBytes, ph.OOMKills = cg.Limit, cg.Peak, cg.OOMKill
	ph.MaxAnonBytes, ph.MaxFileBytes, ph.MaxFileMappedBytes = cg.MaxAnon, cg.MaxFile, cg.MaxFileMapped
	ph.MaxResidentBytes, ph.MaxWorkingSetBytes = cg.MaxResident, cg.MaxWorkingSet
	ph.PeakScratchBytes = cg.MaxScratch
	ph.Processes = cg.Processes
	for _, pr := range cg.Processes {
		ph.MaxRSSBytes = max(ph.MaxRSSBytes, pr.HWM)
	}
}

// memoryNote is what a phase under a memory limit says about it, for a note.
func memoryNote(p *phase) string {
	var top []string
	for i, pr := range p.Processes {
		if i == 4 {
			break
		}
		top = append(top, fmt.Sprintf("%s %s (anon %s, file %s)", pr.Cmd, size(pr.HWM), size(pr.MaxAnon), size(pr.MaxFile)))
	}
	return fmt.Sprintf("under a %s limit: resident at most %s (anon %s, mapped %s), working set %s, memory.peak %s with the page cache (file pages %s), oom kills %d, OOMKilled %v; largest processes: %s",
		size(p.MemoryLimitBytes), size(p.MaxResidentBytes), size(p.MaxAnonBytes), size(p.MaxFileMappedBytes), size(p.MaxWorkingSetBytes),
		size(p.MemoryPeakBytes), size(p.MaxFileBytes), p.OOMKills, p.OOMKilled, strings.Join(top, "; "))
}
