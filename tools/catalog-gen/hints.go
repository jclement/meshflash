package main

import (
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
)

// USB and UF2 identification hints.
//
// Neither upstream publishes USB IDs or UF2 board names, so this table is
// meshflash's own. It is deliberately conservative: a wrong hint would make
// meshflash confidently offer the wrong firmware, which is worse than asking.
//
// USB IDs here are mostly bridge chips shared across many boards, so they only
// ever produce a "possible" match. The UF2 board IDs are the valuable half —
// those come from the board's own INFO_UF2.TXT and identify it exactly.

// uf2BoardHints maps a catalog device id substring to the Board-ID strings its
// bootloader publishes.
var uf2BoardHints = map[string][]string{
	"rak4631":      {"nrf52840-rak4631", "WisCore RAK4631 Board"},
	"rak11310":     {"rp2040-rak11310"},
	"t114":         {"nrf52840-heltec-t114", "heltec_t114"},
	"tracker-t114": {"nrf52840-heltec-t114"},
	"promicro":     {"nrf52840-promicro", "Nice!Nano"},
	"xiao-ble":     {"nrf52840-xiao-ble", "Seeed XIAO nRF52840"},
	"wio-tracker":  {"nrf52840-wio-tracker"},
	"tbeam":        {},
	"pico":         {"rp2040-pico", "RPI-RP2"},
	"picow":        {"rp2040-picow", "RPI-RP2"},
}

// uf2VolumeHints maps a device id substring to mass-storage volume labels.
var uf2VolumeHints = map[string][]string{
	"rak4631":     {"RAK4631", "WISCORE"},
	"rak11310":    {"RPI-RP2"},
	"t114":        {"T114BOOT", "HELTECBOOT"},
	"promicro":    {"NICENANO"},
	"xiao-ble":    {"XIAO-SENSE", "XIAO BOOT"},
	"wio-tracker": {"WIOTRACKER"},
	"pico":        {"RPI-RP2"},
	"picow":       {"RPI-RP2"},
}

// usbHints maps a device id substring to USB IDs. These narrow the candidate
// list; they almost never resolve it on their own.
var usbHints = map[string][]catalog.USBID{
	// Native-USB nRF52840 boards, in application and bootloader mode.
	"rak4631":     {{VID: "239a", PID: "8029"}, {VID: "239a", PID: "0029"}},
	"t114":        {{VID: "239a", PID: "8029"}, {VID: "239a", PID: "0029"}},
	"promicro":    {{VID: "239a", PID: "8029"}, {VID: "239a", PID: "0029"}},
	"xiao-ble":    {{VID: "2886", PID: "0044"}, {VID: "2886", PID: "8044"}},
	"wio-tracker": {{VID: "2886", PID: "0059"}},

	// RP2040 in BOOTSEL and running.
	"rak11310": {{VID: "2e8a", PID: "0003"}, {VID: "2e8a", PID: "000a"}},
	"pico":     {{VID: "2e8a", PID: "0003"}, {VID: "2e8a", PID: "000a"}},

	// ESP32-S3 native USB.
	"tracker-v1":    {{VID: "303a", PID: "1001"}},
	"heltec-v3":     {{VID: "10c4", PID: "ea60"}},
	"tbeam":         {{VID: "10c4", PID: "ea60"}},
	"tlora":         {{VID: "1a86", PID: "55d4"}, {VID: "10c4", PID: "ea60"}},
	"station-g2":    {{VID: "303a", PID: "1001"}},
	"seeed-xiao-s3": {{VID: "303a", PID: "1001"}},
}

// applyUF2Hints fills in the identification fields for a UF2-capable board.
func applyUF2Hints(d *catalog.Device) {
	id := strings.ToLower(d.ID)
	for key, boards := range uf2BoardHints {
		if strings.Contains(id, key) && len(boards) > 0 {
			d.UF2Board = mergeStrings(d.UF2Board, boards)
		}
	}
	for key, vols := range uf2VolumeHints {
		if strings.Contains(id, key) && len(vols) > 0 {
			d.UF2Volume = mergeStrings(d.UF2Volume, vols)
		}
	}
}

// applyUSBHints annotates every device with any USB IDs meshflash knows.
func applyUSBHints(devices []catalog.Device) {
	for i := range devices {
		id := strings.ToLower(devices[i].ID)
		for key, ids := range usbHints {
			if !strings.Contains(id, key) {
				continue
			}
			for _, u := range ids {
				devices[i].USB = mergeUSB(devices[i].USB, u.Normalize())
			}
		}
		applyUF2Hints(&devices[i])
	}
}

func mergeStrings(dst, src []string) []string {
	seen := map[string]bool{}
	for _, s := range dst {
		seen[strings.ToLower(s)] = true
	}
	for _, s := range src {
		if !seen[strings.ToLower(s)] {
			dst = append(dst, s)
			seen[strings.ToLower(s)] = true
		}
	}
	return dst
}

func mergeUSB(dst []catalog.USBID, u catalog.USBID) []catalog.USBID {
	for _, e := range dst {
		if e == u {
			return dst
		}
	}
	return append(dst, u)
}
