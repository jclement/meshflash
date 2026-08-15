package device

import (
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
)

// bridge describes a USB-serial interface chip.
type bridge struct {
	name string
	// shared is true when the chip is a generic UART bridge soldered onto
	// countless unrelated boards. A match on one of these identifies the
	// adapter, never the board, so meshflash must not present it as an
	// identification.
	shared bool
	// driver, when set, is the vendor driver page an operator may need. This
	// is the single largest source of "it doesn't show up" support traffic,
	// especially on Windows.
	driver string
}

// knownBridges maps USB VID:PID to the interface chip behind it.
//
// Entries marked shared are UART bridges. Entries not marked shared are native
// USB peripherals on the MCU itself, which at least pin the chip family.
var knownBridges = map[string]bridge{
	// Silicon Labs CP210x — Heltec, T-Beam, many ESP32 boards.
	"10c4:ea60": {name: "Silicon Labs CP210x", shared: true, driver: "https://www.silabs.com/developer-tools/usb-to-uart-bridge-vcp-drivers"},
	"10c4:ea70": {name: "Silicon Labs CP2105", shared: true, driver: "https://www.silabs.com/developer-tools/usb-to-uart-bridge-vcp-drivers"},
	"10c4:ea71": {name: "Silicon Labs CP2108", shared: true, driver: "https://www.silabs.com/developer-tools/usb-to-uart-bridge-vcp-drivers"},

	// WCH CH34x — cheap ESP32 clones. The usual Windows failure.
	"1a86:7523": {name: "WCH CH340", shared: true, driver: "https://www.wch-ic.com/downloads/CH341SER_EXE.html"},
	"1a86:5523": {name: "WCH CH341", shared: true, driver: "https://www.wch-ic.com/downloads/CH341SER_EXE.html"},
	"1a86:55d4": {name: "WCH CH9102", shared: true, driver: "https://www.wch-ic.com/downloads/CH343SER_EXE.html"},
	"1a86:55d3": {name: "WCH CH9102F", shared: true, driver: "https://www.wch-ic.com/downloads/CH343SER_EXE.html"},

	// FTDI.
	"0403:6001": {name: "FTDI FT232R", shared: true, driver: "https://ftdichip.com/drivers/vcp-drivers/"},
	"0403:6010": {name: "FTDI FT2232", shared: true, driver: "https://ftdichip.com/drivers/vcp-drivers/"},
	"0403:6015": {name: "FTDI FT231X", shared: true, driver: "https://ftdichip.com/drivers/vcp-drivers/"},

	// Prolific.
	"067b:2303": {name: "Prolific PL2303", shared: true, driver: "https://www.prolific.com.tw/US/ShowProduct.aspx?p_id=225&pcid=41"},

	// Espressif native USB-Serial/JTAG. Present on ESP32-S2/S3/C3/C6 boards
	// wired straight to the MCU, so this does pin the chip family.
	"303a:1001": {name: "Espressif USB JTAG/serial debug unit"},
	"303a:0002": {name: "Espressif ESP32-S2 CDC"},
	"303a:4001": {name: "Espressif ESP32-S3 CDC"},

	// Nordic / Adafruit nRF52 bootloader CDC. Seeing this means the board is
	// an nRF52 running the Adafruit bootloader — exactly what serial DFU wants.
	"239a:0029": {name: "Adafruit nRF52840 bootloader"},
	"239a:8029": {name: "Adafruit nRF52840 application"},
	"239a:002a": {name: "Adafruit nRF52 bootloader"},
	"1915:521f": {name: "Nordic nRF52840 CDC"},

	// Raspberry Pi RP2040/RP2350.
	"2e8a:0003": {name: "Raspberry Pi RP2040 BOOTSEL"},
	"2e8a:000a": {name: "Raspberry Pi RP2040 CDC"},
	"2e8a:0005": {name: "Raspberry Pi RP2040 CDC"},
	"2e8a:000f": {name: "Raspberry Pi RP2350 BOOTSEL"},
}

// IsSharedBridge reports whether a USB ID belongs to a generic UART bridge
// rather than to the board itself.
func IsSharedBridge(id catalog.USBID) bool {
	b, ok := knownBridges[id.Normalize().String()]
	return ok && b.shared
}

