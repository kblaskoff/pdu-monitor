// Package buildinfo tells which version of the source a binary was built from. The updater rebuilds from a clone, so the commit that
// Go records in the binary is the version: no -ldflags are needed (and none can be forgotten).
package buildinfo

import "runtime/debug"

// Version is the 12 characters of the commit (with "+local" when files were changed), or fallback when the binary was not built from a
// git checkout (go run, a source archive).
func Version(fallback string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fallback
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return fallback
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "+local"
	}
	return rev
}
