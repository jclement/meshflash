package device

import (
	"sort"
	"strings"

	"go.bug.st/serial/enumerator"
)

// ListPorts enumerates serial ports with USB metadata where the OS exposes it.
//
// On macOS this path goes through IOKit and therefore requires cgo, which is
// why release builds for darwin are produced on macOS runners rather than
// cross-compiled.
func ListPorts() ([]Port, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}

	out := make([]Port, 0, len(details))
	for _, d := range details {
		if d == nil || isNoise(d.Name) {
			continue
		}
		// Every flashable board is a USB device. Skipping the rest removes
		// the standing clutter of Bluetooth serial profiles — paired speakers
		// and headsets — which would otherwise appear as targets forever.
		if !d.IsUSB {
			continue
		}
		out = append(out, Port{
			Name:         d.Name,
			IsUSB:        d.IsUSB,
			VID:          strings.ToLower(d.VID),
			PID:          strings.ToLower(d.PID),
			SerialNumber: d.SerialNumber,
			Product:      strings.TrimSpace(d.Product),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		// USB ports first — those are the ones anyone wants to flash.
		if out[i].IsUSB != out[j].IsUSB {
			return out[i].IsUSB
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// isNoise filters ports that are never a flashable device but always present,
// so `doctor` output stays readable.
func isNoise(name string) bool {
	base := strings.ToLower(name)
	for _, s := range []string{
		"/dev/cu.bluetooth",     // macOS Bluetooth serial
		"/dev/tty.bluetooth",    //
		"/dev/cu.debug-console", // macOS debug console
		"/dev/tty.debug-console",
		"/dev/cu.wlan-debug",
		"/dev/tty.wlan-debug",
	} {
		if strings.HasPrefix(base, s) {
			return true
		}
	}
	return false
}
