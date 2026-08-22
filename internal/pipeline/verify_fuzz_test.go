package pipeline_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/pipeline"
)

// FuzzVerifyManifest hands Verify arbitrary manifest and signature bytes, which is
// exactly the position of an attacker who reached the bucket. The claim under test is
// the product's own: SignatureValid tracks stdlib ed25519 over the exact stored bytes,
// and Verify never reports success unless the signature and every artifact check out.
func FuzzVerifyManifest(f *testing.F) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0xA5}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	stranger := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5A}, ed25519.SeedSize))

	artifact := []byte("bundle bytes")
	const artifactKey = "github.com/octo/hello/2026-06-13/hello.bundle"
	ts := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	m := &pipeline.Manifest{
		Schema:      pipeline.ManifestSchema,
		RunID:       "20260613T120000Z-fuzz00000000",
		Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "fuzz"},
		Source:      pipeline.SourceInfo{Type: "github", Host: "github.com"},
		Destination: pipeline.DestInfo{Type: "s3", Bucket: "b", WormImmutable: true},
		StartedAt:   ts,
		FinishedAt:  ts,
		Status:      pipeline.StatusSuccess,
		Repos: []pipeline.RepoEntry{{
			Slug:   "octo/hello",
			Status: pipeline.StatusSuccess,
			Artifacts: []pipeline.ArtifactInfo{{
				Kind: "bundle", Key: artifactKey, Size: int64(len(artifact)),
				SHA256: crypto.SHA256Bytes(artifact), RetainUntil: ts,
			}},
		}},
	}
	canon, err := m.Canonical()
	if err != nil {
		f.Fatal(err)
	}
	sigOf := func(k ed25519.PrivateKey, b []byte) []byte {
		return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(k, b)))
	}
	tampered := bytes.Clone(canon)
	tampered[len(tampered)/2] ^= 1

	f.Add(canon, []byte{}, true)                                                    // intact and signed, end to end ok
	f.Add(canon, sigOf(priv, canon), false)                                         // same, via explicit sidecar bytes
	f.Add(canon, append([]byte("\n "), append(sigOf(priv, canon), '\n')...), false) // whitespace around the sidecar
	f.Add(tampered, sigOf(priv, canon), false)                                      // flipped bit, stale signature
	f.Add(canon[:len(canon)/2], sigOf(priv, canon), false)                          // truncated manifest
	f.Add(canon, sigOf(stranger, canon), false)                                     // signed by someone else's key
	f.Add(canon, []byte("!not base64!"), false)
	f.Add(canon, []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))), false)
	f.Add([]byte{}, []byte{}, false)
	f.Add([]byte("{"), []byte{}, true) // valid signature over invalid JSON
	checksumLie := `{"schema":"gitdr.manifest/v2","repos":[{"slug":"octo/hello","status":"success",` +
		`"artifacts":[{"kind":"bundle","key":"` + artifactKey + `","sha256":"beef"}]}]}`
	f.Add([]byte(checksumLie), []byte{}, true)                                  // signed checksum lie
	f.Add([]byte(`{"repos":[{"artifacts":[{"key":"gone"}]}]}`), []byte{}, true) // signed reference to a missing artifact

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.Fuzz(func(t *testing.T, manifest, sig []byte, signIt bool) {
		if len(manifest)+len(sig) > 1<<20 { // keep execs fast; structure saturates far below this
			return
		}
		if signIt {
			sig = sigOf(priv, manifest)
		}
		md := newMemDest(true)
		md.objs["m"] = bytes.Clone(manifest)
		md.objs["m.sig"] = bytes.Clone(sig)
		md.objs[artifactKey] = artifact

		res, err := pipeline.Verify(context.Background(),
			pipeline.VerifyDeps{Dest: md, PublicKey: pub, Logger: log}, "m")

		wantValid := false
		if dec, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig))); decErr == nil {
			wantValid = ed25519.Verify(pub, manifest, dec)
		}
		if res.SignatureValid != wantValid {
			t.Fatalf("SignatureValid = %v, ed25519 over the stored bytes says %v", res.SignatureValid, wantValid)
		}
		if !wantValid && err == nil {
			t.Fatal("verify reported success with an invalid signature")
		}
		if err == nil && res.ArtifactsOK != res.ArtifactsChecked {
			t.Fatalf("verify reported success with %d/%d artifacts ok", res.ArtifactsOK, res.ArtifactsChecked)
		}
	})
}
