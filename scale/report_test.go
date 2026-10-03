//go:build scale

package scale

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// What the harness writes down: one JSON document per run under scale-results/, and a summary on
// stdout that says which checks failed and which known defect each failure is.

// finding is a known defect in the engine that a check demonstrates. A check that fails with a
// finding is expected to fail until that defect is fixed; one that fails without a finding is
// news.
type finding struct {
	Title string `json:"title"`
	Where string `json:"where"`
}

var findings = map[string]finding{
	"resume-trusts-objects": {
		Title: "a same-day rerun counts any repository with a bundle under today's date as backed up, whether or not a manifest records the copy, and the skip it records carries no refs or copiedAt",
		Where: "internal/pipeline/backup.go: alreadyBackedUp, backupOne",
	},
	"empty-repo-rerun": {
		Title: "a repository with no commits fails its same-day rerun: its meta.json exists and the store refuses the second write",
		Where: "internal/pipeline/backup.go: alreadyBackedUp looks for the bundle only",
	},
	"newest-manifest-only": {
		Title: "the next run reads only the newest manifest, so one single-repository run makes the next organisation run copy everything",
		Where: "internal/pipeline/previous.go: loadPrevious",
	},
	"manifest-read-cap": {
		Title: "a manifest over 32 MiB is not read back, so the run after it copies every repository again",
		Where: "internal/pipeline/previous.go: maxManifestBytes",
	},
	"stopped-run-no-manifest": {
		Title: "a backup stopped by SIGTERM files no manifest, so nothing records the copies it finished",
		Where: "internal/pipeline/backup.go: run uploads the manifest on the cancelled context",
	},
	"stop-waits-for-git": {
		Title: "after SIGTERM the engine waits on git's children: cancelling kills git, its HTTP helper keeps git's stderr pipe open, and nothing bounds the wait",
		Where: "internal/gitexec/gitexec.go: command sets no WaitDelay",
	},
	"minio-tls-chunk": {
		Title: "over TLS the AWS SDK sends a body of known length as one aws-chunked chunk, and MinIO refuses a chunk over 16 MiB, so no artifact or manifest over 16 MiB can be written to MinIO over TLS",
		Where: "internal/dest/s3/s3.go: PutImmutable asks for a CRC32, which the SDK sends as a trailer",
	},
	"git-memory-unbounded": {
		Title: "git runs with its default pack and mmap settings, so a big repository can need more memory than the container has",
		Where: "internal/gitexec/gitexec.go: commandEnv sets no pack or mmap limit",
	},
	"single-put-limit": {
		Title: "every artifact is one PutObject, which AWS refuses over 5 GiB",
		Where: "internal/dest/s3/s3.go: PutImmutable",
	},
	"rate-limit-not-waited": {
		Title: "a GitHub rate limit fails the repositories that meet it instead of being waited out",
		Where: "internal/source/github: no wait on a RateLimitError",
	},
}

type check struct {
	Name    string `json:"name"`
	Pass    bool   `json:"pass"`
	Detail  string `json:"detail,omitempty"`
	Finding string `json:"finding,omitempty"`
}

