// Package doctor diagnoses why a device is not showing up or not flashing.
//
// "It doesn't appear" is the dominant support cost with these boards, and the
// causes are boring and fixable: a missing USB-UART driver on Windows, a
// missing dialout group on Linux, a charge-only cable, or another program
// holding the port. This package checks each and, where a driver is genuinely
// needed, says exactly which one and where to get it.
package doctor

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"sort"
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/config"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/store"
)

// Status grades a check.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusInfo Status = "info"
)

// Check is one diagnostic result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	// Fix is a concrete action, including a URL when a download is needed.
	Fix string `json:"fix,omitempty"`
}

// Report is the full diagnosis.
type Report struct {
	Checks  []Check         `json:"checks"`
	Targets []device.Target `json:"targets"`
	// MissingDrivers lists driver downloads implied by attached hardware.
	MissingDrivers []DriverHint `json:"missing_drivers,omitempty"`
	// RejectedVolumes are mount points examined and not treated as bootloaders.
	RejectedVolumes []device.Rejection `json:"rejected_volumes,omitempty"`
}

// DriverHint names a driver an operator may need.
type DriverHint struct {
	Chip string `json:"chip"`
	URL  string `json:"url"`
	Why  string `json:"why"`
}

// Worst returns the most severe status in the report.
func (r Report) Worst() Status {
	worst := StatusOK
	for _, c := range r.Checks {
		switch c.Status {
		case StatusFail:
			return StatusFail
		case StatusWarn:
			worst = StatusWarn
		}
	}
	return worst
}

// Options configures Run.
type Options struct {
	Paths   config.Paths
	Cfg     config.Config
	Catalog *catalog.Catalog
	Store   *store.Store
	// CatalogErr is any error encountered loading the catalog.
	CatalogErr error
}

// Run performs every diagnostic and returns a report.
func Run(opts Options) Report {
	var r Report

	r.Checks = append(r.Checks, checkHome(opts.Paths))
	r.Checks = append(r.Checks, checkCatalog(opts.Catalog, opts.CatalogErr))
	r.Checks = append(r.Checks, checkSelection(opts.Cfg, opts.Catalog))
	if opts.Store != nil {
		r.Checks = append(r.Checks, checkCache(opts.Store))
	}
	r.Checks = append(r.Checks, checkSerialPermissions())

	det, errs := device.Detect()
	for _, err := range errs {
		r.Checks = append(r.Checks, Check{
			Name:   "Device enumeration",
			Status: StatusWarn,
			Detail: err.Error(),
		})
	}

	r.Targets = device.Identify(det, opts.Catalog)
	r.Checks = append(r.Checks, checkTargets(r.Targets))
	r.MissingDrivers = driverHints(det)

	// Mount points that were examined and turned down. A board sitting in its
	// bootloader that meshflash cannot see is otherwise indistinguishable from
	// a board that never rebooted, and on macOS the usual cause is a removable
	// volume permission the terminal has not been granted.
	if _, rejected, err := device.ScanVolumesVerbose(); err == nil {
		r.RejectedVolumes = rejected
		if c, ok := checkRejectedVolumes(rejected); ok {
			r.Checks = append(r.Checks, c)
		}
	}

	return r
}

// checkRejectedVolumes flags mount points that could not be read at all, which
// is a fixable permission problem rather than an ordinary non-bootloader disk.
func checkRejectedVolumes(rejected []device.Rejection) (Check, bool) {
	for _, r := range rejected {
		if !strings.Contains(r.Reason, "permission denied") {
			continue
		}
		return Check{
			Name:   "Removable volume access",
			Status: StatusFail,
			Detail: fmt.Sprintf("%s is mounted but cannot be read", r.Path),
			Fix: "Grant your terminal access under System Settings → Privacy & Security → " +
				"Files and Folders → Removable Volumes, then restart the terminal. " +
				"Without it a board in its UF2 bootloader is invisible to meshflash.",
		}, true
	}
	return Check{}, false
}

