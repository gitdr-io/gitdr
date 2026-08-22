package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// render pushes a Secret through every output path the codebase can reach: fmt verbs
// (including mismatched ones, which is where the pre-Format leak lived), error
// wrapping, JSON, YAML, slog text and JSON handlers, and reflection through slices,
// maps, and structs.
//
// %p is deliberately absent: fmt special-cases it before any interface lookup, so for
// a non-pointer Secret it prints the raw value through the bad-verb path and no method
// can intercept it. See the Format comment in redact.go.
func render(s Secret) string {
	var parts []string
	for _, verb := range []string{
		"%v", "%s", "%q", "%x", "%X", "%#v", "%+v", "%-20s", "%5.1s",
		"%d", "%t", "%b", "%c", "%o", "%e", "%U",
	} {
		parts = append(parts, fmt.Sprintf(verb, s))
	}
	parts = append(parts, fmt.Sprint(s))
	parts = append(parts, fmt.Errorf("clone %q failed: %w", "octo/hello", fmt.Errorf("token %d rejected", s)).Error())

	type creds struct {
		Token Secret `json:"token" yaml:"token"`
		Host  string `json:"host" yaml:"host"`
	}
	c := creds{Token: s, Host: "github.com"}
	parts = append(parts, fmt.Sprintf("%v %+v %v", []Secret{s}, c, map[string]Secret{"token": s}))
	for _, v := range []any{s, c} {
		j, err := json.Marshal(v)
		if err != nil {
			parts = append(parts, "json error: "+err.Error())
			continue
		}
		parts = append(parts, string(j))
	}
	y, err := yaml.Marshal(c)
	if err != nil {
		parts = append(parts, "yaml error: "+err.Error())
	} else {
		parts = append(parts, string(y))
	}

	// Timestamps stripped so the rendering is a pure function of the secret.
	opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	var text, jsonOut bytes.Buffer
	slog.New(slog.NewTextHandler(&text, opts)).Error("auth failed", "token", s, "creds", c)
	slog.New(slog.NewJSONHandler(&jsonOut, opts)).Error("auth failed", "token", s, "creds", c)
	parts = append(parts, text.String(), jsonOut.String())

	return strings.Join(parts, "\n")
}

// The claim is stronger than "the secret is not printed": the rendering must be byte
// identical no matter what the secret is, so not even partial information escapes.
// Comparing against a fixed reference also sidesteps false positives from short
// secrets that happen to occur in scaffolding text ("msg", "E", ...).
func FuzzSecretNeverLeaks(f *testing.F) {
	f.Add("hunter2")
	f.Add("")
	f.Add(Placeholder)
	f.Add("%s%d%v%n")                                // format verbs inside the secret
	f.Add(`"],` + "\n" + `token: |`)                 // JSON and YAML breakouts
	f.Add("-----BEGIN PRIVATE KEY-----\nMC4CAQ==")   // realistic PEM fragment
	f.Add("ghp_16C7e42F292c6912E7710c838347Ae178B4") // realistic token shape
	f.Add("p\xc3\xa4ssword \xe5\xaf\x86\xe7\xa0\x81")
	f.Add("\x00\xff\x7f binary")
	f.Add(strings.Repeat("A", 8192))

	want := render(Secret("fuzz-reference-secret"))
	f.Fuzz(func(t *testing.T, secret string) {
		if len(secret) > 1<<16 { // real credentials are bytes to KiBs; keep execs fast
			return
		}
		got := render(Secret(secret))
		if got == want {
			return
		}
		if secret != "" && strings.Contains(got, secret) {
			t.Fatalf("secret survived redaction:\n%s", got)
		}
		t.Fatalf("rendering depends on the secret value:\n got: %s\nwant: %s", got, want)
	})
}
