// Package buildinfo carries version metadata stamped in at link time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These are overridden via -ldflags at release time. Defaults describe a
// developer build so `meshflash upgrade` can refuse to clobber it.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Repo is the canonical source of releases for `meshflash upgrade`.
const Repo = "jclement/meshflash"

// IsRelease reports whether this binary came from a tagged release. Developer
// builds skip self-update so a local build is never silently replaced.
func IsRelease() bool { return Version != "dev" && Version != "" }

// Platform is the GOOS/GOARCH pair used to pick a release asset.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// UserAgent identifies meshflash to GitHub so rate-limit issues are traceable.
func UserAgent() string {
	return fmt.Sprintf("meshflash/%s (%s)", Version, Platform())
}

// String renders the full version banner.
func String() string {
	return fmt.Sprintf("meshflash %s (commit %s, built %s, %s, %s)",
		Version, Commit, Date, Platform(), runtime.Version())
}

func init() {
	// When installed via `go install`, ldflags are absent but the module
	// version is embedded. Prefer that over the "dev" placeholder.
	if Version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "none" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == "unknown" {
				Date = s.Value
			}
		}
	}
}
