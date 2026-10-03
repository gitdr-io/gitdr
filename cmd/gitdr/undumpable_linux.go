//go:build linux

package main

import "syscall"

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>.
const prSetDumpable = 4

// undumpable keeps git and git-lfs out of this process's environment and memory.
//
// They run as gitdr's own user and parse whatever the source sends. Any process with that user
// can read /proc/<pid>/environ and /proc/<pid>/mem of a dumpable process, or attach to it with
// ptrace, and gitdr's environment can hold the destination's keys, the manifest signing key and
// the encryption key. Narrowing git's own environment (gitexec's passedThrough) does not stop a
// child from reading its parent's. A process that is not dumpable is open that way only to one
// holding CAP_SYS_PTRACE, which gitdr's image does not grant its non-root user and its Helm chart
// drops besides. git is dumpable again after its exec, which is fine: it holds only what
// passedThrough gave it.
func undumpable() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
