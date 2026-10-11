package cli

import (
	"flag"
	"io"
	"slices"
	"testing"
)

// --recopy names the repositories as a comma-separated list, and when it names any it is the list,
// over backup.recopy and GITDR_BACKUP_RECOPY, as --repo is over source.repo. Not given, or naming
// nothing, the config's list stands.
//
// Given more than once, every list counts. A caller that passes one flag per repository would
// otherwise have the last one copied and the others go on relying on the copy that failed, with
// nothing said.
func TestRecopyNames(t *testing.T) {
	for _, tc := range []struct {
		name             string
		args             []string
		configured, want []string
	}{
		{name: "neither"},
		{name: "the config", configured: []string{"octo/a"}, want: []string{"octo/a"}},
		{name: "the flag", args: []string{"-recopy", "octo/b,acme/platform/api"}, want: []string{"octo/b", "acme/platform/api"}},
		{name: "the flag over the config", args: []string{"--recopy=octo/b"}, configured: []string{"octo/a"}, want: []string{"octo/b"}},
		{name: "spaces and empty items", args: []string{"-recopy", " octo/b , ,octo/c,"}, want: []string{"octo/b", "octo/c"}},
		{name: "given twice", args: []string{"-recopy", "octo/b", "-recopy", "octo/c,octo/d"}, configured: []string{"octo/a"}, want: []string{"octo/b", "octo/c", "octo/d"}},
		{name: "naming nothing", args: []string{"-recopy", " , "}, configured: []string{"octo/a"}, want: []string{"octo/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("backup", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			given := recopyFlag(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := recopyNames(*given, tc.configured); !slices.Equal(got, tc.want) {
				t.Errorf("%q over %q = %q, want %q", tc.args, tc.configured, got, tc.want)
			}
		})
	}
}
