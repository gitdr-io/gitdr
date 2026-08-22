package crypto

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"testing"
)

// FuzzDecrypt drives the envelope decrypt path from the position of an attacker who
// can rewrite the bucket: a fresh ciphertext is round-tripped, then attacked with a
// fuzz-chosen mutation, the wrong key, and arbitrary bytes. Decrypt must never panic
// and must never return success for bytes it did not authenticate.
func FuzzDecrypt(f *testing.F) {
	kek := bytes.Repeat([]byte{0x11}, EncryptionKeySize)
	wrongKek := bytes.Repeat([]byte{0x22}, EncryptionKeySize)

	headerShaped := make([]byte, encHeaderLen)
	copy(headerShaped, encMagic)
	headerShaped[len(encMagic)] = encVersion
	wrongVersion := bytes.Clone(headerShaped)
	wrongVersion[len(encMagic)] = 2

	f.Add([]byte{}, []byte{}, byte(0), uint32(0), byte(0))
	f.Add([]byte("hello gitdr"), []byte(encMagic), byte(0), uint32(7), byte(0x80))
	f.Add([]byte("hello gitdr"), headerShaped, byte(1), uint32(90), byte(1))
	f.Add([]byte("x"), wrongVersion, byte(2), uint32(0), byte(0xff))
	// Multi-chunk plaintext, truncation aimed at the final chunk, so the last-flag
	// framing is exercised from the first minute.
	f.Add(bytes.Repeat([]byte{'y'}, encChunk+1), []byte{}, byte(1), uint32(encHeaderLen+encChunk+encTag), byte(0))
	f.Add(bytes.Repeat([]byte{'z'}, 300), headerShaped[:20], byte(0), uint32(encHeaderLen-1), byte(4))

	f.Fuzz(func(t *testing.T, plain, raw []byte, op byte, pos uint32, xb byte) {
		// Several buffers of each input live at once across the passes below; unbounded
		// growth lets the mutator stall every worker on memory. Multi-chunk framing is
		// fully exercised within a few chunks.
		if len(plain) > 3*encChunk || len(raw) > 3*encChunk {
			return
		}
		var ct bytes.Buffer
		if err := Encrypt(&ct, bytes.NewReader(plain), kek); err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		var pt bytes.Buffer
		if err := Decrypt(&pt, bytes.NewReader(ct.Bytes()), kek); err != nil {
			t.Fatalf("decrypt of own ciphertext: %v", err)
		}
		if !bytes.Equal(pt.Bytes(), plain) {
			t.Fatalf("round trip: %d bytes in, %d out", len(plain), pt.Len())
		}

		if err := Decrypt(io.Discard, bytes.NewReader(ct.Bytes()), wrongKek); err == nil {
			t.Fatal("decrypt with the wrong key succeeded")
		}

		mut := bytes.Clone(ct.Bytes())
		switch op % 3 {
		case 0:
			mut[int(pos)%len(mut)] ^= xb | 1 // |1 so the byte always changes
		case 1:
			mut = mut[:int(pos)%len(mut)]
		case 2:
			mut = append(mut, xb)
		}
		if err := Decrypt(io.Discard, bytes.NewReader(mut), kek); err == nil {
			t.Fatalf("tampered ciphertext decrypted (op %d, pos %d)", op%3, pos)
		}

		// Nothing was ever encrypted under this kek except ct, so anything else that
		// decrypts is a forgery.
		if !bytes.Equal(raw, ct.Bytes()) {
			if err := Decrypt(io.Discard, bytes.NewReader(raw), kek); err == nil {
				t.Fatal("unauthenticated input decrypted")
			}
		}
	})
}

// FuzzParseKeys covers the three key parsers. Keys come from the operator, not the
// bucket, but a parser that lets a wrong-length key through would hand
// ed25519.Sign/Verify their one panic condition at backup or verify time.
func FuzzParseKeys(f *testing.F) {
	pubPEM, privPEM, err := GenerateKeyPair()
	if err != nil {
		f.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		f.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}

	f.Add(privPEM)
	f.Add(pubPEM)
	f.Add(privPEM[:len(privPEM)/2])
	f.Add(bytes.ReplaceAll(privPEM, []byte("M"), []byte("!")))
	f.Add(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})) // right container, wrong key type
	f.Add([]byte("-----BEGIN PRIVATE KEY-----\n\n-----END PRIVATE KEY-----\n"))
	f.Add(seed)
	f.Add(make([]byte, ed25519.PrivateKeySize))
	f.Add([]byte(base64.StdEncoding.EncodeToString(seed)))
	f.Add([]byte(hex.EncodeToString(bytes.Repeat([]byte{7}, EncryptionKeySize))))
	f.Add([]byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, EncryptionKeySize))))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 { // keys are hundreds of bytes; keep execs fast
			return
		}
		msg := []byte("canonical manifest bytes")
		if priv, err := ParsePrivateKey(data); err == nil {
			if len(priv) != ed25519.PrivateKeySize {
				t.Fatalf("parsed private key has length %d", len(priv))
			}
			sig := Sign(priv, msg)
			// Raw 64-byte input may carry a public half unrelated to its seed; such a
			// key signs unverifiably and fails closed at verify time. Only a
			// half-consistent key owes a working round trip.
			if bytes.Equal(priv, ed25519.NewKeyFromSeed(priv.Seed())) {
				if err := Verify(priv.Public().(ed25519.PublicKey), msg, sig); err != nil {
					t.Fatalf("self-signed message does not verify: %v", err)
				}
			}
		}
		if pub, err := ParsePublicKey(data); err == nil {
			if len(pub) != ed25519.PublicKeySize {
				t.Fatalf("parsed public key has length %d, ed25519.Verify would panic", len(pub))
			}
			_ = Verify(pub, msg, bytes.Repeat([]byte{1}, ed25519.SignatureSize))
		}
		if k, err := ParseEncryptionKey(data); err == nil && len(k) != EncryptionKeySize {
			t.Fatalf("parsed encryption key has length %d", len(k))
		}
	})
}