// BridgeName returns a human name for a USB ID, or "" when unknown.
func BridgeName(id catalog.USBID) string {
	return knownBridges[id.Normalize().String()].name
}

// DriverURL returns the vendor driver download for a USB ID, or "" when the
// device needs no extra driver.
func DriverURL(id catalog.USBID) string {
	return knownBridges[id.Normalize().String()].driver
}

// NativeUSB reports whether the USB ID is the MCU's own USB peripheral. These
// boards reset into their bootloader differently (USB-JTAG sequence on ESP32,
// 1200-baud touch on nRF52) and re-enumerate mid-flash.
func NativeUSB(id catalog.USBID) bool {
	b, ok := knownBridges[id.Normalize().String()]
	return ok && !b.shared
}

// adafruitVID is the vendor id the nRF52 bootloader ships under, including on
// boards from other vendors that build it with their own product id.
const adafruitVID = "239a"

// knownBootloaderPIDs are product ids observed on an nRF52 bootloader.
//
// Vendors rebuild Adafruit's bootloader with their own product id, so this
// list is necessarily incomplete — a Heltec T114 reports 239a:0071, nothing
// like Adafruit's own 239a:0029. Matching only against a list like this is
// what made meshflash stare straight at a T114 bootloader and fail to see it,
// so it is a fast path, not the whole answer: see IsRebootedInto.
var knownBootloaderPIDs = map[string]string{
	"0029": "Adafruit nRF52840 bootloader",
	"002a": "Adafruit nRF52 bootloader",
	"0071": "Heltec nRF52 bootloader (T114 and relatives)",
	"0050": "Nordic nRF52 bootloader",
}

// LooksLikeNRF52Bootloader reports whether a port is an nRF52 bootloader
// waiting in serial DFU mode.
//
// The product string is checked first and matters more than the ids. Product
// ids are not usable on their own here: a Seeed T1000-E's bootloader reports
// 239a:8029, which is the *application* id on a Heltec T114, so any allowlist
// either misses the one or misreads the other. A "-BOOT" marker in the product
// string is unambiguous, and vendors set it precisely to say so.
func LooksLikeNRF52Bootloader(p Port) bool {
	if _, isBoot := trimBootSuffix(strings.TrimSpace(p.Product)); isBoot {
		return true
	}
	id := (catalog.USBID{VID: p.VID, PID: p.PID}).Normalize()
	if id.VID != adafruitVID {
		return false
	}
	_, known := knownBootloaderPIDs[id.PID]
	return known
}

// IsRebootedInto reports whether `now` is the same physical board as `before`,
// having re-enumerated as something else.
//
// This is the reliable signal, and the one a product-id list cannot give. The
// USB serial number is burned into the chip and survives the reboot, so a port
// carrying the same serial with a different product id is unambiguously the
// same board in a different mode — which, right after a bootloader-entry
// request, means it is now in its bootloader.
//
// On a Heltec T114 the port name does not even change (/dev/cu.usbmodem2101
// both times); only the product id moves, 239a:8029 to 239a:0071.
func IsRebootedInto(before, now Port) bool {
	beforeID := (catalog.USBID{VID: before.VID, PID: before.PID}).Normalize()
	nowID := (catalog.USBID{VID: now.VID, PID: now.PID}).Normalize()
	if beforeID == nowID {
		return false // nothing changed, so nothing rebooted
	}

	// A stable chip serial across the change is conclusive. This is how a
	// Heltec T114 presents: 239a:8029 to 239a:0071, same serial throughout.
	if before.SerialNumber != "" && now.SerialNumber != "" &&
		strings.EqualFold(before.SerialNumber, now.SerialNumber) {
		return true
	}

	// Not every board keeps it. A Seeed T1000-E reports an entirely different
	// serial in its bootloader — 2886:0057/33E89E16F14744EB running, versus
	// 239a:8029/FEA5A1A48C126E65 in DFU — so the serial cannot be required.
	// The port name survives on both boards, and this is only ever asked
	// moments after we requested a reboot on that exact port, so a changed id
	// at an unchanged name is the same device.
	return before.Name != "" && before.Name == now.Name
}
