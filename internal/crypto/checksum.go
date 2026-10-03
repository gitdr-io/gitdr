// Package crypto provides the integrity and signing primitives gitdr relies on:
// SHA-256 checksums for every artifact and Ed25519 signatures for the run-manifest.
package crypto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"gitdr.io/gitdr/internal/ctxio"
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

// SHA256File hashes the file at path. Once ctx is done the read stops at its next chunk with ctx's
// error, so a stopped backup does not wait for the hash of an artifact of many GiB.
func SHA256File(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("sha256 open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return SHA256Hex(ctxio.Reader(ctx, f))
}
