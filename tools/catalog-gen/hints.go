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

// deviceAliases maps a MeshCore device slug onto the canonical device id,
// which is Meshtastic's platformioTarget.
//
// The two projects name the same hardware differently — MeshCore derives its
// slug from a release asset name, Meshtastic uses its PlatformIO target — so
// without this a Heltec T114 exists twice in the catalog, as
// "heltec-mesh-node-t114" and "heltec-t114". That is not just untidy: a board
// bound to one id can never be offered the other project's firmware, so
// switching a node between Meshtastic and MeshCore becomes impossible.
//
// Entries must be one-to-one. Where MeshCore splits a board that Meshtastic
// treats as one (t-beam by radio chip, station-g3 by logging) the mapping
// would be many-to-one, and folding those together would make two different
// firmware images collide on the same key — so those stay separate. Boards
// only one project supports need no entry.
var deviceAliases = map[string]string{
	// Heltec
	"heltec-t114":       "heltec-mesh-node-t114",
	"heltec-t1":         "heltec-mesh-node-t1",
	"heltec-t096":       "heltec-mesh-node-t096",
	"heltec-e213":       "heltec-vision-master-e213",
	"heltec-e290":       "heltec-vision-master-e290",
	"heltec-wsl3":       "heltec-wsl-v3",
	"heltec-tracker-v2": "heltec-wireless-tracker-v2",

	// RAK
	"rak-4631":        "rak4631",
	"rak-11310":       "rak11310",
	"rak-wismesh-tag": "rak_wismeshtag",

	// Seeed
	"xiao-nrf52":       "seeed_xiao_nrf52840_kit",
	"xiao-s3":          "seeed-xiao-s3",
	"wiotrackerl1":     "seeed_wio_tracker_L1",
	"wiotrackerl1eink": "seeed_wio_tracker_L1_eink",
	"t1000e":           "tracker-t1000-e",
	"wio-wm1110":       "wio-tracker-wm1110",

	// LilyGo
	"lilygo-t-echo":            "t-echo",
	"lilygo-t-echo-lite":       "t-echo-lite",
	"lilygo-t-impulse-plus":    "t-impulse-plus",
	"lilygo-tdeck":             "t-deck",
	"lilygo-tbeam-1w":          "t-beam-1w",
	"lilygo-tlora-v2-1-1-6":    "tlora-v2-1-1_6",
	"lilygo-teth-elite-sx1262": "t-eth-elite",

	// Others
	"m5stack-unit-c6l": "m5stack-unitc6l",
	"thinknode-m1":     "thinknode_m1",
	"thinknode-m2":     "thinknode_m2",
	"thinknode-m3":     "thinknode_m3",
	"thinknode-m5":     "thinknode_m5",
	"thinknode-m6":     "thinknode_m6",
	"thinknode-m7":     "thinknode_m7",
}

// canonicalDeviceID resolves a project-specific slug to the shared id.
func canonicalDeviceID(id string) string {
	if canon, ok := deviceAliases[id]; ok {
		return canon
	}
	return id
}

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

// usbProductHints maps a device id substring to the USB product string the
// board reports while running its application.
//
// This is the best identification signal a running board offers: vendors put
// the board name here, so unlike a VID/PID it names the model, and unlike
// INFO_UF2.TXT it is readable without the board being in its bootloader. A
// Heltec T114 reports "HT-n5262" in both places.
// Keys here are matched exactly, not as substrings: "t114" would also match
// heltec-t114-without-display, and two boards claiming the same product string
// makes identification ambiguous rather than exact — which is worse than
// having no hint at all.
var usbProductHints = map[string][]string{
	"heltec-mesh-node-t114": {"HT-n5262"},
	"heltec-mesh-node-t1":   {"HT-n5261"},
	"rak4631":               {"WisCore RAK4631 Board", "RAK4631"},
	"t-echo":                {"LilyGo T-Echo"},
	"seeed_xiao_nrf52840":   {"XIAO nRF52840"},
	"tracker-t1000-e":       {"T1000-E"},
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
	applyProductHints(d)
}

// applyProductHints fills in the USB product strings a board reports.
func applyProductHints(d *catalog.Device) {
	id := strings.ToLower(d.ID)
	if products, ok := usbProductHints[id]; ok {
		d.USBProduct = mergeStrings(d.USBProduct, products)
	}
	// A UF2 Board-ID and the application's USB product string are frequently
	// the same vendor string, so each doubles as the other.
	if products, ok := usbProductHints[id]; ok {
		d.UF2Board = mergeStrings(d.UF2Board, products)
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
		applyProductHints(&devices[i])
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
