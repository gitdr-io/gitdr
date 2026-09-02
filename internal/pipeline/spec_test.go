package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SPEC.md is the versioned public contract. A control plane reads it to know what the manifest
// can carry, so a spec naming a version the binary stopped emitting is worse than no spec: it
// is a document that reads as authoritative and is wrong.
//
// This is not hypothetical. The manifest moved to v3 — `refs` and `copiedAt`, the two fields
// `gitdr drill` needs — while SPEC.md kept saying v2 in three places, one of them the sentence
// "the schema version is unchanged".
func TestSpecNamesTheSchemasTheBinaryEmits(t *testing.T) {
	path := filepath.Join("..", "..", "SPEC.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	spec := string(raw)

	for _, tc := range []struct {
		name   string
		schema string
		// The version this schema replaced, which must no longer be claimed as current.
		stale string
	}{
		{name: "manifest", schema: ManifestSchema, stale: "gitdr.manifest/v2"},
		{name: "drill", schema: DrillSchema},
	} {
		if !strings.Contains(spec, tc.schema) {
			t.Errorf("SPEC.md never names %s, which the binary emits", tc.schema)
		}
		if tc.stale == "" {
			continue
		}
		// A historical note may mention the old version; a JSON example claiming to be it
		// may not, because that is the part a consumer copies.
		if strings.Contains(spec, `"schema": "`+tc.stale+`"`) {
			t.Errorf("SPEC.md still shows %q as a manifest's schema value", tc.stale)
		}
	}
}
