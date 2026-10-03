package dest_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/dest"
)

const mib = 1 << 20

// answer is a body of size bytes, counting what anything takes from it.
type answer struct {
	size, taken int64
	closed      bool
}

func (a *answer) Read(p []byte) (int, error) {
	if a.taken >= a.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), a.size-a.taken)
	for i := range n {
		p[i] = 'x'
	}
	a.taken += n
	return int(n), nil
}

func (a *answer) Close() error { a.closed = true; return nil }

// A 64 MiB answer stops at the cap: exactly the cap is delivered, then ErrResponseTooLarge, and
// one byte past the cap is all that is ever taken from the answer. It is the byte that tells a
// long answer from one of exactly the cap.
func TestLimitBodyStopsALongAnswerAtTheCap(t *testing.T) {
	src := &answer{size: 64 * mib}
	body := dest.LimitBody(src, mib)

	got, err := io.Copy(io.Discard, body)
	if !errors.Is(err, dest.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if got != mib {
		t.Errorf("delivered %d bytes, want exactly %d", got, mib)
	}
	if src.taken > mib+1 {
		t.Errorf("took %d bytes from the answer, want at most %d", src.taken, mib+1)
	}

	// And it stays refused, so a caller that drains the rest drains nothing.
	if n, err := body.Read(make([]byte, 32<<10)); n != 0 || !errors.Is(err, dest.ErrResponseTooLarge) {
		t.Errorf("a read after the cap gave %d bytes and %v", n, err)
	}
	if err := body.Close(); err != nil || !src.closed {
		t.Errorf("close: %v, closed the answer: %v", err, src.closed)
	}
}

func TestLimitBodyLeavesAShortAnswerAlone(t *testing.T) {
	for _, size := range []int{0, 1, mib - 1, mib} {
		want := bytes.Repeat([]byte("y"), size)
		got, err := io.ReadAll(dest.LimitBody(io.NopCloser(bytes.NewReader(want)), mib))
		if err != nil {
			t.Errorf("%d bytes: %v", size, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%d bytes: got %d back", size, len(got))
		}
	}
	// One past the cap is too large, and the cap is delivered.
	got, err := io.ReadAll(dest.LimitBody(io.NopCloser(strings.NewReader(strings.Repeat("z", mib+1))), mib))
	if !errors.Is(err, dest.ErrResponseTooLarge) || len(got) != mib {
		t.Errorf("cap+1 bytes: %d delivered, err %v", len(got), err)
	}
}

// A store's error code reaches a log, a signed manifest and doctor's document, so only something
// shaped like a code gets through.
func TestShapedCodeLetsThroughOnlyACode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"AccessDenied", "AccessDenied"},
		{"ObjectLockConfigurationNotFoundError", "ObjectLockConfigurationNotFoundError"},
		{"notFound", "notFound"},
		{"XAmzContentSHA256Mismatch", "XAmzContentSHA256Mismatch"},
		{"Some.Code2", "Some.Code2"},
		{"A", "A"},
		{"A" + strings.Repeat("b", 63), "A" + strings.Repeat("b", 63)},

		{"", dest.UnnamedCode},
		{"A" + strings.Repeat("b", 64), dest.UnnamedCode},
		{"2xx", dest.UnnamedCode},
		{".Code", dest.UnnamedCode},
		{"Access Denied", dest.UnnamedCode},
		{"gitdr-marker", dest.UnnamedCode},
		{"<script>", dest.UnnamedCode},
		{"Code\n", dest.UnnamedCode},
		{"Code\x1b[2J", dest.UnnamedCode},
		{"Cödé", dest.UnnamedCode},
		{"https://example.com", dest.UnnamedCode},
	} {
		if got := dest.ShapedCode(tc.in); got != tc.want {
			t.Errorf("ShapedCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