// phase is one engine run inside a scenario, with its measures.
type phase struct {
	Name        string  `json:"name"`
	Mode        string  `json:"mode"` // in-process or process
	SimulatedAt string  `json:"simulatedAt,omitempty"`
	WallSeconds float64 `json:"wallSeconds"`
	ExitCode    int     `json:"exitCode"`
	Error       string  `json:"error,omitempty"`
	Signal      string  `json:"signal,omitempty"`

	Repos   int `json:"repos"`
	Copied  int `json:"copied"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	// Recopied is the repositories copied in full although nothing changed since a copy that
	// was already recorded, and BytesSentTwice what that cost, with the proxy's own count of
	// bytes written twice to the same key added.
	Recopied       int   `json:"recopied"`
	BytesSentTwice int64 `json:"bytesSentTwice"`

	ManifestKey      string `json:"manifestKey,omitempty"`
	ManifestBytes    int64  `json:"manifestBytes"`
	PeakScratchBytes int64  `json:"peakScratchBytes"`
	// MaxRSSBytes is the peak resident set of the engine or of any git process it waited for,
	// as wait4 reports it. Only a run of the gitdr binary has one; an in-process run shares the
	// test's own memory.
	MaxRSSBytes int64 `json:"maxRSSBytes,omitempty"`

	// A run of the image under a memory limit (make scale-image), from its cgroup. memory.peak
	// counts page cache, which the kernel takes back before it kills anything; anonymous memory is
	// what a process cannot give back, and oom kills are the limit acting. Resident is anonymous
	// memory and mapped file pages at once, what the processes hold; the working set is the
	// kubelet's figure, memory.current less the inactive file pages.
	MemoryLimitBytes   int64        `json:"memoryLimitBytes,omitempty"`
	MemoryPeakBytes    int64        `json:"memoryPeakBytes,omitempty"`
	MaxResidentBytes   int64        `json:"maxResidentBytes,omitempty"`
	MaxWorkingSetBytes int64        `json:"maxWorkingSetBytes,omitempty"`
	MaxAnonBytes       int64        `json:"maxAnonBytes,omitempty"`
	MaxFileBytes       int64        `json:"maxFileBytes,omitempty"`
	MaxFileMappedBytes int64        `json:"maxFileMappedBytes,omitempty"`
	OOMKills           int64        `json:"oomKills,omitempty"`
	OOMKilled          bool         `json:"oomKilled,omitempty"`
	Processes          []procMemory `json:"processes,omitempty"`

	S3Requests    map[string]int64 `json:"s3Requests"`
	S3Refused     map[string]int64 `json:"s3Refused,omitempty"`
	S3BytesIn     int64            `json:"s3BytesIn"`
	S3BytesOut    int64            `json:"s3BytesOut"`
	S3OpenUploads int64            `json:"s3OpenUploads"`
	APIRequests   map[string]int64 `json:"apiRequests"`
	// APIBudgetSpent is how often the installation's rate-limit budget ran out, and APIRateLimited
	// how many requests the forge refused for it.
	APIBudgetSpent int64  `json:"apiBudgetSpent"`
	APIRateLimited int64  `json:"apiRateLimited"`
	LogFile        string `json:"logFile,omitempty"`
}

type scenario struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"` // pass, fail or skip
	WallSeconds float64  `json:"wallSeconds"`
	Phases      []*phase `json:"phases"`
	Checks      []check  `json:"checks"`
	Notes       []string `json:"notes,omitempty"`

	started time.Time
	mu      sync.Mutex
}

type hostInfo struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	CPUs   int    `json:"cpus"`
	Go     string `json:"go"`
	Git    string `json:"git"`
	GitLFS string `json:"gitLfs"`
}

