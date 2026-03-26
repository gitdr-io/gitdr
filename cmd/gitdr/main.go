// Command gitdr backs up Git VCS organizations to WORM-immutable object storage. It
// runs as a one-shot job: backup, restore, verify, or doctor.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"gitdr.io/gitdr/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:]))
}
