// Package fingerprint derives a stable per-board identity.
//
// Board *model* detection is unreliable by nature: most LoRa boards expose a
// generic USB-UART bridge whose VID/PID says nothing about the board behind
// it. Board *instance* identity is a different and much easier problem, and it
// is the one that actually makes a field workflow pleasant — if meshflash
// remembers what it put on this exact board last time, it can do the right
// thing again without anyone picking from a list.
//
// Two sources are used, strongest first:
//
//   - The ESP32 eFuse base MAC. Globally unique, burned at the factory,
//     readable from the ROM bootloader before writing anything, and unchanged
//     by a full chip erase. Costs one bootloader handshake to read.
//   - The USB iSerialNumber, when the device publishes a real one. Native-USB
//     parts (nRF52840, ESP32-S3, RP2040) derive it from a chip ID, so it is
//     unique and free to read during enumeration.
package fingerprint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/device"
	"tinygo.org/x/espflasher/pkg/espflasher"
)

// Kind identifies where a fingerprint came from.
type Kind string

const (
	// KindESPMAC is an ESP32 eFuse base MAC address.
	KindESPMAC Kind = "esp-mac"
	// KindUSBSerial is a USB iSerialNumber.
	KindUSBSerial Kind = "usb"
)

// Fingerprint identifies one physical board.
type Fingerprint struct {
	Kind  Kind   `json:"kind"`
	Value string `json:"value"`
}

// Key is the canonical string form, used as a map and JSON key.
func (f Fingerprint) Key() string {
	if f.Kind == "" || f.Value == "" {
		return ""
	}
	return string(f.Kind) + ":" + f.Value
}

// Valid reports whether the fingerprint is usable.
func (f Fingerprint) Valid() bool { return f.Key() != "" }

func (f Fingerprint) String() string {
	if !f.Valid() {
		return "(no stable fingerprint)"
	}
	return f.Key()
}

// ParseKey reverses Key.
func ParseKey(s string) (Fingerprint, bool) {
	kind, value, ok := strings.Cut(s, ":")
	if !ok || kind == "" || value == "" {
		return Fingerprint{}, false
	}
	return Fingerprint{Kind: Kind(kind), Value: value}, true
}

// genericSerials are iSerialNumber values that USB bridges ship with
// unprogrammed. They are identical across every board using that chip, so
// treating one as an identity would make unrelated boards look like the same
// device — the one failure mode that could flash the wrong firmware.
var genericSerials = map[string]bool{
	"":                 true,
	"0":                true,
	"1":                true,
	"0001":             true,
	"00":               true,
	"000000000000":     true,
	"0000000000000000": true,
	"123456":           true,
	"12345678":         true,
	"0123456789":       true,
	"ffffffffffff":     true,
	"serial":           true,
	"none":             true,
	"n/a":              true,
}

// minSerialLength rejects short serials, which are almost always a counter
// rather than a chip ID.
const minSerialLength = 6

// FromPort derives a free fingerprint from an enumerated serial port.
//
// Returns an invalid fingerprint when the port publishes no trustworthy
// serial, which is the normal outcome for CH340 boards — those need an eFuse
// MAC probe instead.
func FromPort(p device.Port) Fingerprint {
	s := strings.ToLower(strings.TrimSpace(p.SerialNumber))
	if !usableSerial(s) {
		return Fingerprint{}
	}
	return Fingerprint{Kind: KindUSBSerial, Value: s}
}

// FromVolume derives a fingerprint from a mounted UF2 bootloader.
//
// INFO_UF2.TXT has no per-unit field — Board-ID names the model — so this only
// succeeds for bootloaders that also expose a serial number, which is picked
// up from the matching serial port rather than the volume itself. Kept as a
// separate entry point so callers do not have to special-case volumes.
func FromVolume(v device.Volume) Fingerprint {
	// Some bootloaders write a unique device address into the info file under
	// a vendor-specific key. Use it when present.
	for _, key := range []string{"Device-ID", "Serial", "UniqueID", "Chip-ID"} {
		if val, ok := v.Info.Fields[key]; ok {
			s := strings.ToLower(strings.TrimSpace(val))
			if usableSerial(s) {
				return Fingerprint{Kind: KindUSBSerial, Value: s}
			}
		}
	}
	return Fingerprint{}
}

// FromTarget derives whatever fingerprint is available without touching the
// device.
func FromTarget(t device.Target) Fingerprint {
	if t.Port != nil {
		if fp := FromPort(*t.Port); fp.Valid() {
			return fp
		}
	}
	if t.Volume != nil {
		if fp := FromVolume(*t.Volume); fp.Valid() {
			return fp
		}
	}
	return Fingerprint{}
}

func usableSerial(s string) bool {
	if len(s) < minSerialLength || genericSerials[s] {
		return false
	}
	// All-identical characters ("000000", "ffffff") are placeholders.
	first := s[0]
	same := true
	for i := 0; i < len(s); i++ {
		if s[i] != first {
			same = false
			break
		}
	}
	return !same
}

// ProbeESP32 reads the eFuse base MAC over the ROM bootloader.
//
// This is the strongest identity available for ESP32 boards and the only one
// for boards behind a CH340. It costs a bootloader handshake, which resets the
// device and takes a couple of seconds, so callers should treat it as opt-in
// rather than doing it during routine enumeration.
func ProbeESP32(ctx context.Context, portName string) (Fingerprint, string, error) {
	opts := espflasher.DefaultOptions()
	opts.ResetMode = espflasher.ResetAuto
	// The MAC comes from READ_REG, which the ROM implements directly. Skipping
	// the stub upload makes this markedly faster and leaves no resident stub
	// to confuse a subsequent connect.
	opts.SkipStub = true
	opts.ConnectAttempts = 3

	done := make(chan struct{})
	var (
		f   *espflasher.Flasher
		err error
	)
	go func() {
		defer close(done)
		f, err = espflasher.New(portName, opts)
	}()

	select {
	case <-ctx.Done():
		return Fingerprint{}, "", ctx.Err()
	case <-done:
	}
	if err != nil {
		return Fingerprint{}, "", fmt.Errorf("probe %s: %w", portName, err)
	}
	defer f.Close()

	chip := f.ChipName()
	mac, err := f.MAC()
	if err != nil {
		return Fingerprint{}, chip, fmt.Errorf("read MAC from %s: %w", portName, err)
	}

	// Normalise to bare lowercase hex so the key is stable regardless of how
	// the MAC is formatted for display.
	value := strings.ToLower(strings.ReplaceAll(mac.String(), ":", ""))
	if value == "" || value == "000000000000" {
		return Fingerprint{}, chip, fmt.Errorf("device reported an empty MAC")
	}

	// Leave the chip running the application it had.
	f.Reset()
	return Fingerprint{Kind: KindESPMAC, Value: value}, chip, nil
}

// ProbeTimeout bounds a single probe so scanning several ports cannot hang.
const ProbeTimeout = 20 * time.Second
