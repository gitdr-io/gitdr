package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/pipeline"
)

func sampleResult() *pipeline.BackupResult {
	ts := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	return &pipeline.BackupResult{
		Manifest: &pipeline.Manifest{
			Schema:      pipeline.ManifestSchema,
			RunID:       "20260613T120000Z-a1b2c3d4e5f6",
			Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "test"},
			Source:      pipeline.SourceInfo{Type: "gitlab", Host: "gitlab.com"},
			Destination: pipeline.DestInfo{Type: "s3", Bucket: "b", WormImmutable: false},
			StartedAt:   ts,
			FinishedAt:  ts.Add(7 * time.Second),
			Status:      "success",
			Repos:       []pipeline.RepoEntry{{Slug: "acme/api", Status: pipeline.StatusSuccess}},
		},
		ManifestKey: "gitlab.com/acme/manifests/20260613T120007Z.manifest.json",
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// Pins the `backup --output json` contract. The manifest's own fields stay at the top level,
// where every existing consumer already reads them, and manifestKey sits alongside.
//
// The key matters because `gitdr verify -manifest <key>` cannot be called without it, and it
// is deliberately not inside the manifest: the manifest is signed, and a document that names
// its own location changes every time it is copied.
func TestBackupJSONShape(t *testing.T) {
	out := captureStdout(t, func() { emitBackup("json", sampleResult()) })

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	for _, field := range []string{
		"schema", "runId", "tool", "source", "destination",
		"startedAt", "finishedAt", "status", "repos", "manifestKey",
	} {
		if _, ok := got[field]; !ok {
			t.Errorf("missing %q from backup --output json; that is a contract change", field)
		}
	}

	if got["manifestKey"] != "gitlab.com/acme/manifests/20260613T120007Z.manifest.json" {
		t.Errorf("manifestKey = %v, want the key the manifest was stored under", got["manifestKey"])
	}
	if got["schema"] != pipeline.ManifestSchema {
		t.Errorf("schema = %v, want %v", got["schema"], pipeline.ManifestSchema)
	}
}

// The manifest is signed over its canonical bytes; adding a field to stdout must not have
// changed the document itself. If manifestKey ever appears in the canonical form, the
// signature covers a location rather than a copy, and every re-upload invalidates it.
func TestManifestKeyStaysOutOfTheSignedDocument(t *testing.T) {
	res := sampleResult()

	canon, err := res.Manifest.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if strings.Contains(string(canon), "manifestKey") {
		t.Fatal("manifestKey leaked into the signed manifest")
	}

	var signed map[string]any
	if err := json.Unmarshal(canon, &signed); err != nil {
		t.Fatalf("canonical is not valid JSON: %v", err)
	}
	if _, ok := signed["manifestKey"]; ok {
		t.Fatal("manifestKey is present in the canonical manifest")
	}
}

// Text output is for humans and keeps naming the key it always did.
func TestBackupTextStillReportsTheKey(t *testing.T) {
	out := captureStdout(t, func() { emitBackup("text", sampleResult()) })

	if !strings.Contains(out, "manifest: gitlab.com/acme/manifests/20260613T120007Z.manifest.json") {
		t.Errorf("text output lost the manifest key:\n%s", out)
	}
	if !strings.Contains(out, "acme/api") {
		t.Errorf("text output lost the repo list:\n%s", out)
	}
}
