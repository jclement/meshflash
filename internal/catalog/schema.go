// Package catalog defines the normalised firmware index that meshflash consumes.
//
// The design goal is that meshflash never scrapes upstream at flash time. A
// scheduled job (tools/catalog-gen) resolves Meshtastic and MeshCore releases
// into this single schema and publishes it as catalog.json. Clients fetch that
// artifact while online and then run entirely from disk. When upstream renames
// an asset, the generator breaks — not the offline Toughbook in a field.
package catalog

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is bumped when the on-disk shape changes incompatibly.
// meshflash refuses a catalog whose version it does not understand and tells
// the operator to upgrade rather than guessing at unknown fields.
const SchemaVersion = 1

// Method is how a build reaches the device.
type Method string

const (
	// MethodESP32 writes one or more raw images at fixed flash offsets over
	// the ESP ROM/stub serial bootloader.
	MethodESP32 Method = "esp32"
	// MethodUF2 copies a .uf2 file onto the mass-storage volume exposed by a
	// UF2 bootloader (Adafruit nRF52, RP2040 BOOTSEL).
	MethodUF2 Method = "uf2"
	// MethodNRFDFU pushes a legacy Nordic DFU .zip over serial to the
	// Adafruit nRF52 bootloader.
	MethodNRFDFU Method = "nrf-dfu"
)

func (m Method) Valid() bool {
	switch m {
	case MethodESP32, MethodUF2, MethodNRFDFU:
		return true
	}
	return false
}

// Role labels what an artifact contributes to a flash.
type Role string

const (
	RoleApp        Role = "app"        // application image
	RoleBootloader Role = "bootloader" // second-stage bootloader
	RolePartitions Role = "partitions" // partition table
	RoleBootApp0   Role = "boot_app0"  // OTA selector
	RoleMerged     Role = "merged"     // single image covering the whole flash
	RoleLittleFS   Role = "littlefs"   // filesystem image (web UI assets)
	RoleUF2        Role = "uf2"        // UF2 payload
	RolePackage    Role = "package"    // Nordic DFU .zip
)

// Catalog is the whole index.
type Catalog struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Generator     string    `json:"generator"`

	// Devices is the shared hardware registry. Build.DeviceID points here so
	// USB hints and display names are defined exactly once.
	Devices []Device `json:"devices"`

	Projects []Project `json:"projects"`
}

// Device is one physical board.
type Device struct {
	ID       string `json:"id"`   // canonical slug, e.g. "rak4631"
	Name     string `json:"name"` // "RAK WisBlock 4631"
	Vendor   string `json:"vendor,omitempty"`
	Platform string `json:"platform"` // esp32, esp32s3, nrf52840, rp2040, ...

	// USB lists VID/PID pairs seen on this board. These usually identify the
	// USB-serial bridge (CP2102, CH340) rather than the board itself, so they
	// narrow the candidate list but rarely resolve it. Treat as a hint.
	USB []USBID `json:"usb,omitempty"`

	// UF2Board matches the "Board-ID" line in INFO_UF2.TXT on a mounted UF2
	// bootloader. Unlike USB IDs this is authoritative when present.
	UF2Board []string `json:"uf2_board,omitempty"`

	// UF2Volume matches the mounted volume label (e.g. "RAK4631"), used when
	// INFO_UF2.TXT is missing or unreadable.
	UF2Volume []string `json:"uf2_volume,omitempty"`

	// USBProduct matches the USB product string the board reports while
	// running its application. Unlike a VID/PID this is board-specific — a
	// Heltec T114 reports "HT-n5262", the same string its bootloader puts in
	// INFO_UF2.TXT — so it identifies the model without the board having to be
	// in its bootloader at all.
	USBProduct []string `json:"usb_product,omitempty"`

	Notes string `json:"notes,omitempty"`
}

// USBID is a USB vendor/product pair, stored as 4-digit lowercase hex.
type USBID struct {
	VID string `json:"vid"`
	PID string `json:"pid"`
}

func (u USBID) String() string { return u.VID + ":" + u.PID }

// Normalize lowercases and zero-pads so comparisons against enumerated ports
// are stable regardless of how the OS reports them.
func (u USBID) Normalize() USBID {
	return USBID{VID: normHex(u.VID), PID: normHex(u.PID)}
}

func normHex(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// Project is an upstream firmware source (Meshtastic or MeshCore).
type Project struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Homepage string    `json:"homepage,omitempty"`
	Repo     string    `json:"repo"` // "owner/name" on GitHub
	Releases []Release `json:"releases"`
}

// Release is one upstream version.
type Release struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	Channel     string    `json:"channel"` // stable | beta | alpha
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes,omitempty"`
	Builds      []Build   `json:"builds"`
}

// Build is one flashable firmware for one device in one release.
type Build struct {
	DeviceID string `json:"device_id"`
	Method   Method `json:"method"`

	// Variant distinguishes builds for the same board within a release, e.g.
	// MeshCore's companion_radio_ble vs companion_radio_usb, or Meshtastic's
	// -inkhud suffix. Empty means the project ships exactly one build.
	Variant string `json:"variant,omitempty"`

	Artifacts []Artifact `json:"artifacts"`
}

// Label is a human-facing name for the build variant.
func (b Build) Label() string {
	if b.Variant == "" {
		return string(b.Method)
	}
	return b.Variant
}

