package buildinfo

import "testing"

// TestIsCleanSemver guards the rule that decides whether `meshflash upgrade`
// is allowed to replace this binary.
//
// Go stamps a VCS pseudo-version into ordinary local builds, so accepting
// anything non-empty would let an upgrade overwrite a developer's own build
// with a published release.
func TestIsCleanSemver(t *testing.T) {
	releases := []string{"v1.4.0", "1.4.0", "v0.0.1", "v10.20.30"}
	for _, v := range releases {
		if !isCleanSemver(v) {
			t.Errorf("isCleanSemver(%q) = false, want true", v)
		}
	}

	notReleases := []string{
		"",
		"dev",
		"(devel)",
		"v0.0.0-20260814182937-369570f2efea", // pseudo-version from a local build
		"v1.4.0-rc1",
		"v1.4",
		"v1.4.0.1",
		"vX.Y.Z",
		"dev-369570f",
	}
	for _, v := range notReleases {
		if isCleanSemver(v) {
			t.Errorf("isCleanSemver(%q) = true, want false", v)
		}
	}
}

// A build with no ldflags and no usable module version must never self-update.
func TestDevBuildIsNotRelease(t *testing.T) {
	// The test binary itself is built without release ldflags.
	if Version == "dev" && IsRelease() {
		t.Error("a build with the default dev version must not report as a release")
	}
}
