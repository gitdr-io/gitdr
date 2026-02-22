package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secret = "hunter2"

func TestNeverLeaks(t *testing.T) {
	s := Secret(secret)

	if s.String() != Placeholder {
		t.Errorf("String() = %q", s.String())
	}
	if out := fmt.Sprintf("%v %s %#v", s, s, s); strings.Contains(out, secret) {
		t.Errorf("fmt leaked: %s", out)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), secret) {
		t.Errorf("json leaked: %s", b)
	}
	if s.Reveal() != secret {
		t.Errorf("Reveal() = %q, want %q", s.Reveal(), secret)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("msg", "token", s)
	if strings.Contains(buf.String(), secret) {
		t.Errorf("slog leaked: %s", buf.String())
	}
}

func TestIsZero(t *testing.T) {
	if !Secret("").IsZero() {
		t.Error("empty secret should be zero")
	}
	if Secret("x").IsZero() {
		t.Error("non-empty secret should not be zero")
	}
}