// Artifact is one downloadable (or in-archive) file.
type Artifact struct {
	Role Role   `json:"role"`
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`

	// SHA256 is verified after download when present. The generator fills it
	// for small files; large platform zips may omit it to keep generation fast.
	SHA256 string `json:"sha256,omitempty"`

	// URL is the direct download. For files that live inside a release zip,
	// URL is empty and Archive/ArchivePath are set instead — this is how a
	// single 170 MB esp32s3 zip serves dozens of boards without re-hosting.
	URL string `json:"url,omitempty"`

	Archive     string `json:"archive,omitempty"`
	ArchivePath string `json:"archive_path,omitempty"`

	// ArchiveSize is the compressed size of the containing archive. Size
	// describes the extracted file, which is far smaller — a 1.5 MB firmware
	// can live inside a 170 MB platform zip. Download estimates must use this
	// or they understate the transfer by two orders of magnitude.
	ArchiveSize int64 `json:"archive_size,omitempty"`

	// Offset is the flash address for MethodESP32 artifacts.
	Offset *uint32 `json:"offset,omitempty"`
}

// Source returns the URL that must be fetched to obtain this artifact, which
// is the containing archive when the artifact is packed.
func (a Artifact) Source() string {
	if a.Archive != "" {
		return a.Archive
	}
	return a.URL
}

// Packed reports whether the artifact must be extracted from an archive.
func (a Artifact) Packed() bool { return a.Archive != "" }

// DownloadSize is how many bytes fetching this artifact actually transfers.
func (a Artifact) DownloadSize() int64 {
	if a.Packed() {
		return a.ArchiveSize
	}
	return a.Size
}

// --- lookup helpers -------------------------------------------------------

// DeviceByID returns the device registry entry.
func (c *Catalog) DeviceByID(id string) (Device, bool) {
	for _, d := range c.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}

// ProjectByID returns a project.
func (c *Catalog) ProjectByID(id string) (*Project, bool) {
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			return &c.Projects[i], true
		}
	}
	return nil, false
}

// ReleaseByVersion finds a release within a project.
func (p *Project) ReleaseByVersion(v string) (*Release, bool) {
	for i := range p.Releases {
		if p.Releases[i].Version == v || p.Releases[i].Tag == v {
			return &p.Releases[i], true
		}
	}
	return nil, false
}

// LatestStable returns the newest stable release, falling back to the newest
// release of any channel when the project has published no stable build.
func (p *Project) LatestStable() (*Release, bool) {
	var best *Release
	for i := range p.Releases {
		r := &p.Releases[i]
		if r.Channel != "stable" {
			continue
		}
		if best == nil || r.PublishedAt.After(best.PublishedAt) {
			best = r
		}
	}
	if best != nil {
		return best, true
	}
	for i := range p.Releases {
		r := &p.Releases[i]
		if best == nil || r.PublishedAt.After(best.PublishedAt) {
			best = r
		}
	}
	return best, best != nil
}

// BuildsForDevice returns every build in the release targeting a device.
func (r *Release) BuildsForDevice(deviceID string) []Build {
	var out []Build
	for _, b := range r.Builds {
		if b.DeviceID == deviceID {
			out = append(out, b)
		}
	}
	return out
}

// DeviceIDs lists every device the catalog can flash, sorted.
func (c *Catalog) DeviceIDs() []string {
	seen := map[string]bool{}
	for _, p := range c.Projects {
		for _, r := range p.Releases {
			for _, b := range r.Builds {
				seen[b.DeviceID] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Validate checks structural invariants that would otherwise surface as a
// confusing failure halfway through a flash.
func (c *Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("catalog schema version %d is not supported (this build understands %d); run `meshflash upgrade`",
			c.SchemaVersion, SchemaVersion)
	}
	devices := map[string]bool{}
	for _, d := range c.Devices {
		if d.ID == "" {
			return fmt.Errorf("device with empty id")
		}
		if devices[d.ID] {
			return fmt.Errorf("duplicate device id %q", d.ID)
		}
		devices[d.ID] = true
	}
	for _, p := range c.Projects {
		if p.ID == "" {
			return fmt.Errorf("project with empty id")
		}
		for _, r := range p.Releases {
			for _, b := range r.Builds {
				where := fmt.Sprintf("%s/%s/%s", p.ID, r.Version, b.DeviceID)
				if !devices[b.DeviceID] {
					return fmt.Errorf("%s: references unknown device %q", where, b.DeviceID)
				}
				if !b.Method.Valid() {
					return fmt.Errorf("%s: unknown flash method %q", where, b.Method)
				}
				if len(b.Artifacts) == 0 {
					return fmt.Errorf("%s: build has no artifacts", where)
				}
				for _, a := range b.Artifacts {
					if a.Source() == "" {
						return fmt.Errorf("%s: artifact %q has neither url nor archive", where, a.Name)
					}
					if a.Packed() && a.ArchivePath == "" {
						return fmt.Errorf("%s: artifact %q sets archive without archive_path", where, a.Name)
					}
					if b.Method == MethodESP32 && a.Offset == nil {
						return fmt.Errorf("%s: esp32 artifact %q has no flash offset", where, a.Name)
					}
				}
			}
		}
	}
	return nil
}
