// Package redact provides a string type for secrets that refuses to reveal itself in
// logs, errors, JSON/YAML, or fmt output. Wrap every credential field in Secret so
// an accidental %v, slog attribute, or config dump prints the placeholder instead of
// the secret. Call Reveal only at the exact point the secret is used.
package redact

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// Placeholder is what a redacted secret renders as in any output.
const Placeholder = "[REDACTED]"

// Secret is a string whose value never appears in formatted output.
type Secret string

func (s Secret) String() string   { return Placeholder }
func (s Secret) GoString() string { return Placeholder }

// Format implements fmt.Formatter so every verb renders the placeholder. Without it, a
// mismatched verb such as %d takes fmt's bad-verb path, which deliberately suppresses
// String (to avoid recursing while erroring) and prints the raw underlying value — so
// one wrong verb in an error wrap printed the secret. Found by FuzzSecretNeverLeaks.
//
// The residual gap: %p applied to a Secret value (not a *Secret) is special-cased by
// fmt before any interface lookup, still reaches the bad-verb path, and no method can
// intercept it. vet is silent too, because a Formatter is assumed to handle any verb.
func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(Placeholder))
		return
	}
	_, _ = io.WriteString(f, Placeholder)
}

// LogValue implements slog.LogValuer so structured logs never capture the secret.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Placeholder) }

// MarshalText covers fmt and any encoding that honors encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Placeholder), nil }

// MarshalJSON ensures JSON encoders emit the placeholder.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Placeholder + `"`), nil }

// MarshalYAML ensures yaml.v3 encoders emit the placeholder.
func (s Secret) MarshalYAML() (any, error) { return Placeholder, nil }

// Reveal returns the underlying secret value. Call only where the secret is actually
// used (auth, signing); never pass the result to logging or error wrapping.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s == "" }
