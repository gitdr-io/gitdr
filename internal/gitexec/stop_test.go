package gitexec

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A git killed by its context returns within waitDelay, even when a child it started still holds
// its stderr open.
//
// git-lfs and the remote helpers are git's children and write to its stderr. Killed by a stop, git
// went, the child stayed, and Wait waited for the child to let go of the pipe, which could be the
// whole of a long LFS transfer. The stopped run sat there instead of filing its manifest.
func TestAKilledGitDoesNotWaitForAChildHoldingItsStderr(t *testing.T) {
	bin := fakeMisbehaving(t, "hold-stderr")
	pidFile := filepath.Join(filepath.Dir(bin), fakeGitPIDFile)
	t.Cleanup(func() {
		// The child sleeps on its own; it is not left behind past the test.
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})

	g := &Git{bin: bin, logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(500*time.Millisecond, cancel)

	start := time.Now()
	err := g.CloneMirror(ctx, "https://git.example.test/octo/hello.git", filepath.Join(t.TempDir(), "m.git"), Options{})
	took := time.Since(start)
	if err == nil {
		t.Fatal("a killed git reported success")
	}
	// waitDelay after the kill, and room for a slow machine. The child sleeps for 20 seconds.
	const limit = 12 * time.Second
	if took > limit {
		t.Fatalf("the killed git's command returned after %s, past %s: a child holding its stderr kept it waiting", took.Round(time.Second), limit)
	}
}
