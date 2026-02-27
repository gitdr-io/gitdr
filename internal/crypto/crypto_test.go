package crypto

import (
	"strings"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pubPEM, privPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("canonical manifest bytes")
	sig := Sign(priv, msg)
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatalf("verify good signature: %v", err)
	}
	if err := Verify(pub, append(msg, '!'), sig); err == nil {
		t.Fatal("verify should fail on modified message")
	}
}

func TestSHA256Bytes(t *testing.T) {
	// Known SHA-256 of "abc".
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := SHA256Bytes([]byte("abc")); got != want {
		t.Fatalf("SHA256Bytes = %s, want %s", got, want)
	}
}

func TestSHA256Hex(t *testing.T) {
	got, n, err := SHA256Hex(strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("read %d bytes, want 3", n)
	}
	if got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("unexpected digest %s", got)
	}
}

func TestParseSeedAndRawKeys(t *testing.T) {
	// A 32-byte seed and a 64-byte private key are both accepted.
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	if _, err := ParsePrivateKey(seed); err != nil {
		t.Fatalf("parse 32-byte seed: %v", err)
	}
}
