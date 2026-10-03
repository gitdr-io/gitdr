package pipeline

import (
	"context"
	"sync"
	"time"
)

// Test-only access for the package's external tests. Compiled into test binaries alone.

// FailLFSArchiveForTest makes every LFS archive fail with err, until the returned function puts
// the real archiver back. A test that uses it must not run in parallel with another backup.
func FailLFSArchiveForTest(err error) (restore func()) {
	archiveLFS = func(context.Context, string, string) error { return err }
	return func() { archiveLFS = moveIntoTar }
}

// HoldLFSArchiveForTest makes every LFS archive wait for release before it archives, whatever its
// context says: a step that does not stop with the run. started is closed as the first one
// begins. A test that uses it must not run in parallel with another backup.
func HoldLFSArchiveForTest(started chan<- struct{}, release <-chan struct{}) (restore func()) {
	var once sync.Once
	archiveLFS = func(ctx context.Context, src, dst string) error {
		once.Do(func() { close(started) })
		<-release
		return moveIntoTar(ctx, src, dst)
	}
	return func() { archiveLFS = moveIntoTar }
}

// ShortenInFlightGraceForTest makes a stopped run wait d for the repositories in flight, until the
// returned function puts the real grace back. A test that uses it must not run in parallel with
// another backup.
func ShortenInFlightGraceForTest(d time.Duration) (restore func()) {
	old := inFlightGrace
	inFlightGrace = d
	return func() { inFlightGrace = old }
}
