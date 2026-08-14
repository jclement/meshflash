//go:build linux

package device

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// mountCandidates lists FAT mounts on Linux.
//
// /proc/mounts is authoritative and covers every automounter layout, so it is
// the primary source. The glob fallbacks catch the case where a volume is
// mounted somewhere /proc reports with an unusual fstype.
func mountCandidates() ([]string, error) {
	var out []string
	out = append(out, fatMountsFromProc()...)
	out = append(out, autoMountGlobs()...)
	return out, nil
}

// fatMountsFromProc parses /proc/mounts for vfat/msdos filesystems, which is
// what every UF2 bootloader presents.
func fatMountsFromProc() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		switch fields[2] {
		case "vfat", "msdos", "exfat":
			// Mount points are octal-escaped in /proc/mounts (a space is \040).
			out = append(out, unescapeMountPath(fields[1]))
		}
	}
	return out
}

// autoMountGlobs covers the conventional desktop automount locations.
func autoMountGlobs() []string {
	patterns := []string{
		"/media/*",
		"/media/*/*",
		"/run/media/*/*",
		"/mnt/*",
	}
	var out []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	return out
}

// unescapeMountPath decodes the octal escapes /proc/mounts uses for space,
// tab, newline and backslash. Board names like "RAK 4631" hit this.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		var v int
		valid := true
		for _, c := range s[i+1 : i+4] {
			if c < '0' || c > '7' {
				valid = false
				break
			}
			v = v*8 + int(c-'0')
		}
		if !valid || v > 0xFF {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}
