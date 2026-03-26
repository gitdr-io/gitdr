package cli

import "runtime/debug"

// Version is set at build time via -ldflags "-X gitdr.io/gitdr/internal/cli.Version=...".
var Version = "dev"

// version returns the build version, appending the VCS revision when available.
func version() string {
	v := Version
	if info, ok := debug.ReadBuildInfo(); ok {
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
