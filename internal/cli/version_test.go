package cli

import "testing"

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name    string
		ldflags string
		main    string
		want    string
	}{
		{"release build wins", "0.1.8", "v0.1.8", "0.1.8"},
		{"go install falls back to module version", "dev", "v0.1.8", "v0.1.8"},
		{"working-tree build stays dev", "dev", "(devel)", "dev"},
		{"no build info stays dev", "dev", "", "dev"},
		{"ldflags beats a disagreeing module version", "0.2.0", "v0.1.8", "0.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.ldflags, tt.main); got != tt.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", tt.ldflags, tt.main, got, tt.want)
			}
		})
	}
}
