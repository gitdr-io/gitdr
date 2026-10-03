//go:build scale

// Command cgroupwatch runs a command inside a container and watches the memory of the container's
// cgroup while it runs: what the kernel charged in all (memory.current, memory.peak), how much of
// that was anonymous memory a process cannot give back and how much was file pages, how much the
// processes had resident at once, how often the limit killed something (memory.events), and the
// peak resident set of each process it saw.
//
// memory.peak counts the page cache, which the kernel fills up to the limit and takes back when it
// needs the room: a run that writes more than the limit to disk ends with memory.peak at the limit
// whatever its processes hold. What they hold is their anonymous memory and the file pages they
// map, maxResident here. The working set is the kubelet's figure, memory.current less the inactive
// file pages, the one kubectl top shows.
//
// It is the scale harness's way into the cgroup of the released image (make scale-image). It runs
// as the container's entrypoint with the engine as its child, and reads the cgroup one last time
// after the engine exits, before the container and its cgroup are gone. It prints one line,
// "cgroupwatch " and a JSON summary, to stderr, and exits with the child's status.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	interval := flag.Duration("interval", 100*time.Millisecond, "how often to sample")
	cgroup := flag.String("cgroup", "/sys/fs/cgroup", "the cgroup v2 directory to read")
	du := flag.String("du", "", "a directory whose size to sample too, such as the engine's scratch")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "cgroupwatch: no command")
		os.Exit(2)
	}

	w := &watch{cgroup: *cgroup, du: *du, procs: map[string]*procPeak{}}
	// audited: the command is the container's own command line, set by the harness that started
	// the container; there is no other input. An argv array, no shell.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cgroupwatch:", err)
		os.Exit(127)
	}
	// A signal to the container is for the engine.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(*interval)
		defer tick.Stop()
		for {
			w.sample()
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	})
	err := cmd.Wait()
	close(done)
	wg.Wait()
	w.sample()
	w.final()

	b, jerr := json.Marshal(w.summary())
	if jerr == nil {
		fmt.Fprintf(os.Stderr, "cgroupwatch %s\n", b)
	}
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	} else if err != nil {
		code = 1
	}
	os.Exit(code)
}

type procPeak struct {
	Cmd     string `json:"cmd"`
	HWM     int64  `json:"hwm"`     // VmHWM: the process's own peak resident set
	MaxAnon int64  `json:"maxAnon"` // the most RssAnon seen
	MaxFile int64  `json:"maxFile"` // the most RssFile seen: file pages, mmapped packs among them
}

type watch struct {
	cgroup, du string

	mu         sync.Mutex
	limit      int64
	peak       int64
	oom        int64
	oomKill    int64
	maxCurrent int64
	maxAnon    int64
	maxFile    int64
	maxMapped  int64
	maxScratch int64
	procs      map[string]*procPeak
	// The most anon + file_mapped in one sample, and the most memory.current - inactive_file.
	maxResident, maxWorkingSet int64
}

func (w *watch) sample() {
	cur := readInt(filepath.Join(w.cgroup, "memory.current"))
	stat := readKV(filepath.Join(w.cgroup, "memory.stat"))
	events := readKV(filepath.Join(w.cgroup, "memory.events"))
	var scratch int64
	if w.du != "" {
		scratch = dirSize(w.du)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.maxCurrent = max(w.maxCurrent, cur)
	w.maxAnon = max(w.maxAnon, stat["anon"])
	w.maxFile = max(w.maxFile, stat["file"])
	w.maxMapped = max(w.maxMapped, stat["file_mapped"])
	w.maxResident = max(w.maxResident, stat["anon"]+stat["file_mapped"])
	w.maxWorkingSet = max(w.maxWorkingSet, cur-stat["inactive_file"])
	w.oom = max(w.oom, events["oom"])
	w.oomKill = max(w.oomKill, events["oom_kill"])
	w.maxScratch = max(w.maxScratch, scratch)

	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		if _, err := strconv.Atoi(d.Name()); err != nil || d.Name() == strconv.Itoa(os.Getpid()) {
			continue
		}
		// The command line first: a process that exits between the two reads has no status
		// left, and one without a command line is not worth a row.
		name := command(d.Name())
		st := readKV(filepath.Join("/proc", d.Name(), "status"))
		if name == "" || st == nil {
			continue
		}
		p := w.procs[name]
		if p == nil {
			p = &procPeak{Cmd: name}
			w.procs[name] = p
		}
		// /proc reports these in kB.
		p.HWM = max(p.HWM, st["VmHWM"]*1024)
		p.MaxAnon = max(p.MaxAnon, st["RssAnon"]*1024)
		p.MaxFile = max(p.MaxFile, st["RssFile"]*1024)
	}
}

// final reads what only the end can say: the kernel's own peak, and the events once more.
func (w *watch) final() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peak = readInt(filepath.Join(w.cgroup, "memory.peak"))
	if raw, err := os.ReadFile(filepath.Join(w.cgroup, "memory.max")); err == nil {
		w.limit, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64) // "max" reads as 0
	}
	events := readKV(filepath.Join(w.cgroup, "memory.events"))
	w.oom = max(w.oom, events["oom"])
	w.oomKill = max(w.oomKill, events["oom_kill"])
}

type summary struct {
	Limit         int64       `json:"limit"`
	Peak          int64       `json:"peak"`
	OOM           int64       `json:"oom"`
	OOMKill       int64       `json:"oomKill"`
	MaxCurrent    int64       `json:"maxCurrent"`
	MaxAnon       int64       `json:"maxAnon"`
	MaxFile       int64       `json:"maxFile"`
	MaxFileMapped int64       `json:"maxFileMapped"`
	MaxResident   int64       `json:"maxResident"`
	MaxWorkingSet int64       `json:"maxWorkingSet"`
	MaxScratch    int64       `json:"maxScratch,omitempty"`
	Processes     []*procPeak `json:"processes"`
}

func (w *watch) summary() summary {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := summary{
		Limit: w.limit, Peak: w.peak, OOM: w.oom, OOMKill: w.oomKill, MaxCurrent: w.maxCurrent,
		MaxAnon: w.maxAnon, MaxFile: w.maxFile, MaxFileMapped: w.maxMapped, MaxScratch: w.maxScratch,
		MaxResident: w.maxResident, MaxWorkingSet: w.maxWorkingSet,
	}
	for _, p := range w.procs {
		s.Processes = append(s.Processes, p)
	}
	sort.Slice(s.Processes, func(i, j int) bool { return s.Processes[i].HWM > s.Processes[j].HWM })
	if len(s.Processes) > 10 {
		s.Processes = s.Processes[:10]
	}
	return s
}

// command names a process by its program and, for git, its subcommand: "git pack-objects",
// "git-lfs fetch", "gitdr backup". It is empty for a process that has exited or never had a
// command line.
func command(pid string) string {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err != nil || len(raw) == 0 {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	name := filepath.Base(args[0])
	for _, a := range args[1:] {
		if a == "-c" || strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue
		}
		return name + " " + a
	}
	return name
}

func readInt(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

// readKV reads "key value" or "Key:\tvalue kB" lines into a map, nil when the file is unreadable.
func readKV(path string) map[string]int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		fields := strings.Fields(strings.Replace(sc.Text(), ":", " ", 1))
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			out[fields[0]] = n
		}
	}
	return out
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
