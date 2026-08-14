//go:build darwin

package device

import (
	"os"
	"path/filepath"
)

// mountCandidates lists removable volumes on macOS.
//
// diskarbitrationd mounts every removable filesystem under /Volumes, so a
// directory listing is sufficient and avoids pulling in DiskArbitration via
// cgo just to learn what a readdir already tells us.
func mountCandidates() ([]string, error) {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []string
	for _, e := range entries {
		// UF2 bootloaders mount as directories; symlinks under /Volumes point
		// at the boot volume and are not interesting.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		out = append(out, filepath.Join("/Volumes", e.Name()))
	}
	return out, nil
}
