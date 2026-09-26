package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// The release notes a user reads before upgrading come from docs/releases/<tag>.md, pulled into
// the GitHub release by the header template in .goreleaser.yaml.
//
// That template fails silently by design. GoReleaser's readFile returns nothing for a file it
// cannot read, and an empty header is dropped, which is right for a release with no notes and
// wrong for one whose path was mistyped: the release would go out without the warning and
// nothing would say so. So this renders the configured template the way GoReleaser v2.18.2 does
// (text/template, missingkey=error, readFile trimming the file and swallowing errors, paths
// relative to the repository root) for every note in the directory.
func TestEveryReleaseNoteRendersAsTheReleaseHeader(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Release struct {
			Header string `yaml:"header"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	if cfg.Release.Header == "" {
		t.Fatal(".goreleaser.yaml has no release.header, so no release note reaches a release")
	}

	readFile := func(path string) string {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	header, err := template.New("header").Option("missingkey=error").
		Funcs(template.FuncMap{"readFile": readFile}).Parse(cfg.Release.Header)
	if err != nil {
		t.Fatalf("release.header is not a valid template: %v", err)
	}
	render := func(tag string) string {
		t.Helper()
		var out strings.Builder
		if err := header.Execute(&out, map[string]any{"Tag": tag}); err != nil {
			t.Fatalf("render the header for %s: %v", tag, err)
		}
		return out.String()
	}

	notes, err := filepath.Glob(filepath.Join(root, "docs", "releases", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	// A glob that matches nothing asserts nothing, and would keep passing after the notes moved.
	if len(notes) == 0 {
		t.Fatal("no release notes in docs/releases")
	}
	tagName := regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	for _, note := range notes {
		tag := strings.TrimSuffix(filepath.Base(note), ".md")
		t.Run(tag, func(t *testing.T) {
			if !tagName.MatchString(tag) {
				t.Fatalf("%s is not named for a release tag, so no release will ever read it", filepath.Base(note))
			}
			want := readFile(filepath.Join("docs", "releases", filepath.Base(note)))
			if want == "" {
				t.Fatal("the note is empty")
			}
			if got := render(tag); got != want {
				t.Errorf("the %s release header is not its note:\n got %q\nwant %q", tag, got, want)
			}
			// The owner's voice, which these are written in.
			if strings.Contains(want, "—") {
				t.Error("the note has an em dash")
			}
		})
	}

	// A release without a note renders nothing, which GoReleaser drops.
	if got := render("v0.0.0-no-such-release"); got != "" {
		t.Errorf("a release with no note gets a header anyway: %q", got)
	}
}
