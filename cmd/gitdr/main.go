// Command gitdr backs up Git VCS organizations to WORM-immutable object storage. It
// runs as a one-shot job: backup, restore, verify, or doctor.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gitdr.io/gitdr/internal/cli"
)

// Before main, so before any git process exists (undumpable_linux.go). A binary installed as the
// image installs it, root's with mode 0711, was started non-dumpable by the kernel already; this
// covers one installed readable, from here on. In init rather than in main so that it holds for
// everything this package runs, its tests included, which is how they check it.
func init() {
	if err := undumpable(); err != nil {
		fmt.Fprintln(os.Stderr, "gitdr: cannot keep git out of this process's environment and memory:", err)
		os.Exit(1)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:]))
}
