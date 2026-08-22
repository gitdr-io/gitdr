package redact_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// %p is the one verb redact.Secret cannot defend against. fmt resolves it before it looks
// for a Formatter, so no method on the type can intercept it and the raw value is printed.
// go vet is no help either: once a type has a Formatter, vet stops checking its verbs.
//
// So the defence is that the string never appears. Nothing in this tool needs a pointer
// address in its output, and a scan is the only check that can actually enforce that: a
// forbidigo rule was tried first and silently matched nothing, because forbidigo looks at
// function calls, not at the contents of a format string.
func TestNoPointerVerbAnywhere(t *testing.T) {
	root := "../.."
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".cache", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "verb_scan_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.Contains(code, "%p") {
				found = append(found, filepath.Clean(path)+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("%%p prints the raw value of a redact.Secret and cannot be intercepted; found at:\n  %s",
			strings.Join(found, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
