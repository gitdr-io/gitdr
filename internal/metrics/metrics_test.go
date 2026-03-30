package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitdr.prom")
	if err := New(path).WriteSuccess(5); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "# TYPE gitdr_last_successful_run gauge") {
		t.Errorf("missing last_successful_run type:\n%s", s)
	}
	if !strings.Contains(s, "gitdr_repos_backed_up 5\n") {
		t.Errorf("missing repo count:\n%s", s)
	}
	// No stray temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("expected only the .prom file, got %d entries", len(entries))
	}
}

func TestNoopEmptyPath(t *testing.T) {
	if err := New("").WriteSuccess(3); err != nil {
		t.Fatalf("empty path should be a no-op: %v", err)
	}
}
