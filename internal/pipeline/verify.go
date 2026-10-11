package pipeline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
)

// VerifyDeps are the inputs to a verify.
type VerifyDeps struct {
	Dest      dest.Destination
	PublicKey ed25519.PublicKey
	Logger    *slog.Logger
	// Concurrency is how many artifacts Verify reads back at once. The CLI passes
	// backup.concurrency, the bound an operator already set on what a backup has in flight. Below
	// one is one, so a caller that sets nothing reads one artifact at a time, as Verify always did.
	Concurrency int
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
//
// It reads at most maxManifestBytes of the manifest, the cap every other reader has. It used to
// read the whole object, so verify read a manifest that restore and drill then refused, and a
// large enough object could take the memory of whatever ran it.
func Verify(ctx context.Context, d VerifyDeps, manifestKey string) (*VerifyResult, error) {
	return verifyWithin(ctx, d, manifestKey, maxManifestBytes)
}

// verifyWithin is Verify reading at most limit bytes of the manifest, so a test can reach the cap
// without a manifest that large.
func verifyWithin(ctx context.Context, d VerifyDeps, manifestKey string, limit int64) (*VerifyResult, error) {
	log := orDefault(d.Logger)
	res := &VerifyResult{ManifestKey: manifestKey}

	canon, err := readCapped(ctx, d.Dest, manifestKey, limit)
	var large *objectTooLarge
	if errors.As(err, &large) {
		return res, fmt.Errorf("read manifest: %s is larger than the %s this engine reads", manifestKey, mib(limit))
	}
	if err != nil {
		return res, fmt.Errorf("read manifest: %w", err)
	}
	sigB64, err := readCapped(ctx, d.Dest, manifestKey+".sig", maxSignatureBytes)
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

	checks := readBack(ctx, d.Dest, m.Repos, d.Concurrency)
	res.ArtifactsChecked = len(checks)
	for _, c := range checks {
		if !c.ok {
			res.Failures = append(res.Failures, c.failure)
			continue
		}
		res.ArtifactsOK++
	}

	if len(res.Failures) > 0 {
		return res, fmt.Errorf("verify: %d artifact failure(s)", len(res.Failures))
	}
	log.Info("verify ok", "manifest", manifestKey, "artifacts", res.ArtifactsOK)
	return res, nil
}

// artifactCheck is what reading one artifact back found: ok once its SHA-256 was seen to be the one
// the manifest records, and otherwise the failure verify reports for it.
//
// A flag that says ok, rather than an empty failure, so that the zero value is a failure. A check
// that no read filled in, say once a later change stops asking for the rest after a stop or stops
// waiting for a slow read, then fails the verify, where an empty failure would have counted an
// artifact as verified without reading a byte of it.
type artifactCheck struct {
	ok      bool
	failure string
}

// readBack reads back every artifact the repositories list, up to limit at a time, and returns what
// it found for each, in the manifest's order.
//
// Each read fills only its own slot, and the slots are read once every read has finished, so the
// failures come in the manifest's order whatever order the reads finish in: the result, and the JSON
// printed from it, are the ones verify gave reading one artifact at a time. Once ctx is done the rest
// are still asked for, as they were one at a time, and a store that honours ctx fails each of those
// reads at once with its own error, so a stopped verify still names every artifact it did not read.
// A read streams the object through SHA-256, so verify holds a copy buffer per read in flight and
// never an artifact, whatever its size.
func readBack(ctx context.Context, d dest.Destination, repos []RepoEntry, limit int) []artifactCheck {
	n := 0
	for _, repo := range repos {
		n += len(repo.Artifacts)
	}
	checks := make([]artifactCheck, n)
	sem := make(chan struct{}, max(1, min(limit, n)))
	var wg sync.WaitGroup
	i := 0
	for _, repo := range repos {
		for _, a := range repo.Artifacts {
			slot := i
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				checks[slot] = checkArtifact(ctx, d, a)
			})
			i++
		}
	}
	wg.Wait()
	return checks
}

// checkArtifact reads one artifact back and checks its SHA-256 against the one the manifest records.
func checkArtifact(ctx context.Context, d dest.Destination, a ArtifactInfo) artifactCheck {
	got, err := getSHA(ctx, d, a.Key)
	if err != nil {
		return artifactCheck{failure: fmt.Sprintf("%s: %v", a.Key, err)}
	}
	if !strings.EqualFold(got, a.SHA256) {
		return artifactCheck{failure: fmt.Sprintf("%s: checksum mismatch", a.Key)}
	}
	return artifactCheck{ok: true}
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

	canon, err := readCapped(ctx, d.Dest, drillKey, maxManifestBytes)
	if err != nil {
		return res, fmt.Errorf("read drill report: %w", err)
	}
	sigB64, err := readCapped(ctx, d.Dest, drillKey+".sig", maxSignatureBytes)
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
