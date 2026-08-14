package device

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rejection records a mount point that was examined and not accepted, with the
// reason. This is what makes "the board is clearly in its bootloader but
// meshflash cannot see it" diagnosable instead of a silent hang.
type Rejection struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanVolumes returns every mounted UF2 bootloader.
func ScanVolumes() ([]Volume, error) {
	vols, _, err := ScanVolumesVerbose()
	return vols, err
}

// ScanVolumesVerbose also reports the mount points that were considered and
// rejected, which `doctor` prints and the bootloader wait logs.
func ScanVolumesVerbose() ([]Volume, []Rejection, error) {
	mounts, err := mountCandidates()
	if err != nil {
		return nil, nil, err
	}

	seen := map[string]bool{}
	var out []Volume
	var rejected []Rejection

	for _, m := range mounts {
		clean := filepath.Clean(m)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		ok, reason := looksLikeUF2Volume(clean)
		if !ok {
			rejected = append(rejected, Rejection{Path: clean, Reason: reason})
			continue
		}
		info, _ := ReadUF2Info(clean)
		out = append(out, Volume{
			Path:  clean,
			Label: volumeLabel(clean, info),
			Info:  info,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Path < rejected[j].Path })
	return out, rejected, nil
}

// bootloaderMarkers are files a UF2 bootloader exposes at the root of its
// mass-storage volume.
//
// INFO_UF2.TXT is the one worth having because it names the board, but not
// every bootloader ships it and it is not always readable, so any of these is
// enough to treat the volume as a flash target.
var bootloaderMarkers = []string{
	InfoFileName,  // INFO_UF2.TXT — Adafruit nRF52, RP2040, most others
	"INDEX.HTM",   // Adafruit and RP2040 bootloaders ship this alongside
	"CURRENT.UF2", // the readback file, present once a UF2 has been written
}

// looksLikeUF2Volume reports whether a mount point is a UF2 bootloader, and if
// not, why.
//
// Permission errors are distinguished from a plain absence: on recent macOS a
// terminal needs "Files and Folders → Removable Volumes" access before it can
// read a mounted bootloader at all, and without that the volume is present,
// visible in Finder, and completely invisible here.
func looksLikeUF2Volume(mount string) (bool, string) {
	var permDenied bool

	for _, marker := range bootloaderMarkers {
		st, err := os.Stat(filepath.Join(mount, marker))
		switch {
		case err == nil && !st.IsDir():
			return true, ""
		case os.IsPermission(err):
			permDenied = true
		}
	}

	// A readdir failure on the volume root is the clearest permission signal.
	if _, err := os.ReadDir(mount); err != nil {
		if os.IsPermission(err) {
			return false, "permission denied reading the volume — on macOS, grant your terminal " +
				"access under System Settings → Privacy & Security → Files and Folders → Removable Volumes"
		}
		return false, "cannot read the volume: " + err.Error()
	}

	if permDenied {
		return false, "permission denied reading the bootloader marker files — on macOS, grant your terminal " +
			"access under System Settings → Privacy & Security → Files and Folders → Removable Volumes"
	}
	return false, fmt.Sprintf("no bootloader marker (%s)", strings.Join(bootloaderMarkers, ", "))
}

// volumeLabel picks the best display name for a bootloader volume. The mount
// point's base name is the FAT label on macOS and Linux; on Windows it is only
// a drive letter, so the parsed Model is a better choice there.
func volumeLabel(mount string, info UF2Info) string {
	base := strings.Trim(filepath.Base(mount), `\/:`)
	if len(base) <= 2 && info.Model != "" {
		return info.Model
	}
	if base == "" {
		return info.Model
	}
	return base
}
