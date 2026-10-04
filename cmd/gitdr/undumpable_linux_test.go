//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// prGetDumpable is PR_GET_DUMPABLE from <linux/prctl.h>.
const prGetDumpable = 3

// dumpableFlag reads this process's dumpable flag. /proc/self/status has no line for it
// (proc_pid_status(5)), so it is asked of the kernel.
func dumpableFlag(t *testing.T) uintptr {
	t.Helper()
	flag, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_DUMPABLE: %v", errno)
	}
	return flag
}

// The two programs this test binary becomes when TestTheKernelClosesTheStartupWindow copies it:
// the engine, which starts as gitdr does, init included, and exits; and a reader, which starts
// the engine and reads its environment for as long as it runs.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "gitdr":
		os.Exit(0)
	case "reader":
		os.Exit(readWhileItStarts(os.Args[1]))
	}
	os.Exit(m.Run())
}

// startupCanary is the secret the reader gives the engine to keep in its environment.
const startupCanary = "canary-startup-signing-key"

// engineUser is the uid and gid the image runs gitdr as.
const engineUser = 65532

// readWhileItStarts takes the engine's user, starts the engine at path with a secret in its
// environment, reads /proc/<pid>/environ until the engine exits, and prints whether any read held
// the secret.
//
// It takes the user itself rather than being started with it. os/exec starts a child with
// CLONE_VM, and a child that changes its credentials before its exec resets the dumpable flag of
// the memory it still shares with its parent to fs.suid_dumpable. Started with
// SysProcAttr.Credential, the reader would change the test binary's own flag, which on a host with
// fs.suid_dumpable=2 is what TestGitCannotReadTheEngine then found.
func readWhileItStarts(path string) int {
	for _, step := range []func() error{
		func() error { return syscall.Setgroups(nil) },
		func() error { return syscall.Setgid(engineUser) },
		func() error { return syscall.Setuid(engineUser) },
	} {
		if err := step(); err != nil {
			fmt.Fprintln(os.Stderr, "take the engine's user:", err)
			return 2
		}
	}
	cmd := exec.Command(path)
	cmd.Env = []string{"GITDR_MANIFEST_SIGNING_KEY=" + startupCanary}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	environ := fmt.Sprintf("/proc/%d/environ", cmd.Process.Pid)
	for {
		select {
		case <-exited:
			fmt.Print("clean")
			return 0
		default:
		}
		if b, err := os.ReadFile(environ); err == nil && bytes.Contains(b, []byte(startupCanary)) {
			<-exited
			fmt.Print("leaked")
			return 0
		}
	}
}

// The kernel starts a program non-dumpable, before its first instruction, when the user running it
// cannot read its file. init's prctl comes later: the Go runtime and every package's init run
// first, and for those milliseconds any process with the engine's user can read the engine's
// environment. So the image installs gitdr owned by root with mode 0711 (image_test.go), and this
// is the proof that the mode closes the window.
//
// Twenty starts each way, by a reader that has the engine's user, as an exploited git would, and
// reads the engine's environment for as long as it runs. The engine is this test binary, which
// links package main and so starts as gitdr does. Readable, the window is there to be read, which
// is the control; owned by root with mode 0711, no start may leak.
func TestTheKernelClosesTheStartupWindow(t *testing.T) {
	if os.Geteuid() != 0 {
		// A gate that runs this as root sets GITDR_TEST_NEEDS_ROOT, so losing root there fails
		// the gate instead of skipping the proof. Anywhere else it is skipped, as other tests skip
		// what their machine cannot do.
		if os.Getenv("GITDR_TEST_NEEDS_ROOT") != "" {
			t.Fatal("not root: GITDR_TEST_NEEDS_ROOT is set, and this test gives the engine to root and runs it as another user")
		}
		t.Skip("needs root, to give the engine to root and run it as another user")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "gitdr-startup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reader, engine := filepath.Join(dir, "reader"), filepath.Join(dir, "gitdr")
	for _, dst := range []string{reader, engine} {
		b, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	leaks := func(mode os.FileMode) int {
		t.Helper()
		if err := os.Chmod(engine, mode); err != nil {
			t.Fatal(err)
		}
		n := 0
		for range 20 {
			out, err := exec.Command(reader, engine).Output()
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			switch string(out) {
			case "leaked":
				n++
			case "clean":
			default:
				t.Fatalf("the reader said %q", out)
			}
		}
		return n
	}
	readable, unreadable := leaks(0o755), leaks(0o711)
	t.Logf("starts that leaked the engine's environment: %d of 20 readable, %d of 20 at 0711", readable, unreadable)
	if readable == 0 {
		t.Fatal("control: no start of a readable engine leaked, so this test cannot see the window it is about")
	}
	if unreadable != 0 {
		t.Errorf("%d of 20 starts of an engine only root can read leaked its environment", unreadable)
	}
	// The test's own process is the engine too, and nothing here may have changed its flag.
	if flag := dumpableFlag(t); flag != 0 {
		t.Errorf("this test left its own process with dumpable = %d, want 0", flag)
	}
}

// git, which runs as gitdr's own user, cannot read gitdr's environment, where a run keeps the
// destination's keys and the manifest signing key.
//
// This test binary is package main, so the init that ran before it is the engine's. The flag comes
// first (dumpableFlag). Then what the flag is for: a child of this process tries what an exploited
// git would, reading /proc/<pid>/environ. The control comes last, the same read of a dumpable
// process, which has to succeed, or the refusal before it could be a read that never works.
func TestGitCannotReadTheEngine(t *testing.T) {
	if flag := dumpableFlag(t); flag != 0 {
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
	// Start returns when the child's close-on-exec descriptors close, part way through its exec:
	// the kernel may not have made the new program dumpable or laid out its environment yet, so
	// an early read can be refused or come back empty. Poll until it works.
	var out string
	var err error
	for range 200 {
		if out, err = readEnviron(target.Process.Pid); err == nil && strings.Contains(out, "PATH=") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !strings.Contains(out, "PATH=") {
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
