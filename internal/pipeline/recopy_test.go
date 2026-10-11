package pipeline

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/config"
)

// Which names a recopy takes, and what it says about the rest. A name is the slug the manifest
// records, compared exactly: case counts, and a pattern is only a name. Spaces around a name and
// empty items are dropped, as SplitList drops them from the flag and the environment, because a
// YAML list can carry them too. A name given twice is one name, and one the source did not list is
// warned about once.
func TestARecopyTakesOnlyTheNamesTheSourceListed(t *testing.T) {
	selected := map[string]bool{"octo/a": true, "acme/platform/api": true}
	for _, tc := range []struct {
		name   string
		recopy []string
		named  []string // sorted
		warned []string // the repo each warning names, in order
	}{
		{name: "none"},
		{name: "listed", recopy: []string{"octo/a", "acme/platform/api"}, named: []string{"acme/platform/api", "octo/a"}},
		{name: "case counts", recopy: []string{"Octo/A"}, warned: []string{"Octo/A"}},
		{name: "a pattern is a name", recopy: []string{"octo/*"}, warned: []string{"octo/*"}},
		{name: "spaces and empty items", recopy: []string{" octo/a ", "", " "}, named: []string{"octo/a"}},
		{name: "given twice", recopy: []string{"octo/gone", "octo/a", " octo/gone", "octo/a"}, named: []string{"octo/a"}, warned: []string{"octo/gone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Backup.Recopy = tc.recopy
			var logged bytes.Buffer
			r := &backupRun{cfg: cfg, log: slog.New(slog.NewJSONHandler(&logged, nil))}

			if got := slices.Sorted(maps.Keys(r.namedForRecopy(selected))); !slices.Equal(got, tc.named) {
				t.Errorf("named %q, want %q", got, tc.named)
			}
			var warned []string
			for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
				if line == "" {
					continue
				}
				var l struct{ Level, Repo string }
				if err := json.Unmarshal([]byte(line), &l); err != nil {
					t.Fatalf("a log line that is not JSON: %q", line)
				}
				if l.Level == "WARN" {
					warned = append(warned, l.Repo)
				}
			}
			if !slices.Equal(warned, tc.warned) {
				t.Errorf("warned about %q, want %q:\n%s", warned, tc.warned, logged.String())
			}
		})
	}
}
