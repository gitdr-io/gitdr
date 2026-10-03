//go:build linux

package main

import "syscall"

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>.
const prSetDumpable = 4

// undumpable makes this process non-dumpable, from its init on.
//
// git and git-lfs run as gitdr's own user and parse whatever the source sends. Any process with
// that user can read /proc/<pid>/environ and /proc/<pid>/mem of a dumpable process, or attach to
// it with ptrace, and gitdr's environment can hold the destination's keys, the manifest signing
// key and the encryption key. Narrowing git's own environment (gitexec's passedThrough) does not
// stop a child from reading its parent's.
//
// This is the second of two protections. The first is the binary's mode: the image installs it
// owned by root with mode 0711, and the kernel starts a program its user cannot read
// non-dumpable, before its first instruction. This call covers a binary installed readable, from
// init on, and so not the milliseconds the Go runtime and every package's init take to get here
// (TestTheKernelClosesTheStartupWindow). Neither stops root or a process holding CAP_SYS_PTRACE,
// which gitdr's image does not grant its non-root user and its Helm chart drops besides. git is
// dumpable again after its exec, which is fine: it holds only what passedThrough gave it.
func undumpable() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