func checkHome(p config.Paths) Check {
	if err := p.EnsureDirs(); err != nil {
		return Check{
			Name:   "meshflash home",
			Status: StatusFail,
			Detail: fmt.Sprintf("cannot create %s: %v", p.Home, err),
			Fix:    "Check the directory is writable, or set MESHFLASH_HOME to somewhere it is.",
		}
	}
	// Confirm it is actually writable, not merely present.
	probe := p.Home + string(os.PathSeparator) + ".write-probe"
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return Check{
			Name:   "meshflash home",
			Status: StatusFail,
			Detail: fmt.Sprintf("%s is not writable: %v", p.Home, err),
			Fix:    "Fix the permissions, or set MESHFLASH_HOME to a writable directory.",
		}
	}
	os.Remove(probe)

	return Check{Name: "meshflash home", Status: StatusOK, Detail: p.Home}
}

func checkCatalog(cat *catalog.Catalog, err error) Check {
	if err != nil {
		return Check{
			Name:   "Firmware catalog",
			Status: StatusFail,
			Detail: err.Error(),
			Fix:    "Run `meshflash update` while online to fetch the catalog.",
		}
	}
	if cat == nil {
		return Check{
			Name:   "Firmware catalog",
			Status: StatusFail,
			Detail: "no catalog on disk",
			Fix:    "Run `meshflash update` while online.",
		}
	}

	devices := len(cat.DeviceIDs())
	releases := 0
	for _, p := range cat.Projects {
		releases += len(p.Releases)
	}
	age := "unknown age"
	if !cat.GeneratedAt.IsZero() {
		age = cat.GeneratedAt.Format("2006-01-02")
	}
	return Check{
		Name:   "Firmware catalog",
		Status: StatusOK,
		Detail: fmt.Sprintf("%d devices, %d projects, %d releases (generated %s)", devices, len(cat.Projects), releases, age),
	}
}

func checkSelection(cfg config.Config, cat *catalog.Catalog) Check {
	if len(cfg.Devices) == 0 {
		return Check{
			Name:   "Device selection",
			Status: StatusWarn,
			Detail: "no devices selected, so `update` would cache firmware for every board",
			Fix:    "Run `meshflash configure` to pick the boards you actually carry. A full Meshtastic release is a few hundred megabytes.",
		}
	}
	if cat == nil {
		return Check{Name: "Device selection", Status: StatusInfo, Detail: fmt.Sprintf("%d devices selected", len(cfg.Devices))}
	}

	var unknown []string
	for _, id := range cfg.Devices {
		if _, ok := cat.DeviceByID(id); !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return Check{
			Name:   "Device selection",
			Status: StatusWarn,
			Detail: fmt.Sprintf("%d selected devices are not in the catalog: %s", len(unknown), strings.Join(unknown, ", ")),
			Fix:    "Run `meshflash configure` to refresh your selection.",
		}
	}
	return Check{
		Name:   "Device selection",
		Status: StatusOK,
		Detail: fmt.Sprintf("%d devices selected", len(cfg.Devices)),
	}
}

func checkCache(s *store.Store) Check {
	u, err := s.Usage()
	if err != nil {
		return Check{Name: "Firmware cache", Status: StatusWarn, Detail: err.Error()}
	}
	if u.ExtractedFiles == 0 && u.DownloadFiles == 0 {
		return Check{
			Name:   "Firmware cache",
			Status: StatusWarn,
			Detail: "empty",
			Fix:    "Run `meshflash update` while online. Nothing can be flashed offline until this is populated.",
		}
	}
	return Check{
		Name:   "Firmware cache",
		Status: StatusOK,
		Detail: fmt.Sprintf("%s across %d firmware files (%s of source archives)",
			store.FormatBytes(u.Extracted), u.ExtractedFiles, store.FormatBytes(u.Downloads)),
	}
}

