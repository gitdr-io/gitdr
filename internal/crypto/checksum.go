// Package crypto provides the integrity and signing primitives gitdr relies on:
// SHA-256 checksums for every artifact and Ed25519 signatures for the run-manifest.
package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// SHA256Hex streams r through SHA-256 and returns the lowercase hex digest and the
// number of bytes hashed.
func SHA256Hex(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, fmt.Errorf("sha256: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SHA256Bytes returns the lowercase hex SHA-256 of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SHA256File hashes the file at path.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("sha256 open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return SHA256Hex(f)
}
