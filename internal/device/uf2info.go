package device

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// InfoFileName is the metadata file every UF2 bootloader exposes at the root
// of its mass-storage volume.
const InfoFileName = "INFO_UF2.TXT"

// maxInfoBytes bounds the read. The real file is a few hundred bytes.
const maxInfoBytes = 8 << 10

// UF2Info is the parsed contents of INFO_UF2.TXT.
//
// Board-ID is the field worth caring about: it names the actual board, which
// no USB VID/PID on these devices does.
type UF2Info struct {
	Model      string            `json:"model,omitempty"`
	BoardID    string            `json:"board_id,omitempty"`
	SoftDevice string            `json:"softdevice,omitempty"`
	Bootloader string            `json:"bootloader,omitempty"`
	Date       string            `json:"date,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

// Empty reports whether nothing was parsed.
func (i UF2Info) Empty() bool { return len(i.Fields) == 0 }

// ReadUF2Info parses INFO_UF2.TXT from a mounted volume. A missing file is not
// an error — it just means this volume is not a UF2 bootloader.
func ReadUF2Info(mount string) (UF2Info, bool) {
	f, err := os.Open(filepath.Join(mount, InfoFileName))
	if err != nil {
		return UF2Info{}, false
	}
	defer f.Close()

	info := UF2Info{Fields: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxInfoBytes)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// The first line is a banner ("UF2 Bootloader 0.6.2 …") with no colon.
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		info.Fields[key] = value

		switch strings.ToLower(key) {
		case "model":
			info.Model = value
		case "board-id":
			info.BoardID = value
		case "softdevice":
			info.SoftDevice = value
		case "bootloader":
			info.Bootloader = value
		case "date":
			info.Date = value
		}
	}
	if err := sc.Err(); err != nil {
		return info, len(info.Fields) > 0
	}
	return info, true
}

// IsUF2Volume reports whether a mount point looks like a UF2 bootloader.
func IsUF2Volume(mount string) bool {
	st, err := os.Stat(filepath.Join(mount, InfoFileName))
	return err == nil && !st.IsDir()
}
