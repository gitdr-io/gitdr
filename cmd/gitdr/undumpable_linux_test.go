//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// prGetDumpable is PR_GET_DUMPABLE from <linux/prctl.h>.
const prGetDumpable = 3

// git, which runs as gitdr's own user, cannot read gitdr's environment, where a run keeps the
// destination's keys and the manifest signing key.
//
// This test binary is package main, so the init that ran before it is the engine's. The flag comes
// first. /proc/self/status has no line for it (proc_pid_status(5)), so it is asked of the kernel.
// Then what the flag is for: a child of this process tries what an exploited git would, reading
// /proc/<pid>/environ. The control comes last, the same read of a dumpable process, which has to
// succeed, or the refusal before it could be a read that never works.
func TestGitCannotReadTheEngine(t *testing.T) {
	flag, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_DUMPABLE: %v", errno)
	}
	if flag != 0 {
		t.Errorf("dumpable = %d after init, want 0", flag)
	}
	if holdsCapSysPtrace(t) {
		t.Skip("this process holds CAP_SYS_PTRACE, which reads any process's environment; gitdr's image and chart never grant it")
	}

	// A process's environ is the environment it started with, so the canary is a variable this
	// process started with: one set during the test would never appear there.
	if out, err := readEnviron(os.Getpid()); err == nil || strings.Contains(out, "PATH=") {
		t.Errorf("a child read the engine's environment: err=%v", err)
	}

	target := exec.Command("sleep", "60")
	target.Env = []string{"PATH=/usr/bin:/bin"}
	if err := target.Start(); err != nil {
		t.Fatalf("control: %v", err)
	}
	t.Cleanup(func() {
		_ = target.Process.Kill()
		_ = target.Wait()
	})
	if out, err := readEnviron(target.Process.Pid); err != nil || !strings.Contains(out, "PATH=") {
		t.Fatalf("control: a child could not read a dumpable process's environment either (%v), so this test proves nothing", err)
	}
}

// readEnviron reads /proc/<pid>/environ from a child process, the way git would.
func readEnviron(pid int) (string, error) {
	cmd := exec.Command("cat", fmt.Sprintf("/proc/%d/environ", pid))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// holdsCapSysPtrace reports whether this process has CAP_SYS_PTRACE (bit 19) in effect.
func holdsCapSysPtrace(t *testing.T) bool {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatalf("read capabilities: %v", err)
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if hex, ok := strings.CutPrefix(s.Text(), "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
			if err != nil {
				t.Fatalf("parse CapEff: %v", err)
			}
			return caps&(1<<19) != 0
		}
	}
	t.Fatal("no CapEff line in /proc/self/status")
	return false
}
