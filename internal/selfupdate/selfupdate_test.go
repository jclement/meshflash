package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAssetNameMatchesGoReleaser pins the contract between this package and
// .goreleaser.yaml's archive name_template.
//
// If they drift, `meshflash upgrade` keeps working right up until a release is
// published and then fails for every user at once, with nothing in CI having
// caught it. The names are cheap to assert, so they are asserted.
func TestAssetNameMatchesGoReleaser(t *testing.T) {
	const wantTemplate = `name_template: "meshflash_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`

	data, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Skipf("no .goreleaser.yaml to check against: %v", err)
	}
	if !strings.Contains(string(data), wantTemplate) {
		t.Errorf("archive name_template in .goreleaser.yaml no longer matches AssetName.\n"+
			"AssetName produces meshflash_<version>_<goos>_<goarch>, which requires:\n  %s", wantTemplate)
	}

	// GoReleaser appends an ARM variant suffix (armv7) unless the template
	// omits it. AssetName uses a bare runtime.GOARCH ("arm"), so the template
	// must not carry {{ .Arm }}.
	if strings.Contains(string(data), "{{ .Arm }}") {
		t.Error("archive name_template includes {{ .Arm }}, which AssetName does not produce")
	}

	// selfupdate looks up this exact filename in the release assets.
	if !strings.Contains(string(data), "name_template: checksums.txt") {
		t.Error(".goreleaser.yaml must name the checksum file exactly checksums.txt")
	}
}

func TestAssetName(t *testing.T) {
	got := AssetName("1.4.0")

	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	want := "meshflash_1.4.0_" + runtime.GOOS + "_" + runtime.GOARCH + ext
	if got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}

	// The version must not carry a leading v: Check() strips it from the tag,
	// and GoReleaser's .Version is likewise tag-minus-v.
	if strings.Contains(got, "_v1.4.0_") {
		t.Error("AssetName should not include a leading v in the version")
	}
}

func TestBinaryName(t *testing.T) {
	want := "meshflash"
	if runtime.GOOS == "windows" {
		want = "meshflash.exe"
	}
	if got := binaryName(); got != want {
		t.Errorf("binaryName = %q, want %q", got, want)
	}
}