// checkSerialPermissions catches the Linux dialout-group problem, which
// presents as ports being visible but unopenable.
func checkSerialPermissions() Check {
	if runtime.GOOS != "linux" {
		return Check{
			Name:   "Serial port permissions",
			Status: StatusOK,
			Detail: "not applicable on " + runtime.GOOS,
		}
	}
	if os.Geteuid() == 0 {
		return Check{Name: "Serial port permissions", Status: StatusOK, Detail: "running as root"}
	}

	groups, err := os.Getgroups()
	if err != nil {
		return Check{Name: "Serial port permissions", Status: StatusInfo, Detail: "could not read group membership"}
	}

	// Distros use dialout (Debian/Ubuntu) or uucp (Arch/Fedora).
	for _, name := range []string{"dialout", "uucp", "plugdev"} {
		g, err := user.LookupGroup(name)
		if err != nil {
			continue
		}
		for _, gid := range groups {
			if fmt.Sprint(gid) == g.Gid {
				return Check{
					Name:   "Serial port permissions",
					Status: StatusOK,
					Detail: "member of " + name,
				}
			}
		}
	}

	return Check{
		Name:   "Serial port permissions",
		Status: StatusWarn,
		Detail: "not a member of dialout, uucp or plugdev, so serial ports will fail to open",
		Fix:    "Run: sudo usermod -aG dialout $USER    then log out and back in.",
	}
}

func checkTargets(targets []device.Target) Check {
	if len(targets) == 0 {
		return Check{
			Name:   "Connected devices",
			Status: StatusWarn,
			Detail: "none found",
			Fix: "Check the USB cable carries data (many charge-only cables do not), " +
				"then re-plug the board. " + platformHint(),
		}
	}

	identified := 0
	for _, t := range targets {
		if t.Resolved() {
			identified++
		}
	}
	return Check{
		Name:   "Connected devices",
		Status: StatusOK,
		Detail: fmt.Sprintf("%d attached, %d identified themselves", len(targets), identified),
	}
}

func platformHint() string {
	switch runtime.GOOS {
	case "windows":
		return "On Windows, a board that appears in Device Manager under 'Other devices' needs its USB-UART driver installed."
	case "darwin":
		return "On macOS, run `ls /dev/cu.*` to confirm the port exists."
	default:
		return "On Linux, run `dmesg | tail` after plugging in to see whether the kernel bound a driver."
	}
}

// driverHints reports vendor drivers implied by attached hardware.
//
// A device that already enumerated as a serial port clearly has a working
// driver, so its chip is reported as informational. The valuable case is
// Windows, where a board with no driver never becomes a port at all — so the
// hint doubles as a list of what to pre-install on a field machine.
func driverHints(d device.Detection) []DriverHint {
	seen := map[string]bool{}
	var out []DriverHint

	for _, p := range d.Ports {
		if !p.IsUSB {
			continue
		}
		id := catalog.USBID{VID: p.VID, PID: p.PID}
		url := device.DriverURL(id)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, DriverHint{
			Chip: device.BridgeName(id),
			URL:  url,
			Why:  fmt.Sprintf("%s uses this bridge; install it on any machine where the board does not appear as a port", p.Name),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Chip < out[j].Chip })
	return out
}

// CommonDrivers lists the drivers worth pre-installing on a field machine,
// independent of what happens to be plugged in right now.
//
// This is the list an operator wants before going somewhere without a network.
func CommonDrivers() []DriverHint {
	return []DriverHint{
		{
			Chip: "Silicon Labs CP210x",
			URL:  "https://www.silabs.com/developer-tools/usb-to-uart-bridge-vcp-drivers",
			Why:  "Heltec, LILYGO T-Beam and most older ESP32 LoRa boards",
		},
		{
			Chip: "WCH CH340 / CH341",
			URL:  "https://www.wch-ic.com/downloads/CH341SER_EXE.html",
			Why:  "budget ESP32 boards; the most common Windows failure",
		},
		{
			Chip: "WCH CH9102",
			URL:  "https://www.wch-ic.com/downloads/CH343SER_EXE.html",
			Why:  "newer LILYGO and Heltec revisions",
		},
		{
			Chip: "FTDI FT232",
			URL:  "https://ftdichip.com/drivers/vcp-drivers/",
			Why:  "older and industrial boards",
		},
	}
}
