//go:build windows

package device

import (
	"golang.org/x/sys/windows"
)

// mountCandidates lists drive letters that hold removable media on Windows.
//
// UF2 bootloaders always appear as their own drive letter. Filtering to
// removable drives up front keeps meshflash from stat-ing network shares,
// which can block for seconds when a mapped drive is unreachable.
func mountCandidates() ([]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}

	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`

		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		switch windows.GetDriveType(ptr) {
		case windows.DRIVE_REMOVABLE, windows.DRIVE_FIXED:
			// Some UF2 bootloaders report as fixed rather than removable, so
			// both are probed; the INFO_UF2.TXT check filters the rest.
			out = append(out, root)
		}
	}
	return out, nil
}