type report struct {
	Schema     string      `json:"schema"`
	RunID      string      `json:"runId"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt time.Time   `json:"finishedAt"`
	Engine     string      `json:"engineCommit"`
	Host       hostInfo    `json:"host"`
	Profile    profile     `json:"profile"`
	Findings   any         `json:"findings"`
	Scenarios  []*scenario `json:"scenarios"`

	mu sync.Mutex
}

// begin registers a scenario and arranges for its status to be settled when the test ends.
func (h *harness) begin(t *testing.T, id, title string) *scenario {
	t.Helper()
	sc := &scenario{ID: id, Title: title, Status: "running", started: time.Now()}
	h.report.mu.Lock()
	h.report.Scenarios = append(h.report.Scenarios, sc)
	h.report.mu.Unlock()
	t.Cleanup(func() {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.WallSeconds = time.Since(sc.started).Seconds()
		switch {
		case t.Skipped():
			sc.Status = "skip"
		case t.Failed():
			sc.Status = "fail"
		default:
			sc.Status = "pass"
		}
	})
	t.Logf("scenario %s: %s", id, title)
	return sc
}

// check records one pass/fail statement. A failure fails the test; with a finding it is a known
// defect, and the message says which.
func (sc *scenario) check(t *testing.T, name string, pass bool, detail, findingID string) {
	t.Helper()
	sc.mu.Lock()
	sc.Checks = append(sc.Checks, check{Name: name, Pass: pass, Detail: detail, Finding: findingID})
	sc.mu.Unlock()
	if pass {
		t.Logf("PASS %s", name)
		return
	}
	if findingID != "" {
		t.Errorf("FAIL %s: %s\n    known finding %s: %s (%s)", name, detail, findingID, findings[findingID].Title, findings[findingID].Where)
		return
	}
	t.Errorf("FAIL %s: %s", name, detail)
}

func (sc *scenario) note(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	sc.mu.Lock()
	sc.Notes = append(sc.Notes, msg)
	sc.mu.Unlock()
	t.Log(msg)
}

func (sc *scenario) addPhase(p *phase) {
	sc.mu.Lock()
	sc.Phases = append(sc.Phases, p)
	sc.mu.Unlock()
}

// write stores the report and prints the summary.
func (h *harness) writeReport() {
	r := h.report
	r.mu.Lock()
	r.FinishedAt = time.Now().UTC()
	used := map[string]finding{}
	for _, sc := range r.Scenarios {
		for _, c := range sc.Checks {
			if c.Finding != "" {
				used[c.Finding] = findings[c.Finding]
			}
		}
	}
	r.Findings = used
	b, err := json.MarshalIndent(r, "", "  ")
	r.mu.Unlock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scale: report:", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.reportPath), 0o755); err == nil {
		err = os.WriteFile(h.reportPath, append(b, '\n'), 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "scale: report:", err)
		}
	}
	h.printSummary(os.Stdout, previousReport(filepath.Dir(h.reportPath), h.reportPath))
}

// previousReport is the newest report written before this one, for comparison, or nil.
func previousReport(dir, current string) *report {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && filepath.Join(dir, e.Name()) != current {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		b, err := os.ReadFile(filepath.Join(dir, names[i]))
		if err != nil {
			continue
		}
		var prev report
		if json.Unmarshal(b, &prev) == nil && prev.Schema == reportSchema {
			return &prev
		}
	}
	return nil
}

const reportSchema = "gitdr.scale/v1"

func (h *harness) printSummary(w io.Writer, prev *report) {
	r := h.report
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	defer func() { _, _ = io.WriteString(w, b.String()) }()
	fmt.Fprintf(&b, "\n==== gitdr scale harness, run %s, engine %s\n", r.RunID, r.Engine)
	fmt.Fprintf(&b, "profile: %d small repositories, %d changing a day, %d x %d refs, big %v\n",
		r.Profile.Repos, r.Profile.ChangePercent, r.Profile.RefsRepos, r.Profile.RefsPerRepo, r.Profile.Big)
	prevPhases := map[string]*phase{}
	if prev != nil {
		for _, sc := range prev.Scenarios {
			for _, p := range sc.Phases {
				prevPhases[sc.ID+"/"+p.Name] = p
			}
		}
		fmt.Fprintf(&b, "compared with run %s\n", prev.RunID)
	}
	for _, sc := range r.Scenarios {
		sc.mu.Lock()
		fmt.Fprintf(&b, "\n%-5s %s  [%s, %s]\n", strings.ToUpper(sc.Status), sc.Title, sc.ID, (time.Duration(sc.WallSeconds) * time.Second).String())
		for _, p := range sc.Phases {
			cmp := ""
			if pp := prevPhases[sc.ID+"/"+p.Name]; pp != nil {
				cmp = fmt.Sprintf(" (was %.1fs)", pp.WallSeconds)
			}
			fmt.Fprintf(&b, "      %-38s %7.1fs%s  copied %d, skipped %d, failed %d, recopied %d, manifest %s, scratch peak %s\n",
				p.Name, p.WallSeconds, cmp, p.Copied, p.Skipped, p.Failed, p.Recopied, size(p.ManifestBytes), size(p.PeakScratchBytes))
			if p.MemoryLimitBytes > 0 {
				fmt.Fprintf(&b, "      %-38s %s\n", "", memoryNote(p))
			}
		}
		for _, c := range sc.Checks {
			mark := "ok  "
			if !c.Pass {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "      %s %s\n", mark, c.Name)
			if !c.Pass {
				if c.Detail != "" {
					fmt.Fprintf(&b, "           %s\n", c.Detail)
				}
				if c.Finding != "" {
					fmt.Fprintf(&b, "           known: %s (%s)\n", c.Finding, findings[c.Finding].Where)
				} else {
					fmt.Fprintf(&b, "           not a known finding\n")
				}
			}
		}
		sc.mu.Unlock()
	}
	var known, unknown []string
	for _, sc := range r.Scenarios {
		for _, c := range sc.Checks {
			if c.Pass {
				continue
			}
			if c.Finding != "" {
				if !slices.Contains(known, c.Finding) {
					known = append(known, c.Finding)
				}
			} else {
				unknown = append(unknown, sc.ID+": "+c.Name)
			}
		}
	}
	fmt.Fprintf(&b, "\nknown findings shown: %d %v\nunexplained failures: %d %v\nreport: %s\n\n", len(known), known, len(unknown), unknown, h.reportPath)
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
