//go:build !linux

package main

import (
	"runtime"
	"testing"
)

// The Linux test is in undumpable_linux_test.go.
func TestGitCannotReadTheEngine(t *testing.T) {
	t.Skip("PR_SET_DUMPABLE is Linux-only and gitdr ships for Linux only; on " + runtime.GOOS + " undumpable does nothing")
}
