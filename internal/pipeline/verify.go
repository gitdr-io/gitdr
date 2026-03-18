package pipeline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
)

// VerifyDeps are the inputs to a verify.
type VerifyDeps struct {
	Dest      dest.Destination
	PublicKey ed25519.PublicKey
	Logger    *slog.Logger
}

// VerifyResult reports signature and per-artifact checksum results.
type VerifyResult struct {
	ManifestKey      string   `json:"manifestKey"`
	SignatureValid   bool     `json:"signatureValid"`
	ArtifactsChecked int      `json:"artifactsChecked"`
	ArtifactsOK      int      `json:"artifactsOk"`
	Failures         []string `json:"failures,omitempty"`
}

// Verify checks the manifest's Ed25519 signature, then re-reads every referenced
// artifact and recomputes its SHA-256 against the manifest. Read-only.
func Verify(ctx context.Context, d VerifyDeps, manifestKey string) (*VerifyResult, error) {
	log := orDefault(d.Logger)
	res := &VerifyResult{ManifestKey: manifestKey}

	canon, err := getBytes(ctx, d.Dest, manifestKey)
	if err != nil {
		return res, fmt.Errorf("read manifest: %w", err)
	}
	sigB64, err := getBytes(ctx, d.Dest, manifestKey+".sig")
	if err != nil {
		return res, fmt.Errorf("read signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return res, fmt.Errorf("decode signature: %w", err)
	}
	if err := crypto.Verify(d.PublicKey, canon, sig); err != nil {
		res.Failures = append(res.Failures, "manifest signature invalid")
		return res, fmt.Errorf("verify: %w", err)
	}
	res.SignatureValid = true

	var m Manifest
	if err := json.Unmarshal(canon, &m); err != nil {
		return res, fmt.Errorf("parse manifest: %w", err)
	}

	for _, repo := range m.Repos {
		for _, a := range repo.Artifacts {
			res.ArtifactsChecked++
			got, err := getSHA(ctx, d.Dest, a.Key)
			if err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", a.Key, err))
				continue
			}
			if !strings.EqualFold(got, a.SHA256) {
				res.Failures = append(res.Failures, fmt.Sprintf("%s: checksum mismatch", a.Key))
				continue
			}
			res.ArtifactsOK++
		}
	}

	if len(res.Failures) > 0 {
		return res, fmt.Errorf("verify: %d artifact failure(s)", len(res.Failures))
	}
	log.Info("verify ok", "manifest", manifestKey, "artifacts", res.ArtifactsOK)
	return res, nil
}

func getBytes(ctx context.Context, d dest.Destination, key string) ([]byte, error) {
	rc, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// getSHA streams an object through SHA-256 without buffering it whole.
func getSHA(ctx context.Context, d dest.Destination, key string) (string, error) {
	rc, err := d.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	sum, _, err := crypto.SHA256Hex(rc)
	return sum, err
}
