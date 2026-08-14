package nrfdfu

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

// maxMemberBytes caps any single file read out of a DFU zip. Real images are
// well under a megabyte; this bounds a hostile or corrupt archive.
const maxMemberBytes = 8 << 20

// Package is a parsed Nordic legacy DFU archive: a manifest.json plus one or
// more (bin, dat) image pairs.
type Package struct {
	DFUVersion float64
	Images     []Image
}

// Image is a single programmable component of a package.
type Image struct {
	// Mode is the DFU update mode / HexType for this image.
	Mode uint32
	// Name is a human label ("application", "bootloader", ...).
	Name string
	// Firmware is the raw .bin payload.
	Firmware []byte
	// InitPacket is the .dat blob, forwarded to the device verbatim. It
	// encodes device type, revision, SoftDevice requirement and the firmware
	// CRC-16 the bootloader checks after transfer.
	InitPacket []byte

	// SoftDeviceSize and BootloaderSize are only meaningful for a combined
	// SoftDevice+bootloader image, where the start packet must describe the
	// split rather than a single blob.
	SoftDeviceSize uint32
	BootloaderSize uint32
}

// manifestFile mirrors the on-disk manifest.json schema.
type manifestFile struct {
	Manifest struct {
		Application          *firmwareEntry `json:"application"`
		Bootloader           *firmwareEntry `json:"bootloader"`
		SoftDevice           *firmwareEntry `json:"softdevice"`
		SoftDeviceBootloader *sdblEntry     `json:"softdevice_bootloader"`
		DFUVersion           float64        `json:"dfu_version"`
	} `json:"manifest"`
}

type firmwareEntry struct {
	BinFile string `json:"bin_file"`
	DatFile string `json:"dat_file"`
	// init_packet_data is descriptive only; the .dat file is authoritative.
	InitPacketData json.RawMessage `json:"init_packet_data,omitempty"`
}

type sdblEntry struct {
	firmwareEntry
	SDSize uint32 `json:"sd_size"`
	BLSize uint32 `json:"bl_size"`
}

// OpenPackage parses a DFU archive from a byte slice, which is how meshflash
// holds it after pulling from the firmware cache.
func OpenPackage(data []byte) (*Package, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open DFU package: %w", err)
	}

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		// Package layouts occasionally nest under a directory; index by base
		// name so manifest references resolve either way.
		files[path.Base(f.Name)] = f
	}

	mf, ok := files["manifest.json"]
	if !ok {
		return nil, fmt.Errorf("DFU package has no manifest.json")
	}
	raw, err := readMember(mf)
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}

	var m manifestFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}

	pkg := &Package{DFUVersion: m.Manifest.DFUVersion}

	// This transport implements the legacy protocol only. Nordic's Secure DFU
	// (dfu_version >= 0.8, used by stock nRF5 SDK bootloaders) is a different
	// wire format; refusing loudly beats corrupting a device.
	if pkg.DFUVersion != 0 && pkg.DFUVersion > 0.5 {
		return nil, fmt.Errorf("DFU package declares version %.1f; meshflash implements the legacy 0.5 protocol used by the Adafruit nRF52 bootloader", pkg.DFUVersion)
	}

	add := func(name string, mode uint32, e *firmwareEntry, sdSize, blSize uint32) error {
		if e == nil {
			return nil
		}
		bin, err := readNamed(files, e.BinFile)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		dat, err := readNamed(files, e.DatFile)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		pkg.Images = append(pkg.Images, Image{
			Mode:           mode,
			Name:           name,
			Firmware:       bin,
			InitPacket:     dat,
			SoftDeviceSize: sdSize,
			BootloaderSize: blSize,
		})
		return nil
	}

	// Order matters: the bootloader expects SoftDevice/bootloader work before
	// the application, matching Adafruit's dfu_send_images.
	if e := m.Manifest.SoftDeviceBootloader; e != nil {
		if err := add("softdevice_bootloader", ModeSoftDevice|ModeBootloader, &e.firmwareEntry, e.SDSize, e.BLSize); err != nil {
			return nil, err
		}
	}
	if err := add("softdevice", ModeSoftDevice, m.Manifest.SoftDevice, 0, 0); err != nil {
		return nil, err
	}
	if err := add("bootloader", ModeBootloader, m.Manifest.Bootloader, 0, 0); err != nil {
		return nil, err
	}
	if err := add("application", ModeApplication, m.Manifest.Application, 0, 0); err != nil {
		return nil, err
	}

	if len(pkg.Images) == 0 {
		return nil, fmt.Errorf("DFU package manifest lists no images")
	}

	if e := m.Manifest.SoftDeviceBootloader; e != nil {
		img := &pkg.Images[0]
		if int(img.SoftDeviceSize)+int(img.BootloaderSize) != len(img.Firmware) {
			return nil, fmt.Errorf("softdevice_bootloader: sd_size %d + bl_size %d does not match image length %d",
				img.SoftDeviceSize, img.BootloaderSize, len(img.Firmware))
		}
	}

	return pkg, nil
}

// TotalBytes is the sum of all image payloads, used for progress reporting.
func (p *Package) TotalBytes() int {
	n := 0
	for _, img := range p.Images {
		n += len(img.Firmware)
	}
	return n
}

// VerifyChecksums recomputes the CRC-16 embedded in each init packet and
// compares it to the firmware. A legacy init packet ends with that CRC, so a
// truncated download is caught here rather than by a bricked device.
func (p *Package) VerifyChecksums() error {
	for _, img := range p.Images {
		if len(img.InitPacket) < 2 {
			continue // nothing to check against
		}
		want := uint16(img.InitPacket[len(img.InitPacket)-2]) |
			uint16(img.InitPacket[len(img.InitPacket)-1])<<8
		if got := CRC16(img.Firmware); got != want {
			return fmt.Errorf("%s: firmware CRC-16 is 0x%04X but the init packet expects 0x%04X (corrupt or truncated download)",
				img.Name, got, want)
		}
	}
	return nil
}

func readNamed(files map[string]*zip.File, name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("manifest omits a file name")
	}
	f, ok := files[path.Base(name)]
	if !ok {
		return nil, fmt.Errorf("package is missing %q", name)
	}
	return readMember(f)
}

func readMember(f *zip.File) ([]byte, error) {
	if f.UncompressedSize64 > maxMemberBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit", f.Name, f.UncompressedSize64, maxMemberBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxMemberBytes+1))
}

// Describe renders a one-line summary for logs.
func (p *Package) Describe() string {
	parts := make([]string, 0, len(p.Images))
	for _, img := range p.Images {
		parts = append(parts, fmt.Sprintf("%s %d bytes", img.Name, len(img.Firmware)))
	}
	return strings.Join(parts, ", ")
}
