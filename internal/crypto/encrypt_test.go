package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, EncryptionKeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncryptRoundTrip(t *testing.T) {
	key := randKey(t)
	// Sizes around the chunk boundary to exercise the streaming framing.
	for _, sz := range []int{0, 1, 100, encChunk - 1, encChunk, encChunk + 1, 3 * encChunk, 200000} {
		plain := make([]byte, sz)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		var ct bytes.Buffer
		if err := Encrypt(&ct, bytes.NewReader(plain), key); err != nil {
			t.Fatalf("size %d encrypt: %v", sz, err)
		}
		if ct.Len() <= sz {
			t.Fatalf("size %d: ciphertext %d not larger than plaintext", sz, ct.Len())
		}
		var pt bytes.Buffer
		if err := Decrypt(&pt, bytes.NewReader(ct.Bytes()), key); err != nil {
			t.Fatalf("size %d decrypt: %v", sz, err)
		}
		if !bytes.Equal(pt.Bytes(), plain) {
			t.Fatalf("size %d: round-trip mismatch", sz)
		}
	}
}

func TestDecryptWrongKey(t *testing.T) {
	var ct bytes.Buffer
	if err := Encrypt(&ct, bytes.NewReader([]byte("secret bundle bytes")), randKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(ct.Bytes()), randKey(t)); err == nil {
		t.Fatal("decrypt with the wrong key must fail")
	}
}

func TestDecryptTamper(t *testing.T) {
	key := randKey(t)
	var ct bytes.Buffer
	if err := Encrypt(&ct, bytes.NewReader(bytes.Repeat([]byte("x"), 1000)), key); err != nil {
		t.Fatal(err)
	}
	b := ct.Bytes()
	b[len(b)-1] ^= 0xff // corrupt the auth tag
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(b), key); err == nil {
		t.Fatal("decrypt of tampered data must fail")
	}
}

func TestDecryptTruncation(t *testing.T) {
	key := randKey(t)
	var ct bytes.Buffer
	if err := Encrypt(&ct, bytes.NewReader(bytes.Repeat([]byte("y"), 3*encChunk)), key); err != nil {
		t.Fatal(err)
	}
	truncated := ct.Bytes()[:ct.Len()-(encChunk+encTag)] // drop the final (last-flagged) chunk
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(truncated), key); err == nil {
		t.Fatal("decrypt of a truncated stream must fail")
	}
}

// The file helpers stop with their context. A backup stopped by SIGTERM used to wait for the
// encryption and the hash of every artifact in flight to reach the end of the file, and the
// manifest's grace ran out meanwhile.
func TestTheFileHelpersStopWithTheirContext(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "artifact")
	body := bytes.Repeat([]byte("z"), 3*encChunk+7)
	if err := os.WriteFile(plain, body, 0o600); err != nil {
		t.Fatal(err)
	}
	key := randKey(t)
	stopped, stop := context.WithCancel(t.Context())
	stop()

	enc := filepath.Join(dir, "artifact.enc")
	if err := EncryptFile(stopped, plain, enc, key); !errors.Is(err, context.Canceled) {
		t.Errorf("EncryptFile on a stopped context: %v, want context.Canceled", err)
	}
	if _, _, err := SHA256File(stopped, plain); !errors.Is(err, context.Canceled) {
		t.Errorf("SHA256File on a stopped context: %v, want context.Canceled", err)
	}

	// Not stopped, they do the whole file.
	if err := EncryptFile(t.Context(), plain, enc, key); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(stopped, enc, filepath.Join(dir, "stopped"), key); !errors.Is(err, context.Canceled) {
		t.Errorf("DecryptFile on a stopped context: %v, want context.Canceled", err)
	}
	back := filepath.Join(dir, "artifact.back")
	if err := DecryptFile(t.Context(), enc, back, key); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(back); err != nil || !bytes.Equal(got, body) {
		t.Errorf("the round trip gave %d bytes (%v), want the %d written", len(got), err, len(body))
	}
	sum, n, err := SHA256File(t.Context(), plain)
	if err != nil || n != int64(len(body)) || sum != SHA256Bytes(body) {
		t.Errorf("SHA256File = %s, %d, %v; want %s over %d bytes", sum, n, err, SHA256Bytes(body), len(body))
	}
}

func TestParseEncryptionKey(t *testing.T) {
	raw := make([]byte, EncryptionKeySize)
	for i := range raw {
		raw[i] = byte(i)
	}
	for _, in := range [][]byte{[]byte(hex.EncodeToString(raw)), []byte(base64.StdEncoding.EncodeToString(raw)), raw} {
		k, err := ParseEncryptionKey(in)
		if err != nil || !bytes.Equal(k, raw) {
			t.Fatalf("parse failed: %v", err)
		}
	}
	if _, err := ParseEncryptionKey([]byte("short")); err == nil {
		t.Fatal("short key must be rejected")
	}
}
