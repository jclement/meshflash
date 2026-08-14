// Package device discovers what is plugged in.
//
// Two very different things count as a connected device here. A serial port is
// how ESP32 boards and nRF52 boards in application mode appear. A mounted UF2
// volume is how nRF52 and RP2040 boards appear once they are in their
// bootloader. meshflash has to enumerate both and reconcile them, because the
// same physical board shows up as one or the other depending on its state.
package device

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
)

// Port is a discovered serial port.
type Port struct {
	Name         string `json:"name"`
	IsUSB        bool   `json:"is_usb"`
	VID          string `json:"vid,omitempty"`
	PID          string `json:"pid,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	Product      string `json:"product,omitempty"`
}

// USBID renders the vid:pid pair.
func (p Port) USBID() string {
	if p.VID == "" && p.PID == "" {
		return ""
	}
	return p.VID + ":" + p.PID
}

// Volume is a mounted UF2 bootloader presenting as mass storage.
type Volume struct {
	// Path is the mount point (/Volumes/RAK4631, E:\).
	Path string `json:"path"`
	// Label is the volume name, which for UF2 bootloaders is usually the
	// board name.
	Label string `json:"label"`
	// Info is the parsed INFO_UF2.TXT. Unlike a USB VID/PID, its Board-ID
	// identifies the actual board and not just the USB bridge.
	Info UF2Info `json:"info"`
}

// Detection is a snapshot of everything currently attached.
type Detection struct {
	Ports   []Port   `json:"ports"`
	Volumes []Volume `json:"volumes"`
}

// Detect enumerates serial ports and UF2 volumes.
//
// Errors from either half are returned but do not suppress the other: a
// permissions problem reading mount points should not hide the serial ports.
func Detect() (Detection, []error) {
	var errs []error

	ports, err := ListPorts()
	if err != nil {
		errs = append(errs, fmt.Errorf("enumerate serial ports: %w", err))
	}
	volumes, err := ScanVolumes()
	if err != nil {
		errs = append(errs, fmt.Errorf("scan UF2 volumes: %w", err))
	}

	return Detection{Ports: ports, Volumes: volumes}, errs
}

// Confidence describes how firmly a candidate device was identified.
type Confidence int

const (
	// ConfidencePossible means a USB bridge chip matched. These bridges
	// (CP2102, CH340, FTDI) are shared across dozens of unrelated boards, so
	// this narrows nothing on its own.
	ConfidencePossible Confidence = iota
	// ConfidenceLikely means the USB IDs matched exactly one catalog device,
	// or a native-USB chip pinned the family.
	ConfidenceLikely
	// ConfidenceExact means the board identified itself, via INFO_UF2.TXT's
	// Board-ID or an unambiguous volume label.
	ConfidenceExact
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceExact:
		return "exact"
	case ConfidenceLikely:
		return "likely"
	default:
		return "possible"
	}
}

// Candidate is one catalog device a detected target might be.
type Candidate struct {
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	Confidence Confidence `json:"confidence"`
	Reason     string     `json:"reason"`
}

// Target is something flashable: either a serial port or a UF2 volume, plus
// the catalog devices it could be.
type Target struct {
	// Exactly one of Port or Volume is set.
	Port   *Port   `json:"port,omitempty"`
	Volume *Volume `json:"volume,omitempty"`

	// Candidates is ordered best-first.
	Candidates []Candidate `json:"candidates"`

	// ChipHint is what the transport itself reported, e.g. "ESP32-S3" after a
	// bootloader handshake. Filled in by the flash layer, not detection.
	ChipHint string `json:"chip_hint,omitempty"`
}

// Describe renders a one-line label for lists and logs.
func (t Target) Describe() string {
	switch {
	case t.Volume != nil:
		label := t.Volume.Label
		if label == "" {
			label = t.Volume.Path
		}
		return fmt.Sprintf("%s (UF2 bootloader at %s)", label, t.Volume.Path)
	case t.Port != nil:
		if id := t.Port.USBID(); id != "" {
			return fmt.Sprintf("%s (USB %s)", t.Port.Name, id)
		}
		return t.Port.Name
	default:
		return "unknown target"
	}
}

// Address is the stable identifier used on the command line.
func (t Target) Address() string {
	switch {
	case t.Volume != nil:
		return t.Volume.Path
	case t.Port != nil:
		return t.Port.Name
	default:
		return ""
	}
}

// InBootloader reports whether the target is already in a UF2 bootloader, in
// which case no auto-DFU touch is needed.
func (t Target) InBootloader() bool { return t.Volume != nil }

// BestCandidate returns the highest-confidence match, if any.
func (t Target) BestCandidate() (Candidate, bool) {
	if len(t.Candidates) == 0 {
		return Candidate{}, false
	}
	return t.Candidates[0], true
}

// Resolved reports whether identification is confident enough to flash without
// asking the operator to choose. Only a board that named itself qualifies.
func (t Target) Resolved() bool {
	c, ok := t.BestCandidate()
	if !ok || c.Confidence != ConfidenceExact {
		return false
	}
	// Even an exact match is ambiguous if a second device matched equally.
	return len(t.Candidates) == 1 || t.Candidates[1].Confidence < ConfidenceExact
}

// Identify pairs each detected port and volume with catalog devices.
func Identify(d Detection, cat *catalog.Catalog) []Target {
	var targets []Target

	for i := range d.Volumes {
		v := d.Volumes[i]
		targets = append(targets, Target{
			Volume:     &v,
			Candidates: matchVolume(v, cat),
		})
	}
	for i := range d.Ports {
		p := d.Ports[i]
		targets = append(targets, Target{
			Port:       &p,
			Candidates: matchPort(p, cat),
		})
	}

	// Bootloader volumes first: they are both unambiguous and ready to flash.
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].Volume != nil && targets[j].Volume == nil
	})
	return targets
}

// matchVolume identifies a board from its UF2 metadata. Board-ID is the
// authoritative signal; the volume label is a decent fallback.
func matchVolume(v Volume, cat *catalog.Catalog) []Candidate {
	if cat == nil {
		return nil
	}
	var out []Candidate
	boardID := strings.TrimSpace(v.Info.BoardID)
	label := strings.TrimSpace(v.Label)

	for _, d := range cat.Devices {
		switch {
		case boardID != "" && containsFold(d.UF2Board, boardID):
			out = append(out, Candidate{
				DeviceID:   d.ID,
				Name:       d.Name,
				Confidence: ConfidenceExact,
				Reason:     "INFO_UF2.TXT Board-ID " + boardID,
			})
		case label != "" && containsFold(d.UF2Volume, label):
			out = append(out, Candidate{
				DeviceID:   d.ID,
				Name:       d.Name,
				Confidence: ConfidenceExact,
				Reason:     "volume label " + label,
			})
		}
	}

	sortCandidates(out)
	return out
}

// containsFold reports whether want matches any entry, ignoring case and
// surrounding whitespace.
func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}

// matchPort identifies a board from USB IDs.
//
// This is inherently weak. Most LoRa boards expose a generic USB-UART bridge
// whose VID/PID says nothing about the board behind it, so the usual outcome
// is a long "possible" list that the operator has to narrow by hand. Boards
// with native USB (ESP32-S3, nRF52840) at least pin the chip family.
func matchPort(p Port, cat *catalog.Catalog) []Candidate {
	if cat == nil || !p.IsUSB {
		return nil
	}
	id := catalog.USBID{VID: p.VID, PID: p.PID}.Normalize()
	if id.VID == "0000" && id.PID == "0000" {
		return nil
	}

	shared := IsSharedBridge(id)

	var out []Candidate
	for _, d := range cat.Devices {
		for _, u := range d.USB {
			if u.Normalize() != id {
				continue
			}
			conf := ConfidenceLikely
			reason := "USB ID " + id.String()
			if shared {
				conf = ConfidencePossible
				reason = fmt.Sprintf("USB ID %s (%s, shared across many boards)", id.String(), BridgeName(id))
			}
			out = append(out, Candidate{
				DeviceID:   d.ID,
				Name:       d.Name,
				Confidence: conf,
				Reason:     reason,
			})
			break
		}
	}

	// A single non-shared match is as good as this signal gets.
	if len(out) > 1 {
		for i := range out {
			if out[i].Confidence > ConfidencePossible {
				out[i].Confidence = ConfidencePossible
				out[i].Reason += " (matches multiple boards)"
			}
		}
	}

	sortCandidates(out)
	return out
}

func sortCandidates(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Confidence != c[j].Confidence {
			return c[i].Confidence > c[j].Confidence
		}
		return c[i].DeviceID < c[j].DeviceID
	})
}
