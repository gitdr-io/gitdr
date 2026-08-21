package cli

import "runtime/debug"

// Version is set at build time via -ldflags "-X gitdr.io/gitdr/internal/cli.Version=...".
var Version = "dev"

// resolveVersion picks between the ldflags version and the module version from build info.
//
// Release builds get Version stamped and win outright. A `go install
// gitdr.io/gitdr/cmd/gitdr@vX.Y.Z` build gets no ldflags and no VCS metadata (it builds
// from the module zip, not a checkout), but the module version is recorded in the build
// info; without this fallback every binary installed that way reported "dev".
// "(devel)" is what a plain `go build` in a working tree reports, and is no version at all.
func resolveVersion(ldflags, mainVersion string) string {
	if ldflags != "dev" {
		return ldflags
	}
	if mainVersion != "" && mainVersion != "(devel)" {
		return mainVersion
	}
	return ldflags
}

// version returns the build version, appending the VCS revision when available.
func version() string {
	v := Version
	if info, ok := debug.ReadBuildInfo(); ok {
		v = resolveVersion(Version, info.Main.Version)
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				return v + " (" + rev + ")"
			}
		}
	}
	return v
}
