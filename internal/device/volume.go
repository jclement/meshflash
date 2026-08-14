package device

import (
	"path/filepath"
	"sort"
	"strings"
)

// ScanVolumes returns every mounted UF2 bootloader.
//
// The platform files supply candidate mount points; the shared logic here
// decides which of them are UF2 bootloaders by looking for INFO_UF2.TXT.
func ScanVolumes() ([]Volume, error) {
	mounts, err := mountCandidates()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []Volume
	for _, m := range mounts {
		clean := filepath.Clean(m)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		if !IsUF2Volume(clean) {
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
	return out, nil
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
