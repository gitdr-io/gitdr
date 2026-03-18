package pipeline_test

import (
	"encoding/json"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/pipeline"
)

// Pins the gitdr.manifest/v2 field set. A change here means the public output contract
// changed, bump the schema version and update SPEC.md §14 deliberately.
func TestManifestV2Shape(t *testing.T) {
	if pipeline.ManifestSchema != "gitdr.manifest/v2" {
		t.Fatalf("manifest schema is now %q, that is a breaking contract change", pipeline.ManifestSchema)
	}

	ts := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	m := &pipeline.Manifest{
		Schema:      pipeline.ManifestSchema,
		RunID:       "20260613T120000Z-a1b2c3d4e5f6",
		Tool:        pipeline.ToolInfo{Name: "gitdr", Version: "test"},
		Source:      pipeline.SourceInfo{Type: "github", Host: "github.com"},
		Destination: pipeline.DestInfo{Type: "s3", Bucket: "b", WormMode: "COMPLIANCE", WormImmutable: true, WormDetails: "Object Lock enabled"},
		StartedAt:   ts,
		FinishedAt:  ts,
		Status:      pipeline.StatusSuccess,
		Repos: []pipeline.RepoEntry{{
			Slug:      "octo/hello",
			Status:    pipeline.StatusSuccess,
			Artifacts: []pipeline.ArtifactInfo{{Kind: "bundle", Key: "k", Size: 1, SHA256: "h", RetainUntil: ts}},
		}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	top := decode(t, b)
	checkKeys(t, "manifest", top, "schema", "runId", "tool", "source", "destination", "startedAt", "finishedAt", "status", "repos")
	checkKeys(t, "tool", decode(t, top["tool"]), "name", "version")
	checkKeys(t, "source", decode(t, top["source"]), "type", "host")
	checkKeys(t, "destination", decode(t, top["destination"]), "type", "bucket", "wormMode", "wormImmutable", "wormDetails")

	var repos []json.RawMessage
	if err := json.Unmarshal(top["repos"], &repos); err != nil {
		t.Fatal(err)
	}
	repo := decode(t, repos[0])
	checkKeys(t, "repos[]", repo, "slug", "status", "artifacts") // error is omitempty
	var arts []json.RawMessage
	if err := json.Unmarshal(repo["artifacts"], &arts); err != nil {
		t.Fatal(err)
	}
	checkKeys(t, "artifacts[]", decode(t, arts[0]), "kind", "key", "size", "sha256", "retainUntil")
}

func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func checkKeys(t *testing.T, where string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Fatalf("%s: field set changed (versioned contract, bump schema + SPEC §14): got %v, want %v", where, keys, want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s: missing contract field %q", where, k)
		}
	}
}
