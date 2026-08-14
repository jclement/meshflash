// Package buildinfo carries version metadata stamped in at link time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
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

// release records whether this binary came from a real release, decided once
// in init. It is not derived from Version at call time because Go stamps a
// VCS pseudo-version (v0.0.0-20260814182937-369570f2efea) into ordinary local
// builds — treating that as a release would let `meshflash upgrade` overwrite
// a developer's own binary with a published one.
var release bool

// IsRelease reports whether this binary came from a tagged release. Developer
// builds skip self-update so a local build is never silently replaced.
func IsRelease() bool { return release }

// isCleanSemver matches a plain tagged version: no prerelease suffix and, in
// particular, no pseudo-version timestamp.
func isCleanSemver(v string) bool {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

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
	// A version supplied via -ldflags is by definition a release build.
	if Version != "dev" && Version != "" {
		release = true
		return
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	// `go install ...@v1.4.0` embeds a real tag and deserves self-update.
	// A local build embeds a pseudo-version or "(devel)" and does not.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		Version = v
		release = isCleanSemver(v)
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
