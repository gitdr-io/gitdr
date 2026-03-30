// Package metrics emits gitdr's run metrics in Prometheus text-exposition format to a
// file that node_exporter's textfile collector scrapes. The metric DR alerting cares
// about is gitdr_last_successful_run (unix seconds of the last successful backup).
// An empty path is a no-op, so the tool runs anywhere. OTel users can still consume
// these via the Collector's Prometheus receiver.
package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Writer writes the metrics file. Construct with New; an empty path disables output.
type Writer struct{ path string }

// New returns a Writer for the given textfile-collector path ("" = disabled).
func New(path string) *Writer { return &Writer{path: path} }

// WriteSuccess records the last-successful-run timestamp and protected-repo count,
// writing the file atomically (temp + rename) so the collector never reads a partial
// file. Call only after a fully successful run, so a failure leaves the value stale
// and DR alerting fires.
func (w *Writer) WriteSuccess(repos int) error {
	if w.path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("# HELP gitdr_last_successful_run Unix timestamp (seconds) of the last successful gitdr backup.\n")
	b.WriteString("# TYPE gitdr_last_successful_run gauge\n")
	fmt.Fprintf(&b, "gitdr_last_successful_run %d\n", time.Now().Unix())
	b.WriteString("# HELP gitdr_repos_backed_up Repositories protected by the last successful run.\n")
	b.WriteString("# TYPE gitdr_repos_backed_up gauge\n")
	fmt.Fprintf(&b, "gitdr_repos_backed_up %d\n", repos)
	return atomicWrite(w.path, b.String())
}

func atomicWrite(path, data string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gitdr-*.prom.tmp")
	if err != nil {
		return fmt.Errorf("metrics: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.WriteString(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("metrics: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("metrics: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { // collector must be able to read it
		return fmt.Errorf("metrics: chmod: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("metrics: rename: %w", err)
	}
	return nil
}
