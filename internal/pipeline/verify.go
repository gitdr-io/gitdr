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
	// Refused rather than counted, and this is not pedantry about a field.
	//
	// Verify is schema-agnostic up to this point: it fetches the object and its .sig and checks
	// the signature, which is right, because the signature is over bytes. Then it unmarshals into
	// a Manifest, and a drill report unmarshals into a Manifest without error - it simply has no
	// artifacts. So `verify -manifest {ts}.drill.json` printed "signature valid, 0 of 0 artifacts
	// ok" and exited zero: a check that passes on a document it does not understand, and that
	// would go on passing if the report were swapped for anything else signed by the same key.
	//
	// A drill report is signed evidence and deserves a real check; it is just not this one.
	if m.Schema != "" && m.Schema != ManifestSchema && !strings.HasPrefix(m.Schema, "gitdr.manifest/") {
		return res, fmt.Errorf("%s is a %s document, not a manifest: verify checks manifests, and counting its zero artifacts as a pass would be a green over a document this command cannot read", manifestKey, m.Schema)
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

// VerifyDrillResult is what a drill report says about itself, once its signature has held.
//
// Deliberately not VerifyResult with different meanings poured into the same fields. A manifest
// verification counts artifacts read back out of the bucket; this one reads nothing back and
// counts nothing. Reusing `artifactsChecked` to mean "repositories the report claims" would put
// two different measurements behind one name, which is the collapse this codebase keeps undoing.
type VerifyDrillResult struct {
	DrillKey       string `json:"drillKey"`
	SignatureValid bool   `json:"signatureValid"`
	Schema         string `json:"schema"`
	DrillID        string `json:"drillId"`
	// The manifest the drill tested, as the report names it.
	ManifestKey string `json:"manifestKey"`
	// Whether that manifest's own signature was checked before its contents were believed. The
	// report carries a plain bool, so this is the report's word, verbatim.
	ManifestSigned bool   `json:"manifestSigned"`
	Status         string `json:"status"`
	Eligible       int    `json:"eligible"`
	Drilled        int    `json:"drilled"`
	// Repositories the report records as not coming back, named so a reader has something to act
	// on rather than a count to trust.
	Failures []string `json:"failures,omitempty"`
}

// VerifyDrill checks a drill report's Ed25519 signature and reports what the document claims.
//
// It does not re-drill, and it reads no artifacts. The question it answers is the one an auditor
// holding a printed evidence pack actually has: is this document authentic, and what does it say.
// Re-running the drill is a different and much more expensive question, and conflating them would
// make the cheap check unavailable.
//
// In the engine rather than the control plane because the platform holds no read credentials for
// the customer's bucket and structurally cannot fetch the signed object. This serves the operator
// running the container from cron, who has no account at all, with the same command.
func VerifyDrill(ctx context.Context, d VerifyDeps, drillKey string) (*VerifyDrillResult, error) {
	res := &VerifyDrillResult{DrillKey: drillKey}

	canon, err := getBytes(ctx, d.Dest, drillKey)
	if err != nil {
		return res, fmt.Errorf("read drill report: %w", err)
	}
	sigB64, err := getBytes(ctx, d.Dest, drillKey+".sig")
	if err != nil {
		return res, fmt.Errorf("read signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return res, fmt.Errorf("decode signature: %w", err)
	}
	if err := crypto.Verify(d.PublicKey, canon, sig); err != nil {
		return res, fmt.Errorf("verify: %w", err)
	}
	res.SignatureValid = true

	var r DrillReport
	if err := json.Unmarshal(canon, &r); err != nil {
		return res, fmt.Errorf("parse drill report: %w", err)
	}
	// The mirror of the refusal in Verify. A manifest unmarshals into a DrillReport as happily as
	// the reverse, and reporting "0 of 0 restored, signature valid" over one would be the same
	// green on a document this command cannot read.
	if r.Schema != DrillSchema {
		return res, fmt.Errorf("%s is a %q document, not a drill report: use `gitdr verify -manifest` for a manifest", drillKey, r.Schema)
	}

	res.Schema, res.DrillID, res.ManifestKey = r.Schema, r.DrillID, r.ManifestKey
	res.ManifestSigned, res.Status = r.ManifestSigned, r.Status
	res.Eligible, res.Drilled = r.Eligible, r.Drilled
	for _, repo := range r.Repos {
		if repo.Status != StatusSuccess {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %s", repo.Slug, repo.Error))
		}
	}
	return res, nil
}
