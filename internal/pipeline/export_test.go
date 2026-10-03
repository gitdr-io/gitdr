package pipeline

// Test-only access for the package's external tests. Compiled into test binaries alone.

// FailLFSArchiveForTest makes every LFS archive fail with err, until the returned function puts
// the real archiver back. A test that uses it must not run in parallel with another backup.
func FailLFSArchiveForTest(err error) (restore func()) {
	archiveLFS = func(string, string) error { return err }
	return func() { archiveLFS = moveIntoTar }
}
