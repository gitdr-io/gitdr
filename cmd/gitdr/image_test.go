package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The image installs gitdr owned by root with mode 0711. Its user can run it and cannot read it,
// and the kernel starts a program its user cannot read non-dumpable, which covers the
// milliseconds before init's prctl (TestTheKernelClosesTheStartupWindow). Both Dockerfiles: the
// release image is built from Dockerfile.goreleaser and `make image` from Dockerfile.
func TestTheImageInstallsGitdrUnreadable(t *testing.T) {
	for _, name := range []string{"Dockerfile", "Dockerfile.goreleaser"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			var installs []string
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) > 1 && strings.EqualFold(f[0], "COPY") && f[len(f)-1] == "/usr/bin/gitdr" {
					installs = append(installs, line)
				}
			}
			if len(installs) != 1 {
				t.Fatalf("%d COPY lines install /usr/bin/gitdr, want 1: %q", len(installs), installs)
			}
			if !strings.Contains(installs[0], "--chmod=0711") {
				t.Errorf("/usr/bin/gitdr is not installed with --chmod=0711: %q", installs[0])
			}
			if strings.Contains(installs[0], "--chown") {
				t.Errorf("/usr/bin/gitdr is given an owner other than root: %q", installs[0])
			}
		})
	}
}
