package gitexec

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// floodLastLine is the last thing the flooding fake git says, which is what an error must keep.
const floodLastLine = "fatal: the remote end hung up unexpectedly"

// A git that fails after 10 MiB of stderr gives an error under 64 KiB that keeps the end of what
// it said, where git says what went wrong, and ends by saying how much was cut.
//
// The whole of stderr used to go into the error, and from there into the manifest and the log, so
// one noisy repository could make a manifest no reader would take.
func TestAGitErrorCarriesAtMost64KiBOfStderr(t *testing.T) {
	g := &Git{bin: fakeMisbehaving(t, "stderr-flood"), logger: slog.New(slog.DiscardHandler)}
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"a command that prints nothing": func() error {
			return g.CloneMirror(ctx, "https://git.example.test/octo/hello.git", filepath.Join(t.TempDir(), "m.git"), Options{})
		},
		"a command whose output is read": func() error {
			_, err := g.LsRemote(ctx, "https://git.example.test/octo/hello.git", Options{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil {
				t.Fatal("a git that exited 128 reported success")
			}
			msg := err.Error()
			if len(msg) >= 64<<10 {
				t.Errorf("the error is %d bytes, want under 64 KiB", len(msg))
			}
			if !strings.Contains(msg, floodLastLine) {
				t.Errorf("the error lost the end of stderr, where git says what failed: ...%s", msg[max(0, len(msg)-300):])
			}
			if !strings.HasSuffix(msg, "kept]") || !strings.Contains(msg, "[git stderr cut: the first ") {
				t.Errorf("the error does not end by saying stderr was cut: ...%s", msg[max(0, len(msg)-300):])
			}
		})
	}
}

// The writer keeps the last bytes whatever sizes they arrive in, and counts the rest.
func TestStderrTailKeepsTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   string
	}{
		{"under the limit", []string{"ab", "cd"}, "abcd"},
		{"over it in small writes", []string{"abc", "def", "gh"}, "efgh [git stderr cut: the first 4 bytes dropped, the last 4 kept]"},
		{"over it in one write", []string{"abcdefghij"}, "ghij [git stderr cut: the first 6 bytes dropped, the last 4 kept]"},
		{"a large write after a small one", []string{"ab", "cdefgh"}, "efgh [git stderr cut: the first 4 bytes dropped, the last 4 kept]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail := &stderrTail{max: 4}
			for _, w := range tc.writes {
				if n, err := tail.Write([]byte(w)); err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if got := tail.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
