package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// Run-manifest signing uses Ed25519 detached signatures. Keys are accepted as PEM
// (PKCS#8 private / PKIX public) or as raw/base64 key bytes, so operators can supply
// them via mounted files or environment variables.

// ParsePrivateKey decodes an Ed25519 private key from PEM, raw, or base64 input.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	data = bytes.TrimSpace(data)
	if blk, _ := pem.Decode(data); blk != nil {
		key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse pkcs8 private key: %w", err)
		}
		ed, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is %T, want ed25519", key)
		}
		return ed, nil
	}
	raw := decodeRaw(data)
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, errors.New("unrecognized ed25519 private key (want PEM PKCS#8, 64-byte key, or 32-byte seed)")
}

// ParsePublicKey decodes an Ed25519 public key from PEM, raw, or base64 input.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	data = bytes.TrimSpace(data)
	if blk, _ := pem.Decode(data); blk != nil {
		key, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse pkix public key: %w", err)
		}
		ed, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is %T, want ed25519", key)
		}
		return ed, nil
	}
	raw := decodeRaw(data)
	if len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	return nil, errors.New("unrecognized ed25519 public key (want PEM PKIX or 32-byte key)")
}

func decodeRaw(data []byte) []byte {
	if dec, err := base64.StdEncoding.DecodeString(string(data)); err == nil {
		return dec
	}
	return data
}

// Sign returns a detached Ed25519 signature over msg.
func Sign(priv ed25519.PrivateKey, msg []byte) []byte {
	return ed25519.Sign(priv, msg)
}

// Verify checks a detached Ed25519 signature, returning an error on mismatch.
func Verify(pub ed25519.PublicKey, msg, sig []byte) error {
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("ed25519 signature verification failed")
	}
	return nil
}

// GenerateKeyPair creates an Ed25519 keypair and returns PEM-encoded public (PKIX)
// and private (PKCS#8) keys. Primarily for tests and key bootstrap.
func GenerateKeyPair() (pubPEM, privPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal public key: %w", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})
	return pubPEM, privPEM, nil
}
