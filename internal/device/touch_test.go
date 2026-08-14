package device

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A board already sitting in its bootloader must not be touched.
//
// Touching a device that is already in DFU reboots it back out into the
// application — the opposite of what was asked for. This is reachable in
// normal use: the operator double-taps reset, then runs meshflash.
func TestEnterBootloaderDoesNotTouchABootloader(t *testing.T) {
	// 239a:0029 is the Adafruit nRF52840 bootloader.
	port := Port{Name: "/dev/cu.usbmodem-test", IsUSB: true, VID: "239a", PID: "0029"}
	target := Target{Port: &port}

	// A short deadline: if this path tried to touch or wait for a volume it
	// would block for seconds, so a fast return is itself part of the assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := EnterUF2Bootloader(ctx, target, EnterBootloaderOptions{
		OnTouch: func(attempt, total int) {
			t.Errorf("touched a device that was already in its bootloader (attempt %d)", attempt)
		},
	})

	var serialOnly *SerialOnlyBootloaderError
	if !errors.As(err, &serialOnly) {
		t.Fatalf("got %v, want a SerialOnlyBootloaderError", err)
	}
	if serialOnly.Port.Name != port.Name {
		t.Errorf("reported port %q, want %q", serialOnly.Port.Name, port.Name)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s to notice an existing bootloader; it should be immediate", elapsed)
	}
}

// An already-mounted bootloader volume short-circuits everything.
func TestEnterBootloaderUsesAnExistingVolume(t *testing.T) {
	vol := Volume{Path: "/Volumes/T114BOOT", Label: "T114BOOT"}
	target := Target{Volume: &vol}

	got, err := EnterUF2Bootloader(context.Background(), target, EnterBootloaderOptions{
		OnTouch: func(attempt, total int) {
			t.Error("touched a device that already had a bootloader volume")
		},
	})
	if err != nil {
		t.Fatalf("EnterUF2Bootloader: %v", err)
	}
	if got.Path != vol.Path {
		t.Errorf("returned %q, want %q", got.Path, vol.Path)
	}
}

func TestLooksLikeNRF52Bootloader(t *testing.T) {
	cases := []struct {
		vid, pid string
		want     bool
	}{
		{"239a", "0029", true},  // Adafruit nRF52840 bootloader
		{"239a", "002a", true},  // Adafruit nRF52 bootloader
		{"239a", "0071", true},  // Heltec T114 bootloader, observed on hardware
		{"239a", "8029", false}, // the same board running its application
		{"10c4", "ea60", false}, // CP2102 bridge
		{"", "", false},
	}
	for _, c := range cases {
		got := LooksLikeNRF52Bootloader(Port{VID: c.vid, PID: c.pid})
		if got != c.want {
			t.Errorf("%s:%s = %v, want %v", c.vid, c.pid, got, c.want)
		}
	}
}

// The serial-only signal must not be mistaken for a timeout.
//
// This is the entire no-button path: the flash layer routes
// SerialOnlyBootloaderError to serial DFU, but only if it can still recognise
// it. If this ever started matching ErrBootloaderTimeout it would be reported
// as "the board never entered its bootloader" and the operator would be told
// to press reset — exactly the behaviour the serial DFU path exists to remove.
func TestSerialOnlyIsNotATimeout(t *testing.T) {
	err := error(&SerialOnlyBootloaderError{Port: Port{Name: "/dev/cu.usbmodem2101"}})

	if errors.Is(err, ErrBootloaderTimeout) {
		t.Fatal("a serial-only bootloader is being reported as a timeout")
	}

	// It must survive wrapping, since the flash layer inspects a returned error.
	wrapped := fmt.Errorf("entering bootloader: %w", err)
	var serialOnly *SerialOnlyBootloaderError
	if !errors.As(wrapped, &serialOnly) {
		t.Fatal("SerialOnlyBootloaderError is not recoverable through a wrap")
	}
	if serialOnly.Port.Name != "/dev/cu.usbmodem2101" {
		t.Errorf("port = %q", serialOnly.Port.Name)
	}
	if serialOnly.Error() == "" {
		t.Error("error message is empty")
	}
}

// Detecting the bootloader by product id alone is not enough.
//
// Vendors rebuild Adafruit's bootloader with their own id, so any allowlist is
// incomplete by construction — a Heltec T114 reports 239a:0071, and meshflash
// looked straight at one and failed to see it. The chip serial number survives
// the reboot, so the same serial with a different product id is unambiguously
// the same board in a different mode.
//
// These are the exact values observed on a real T114: the port name does not
// even change, only the product id.
func TestIsRebootedInto(t *testing.T) {
	app := Port{
		Name: "/dev/cu.usbmodem2101", IsUSB: true,
		VID: "239A", PID: "8029", SerialNumber: "EB300C0DB2B2863C", Product: "HT-n5262",
	}
	boot := Port{
		Name: "/dev/cu.usbmodem2101", IsUSB: true,
		VID: "239A", PID: "0071", SerialNumber: "EB300C0DB2B2863C", Product: "HT-n5262",
	}

	if !IsRebootedInto(app, boot) {
		t.Error("did not recognise a T114 that rebooted into its bootloader")
	}
	// Same mode is not a reboot.
	if IsRebootedInto(app, app) {
		t.Error("an unchanged port was reported as rebooted")
	}
	// A different board that happens to be in bootloader mode is not ours.
	other := boot
	other.SerialNumber = "0123456789ABCDEF"
	if IsRebootedInto(app, other) {
		t.Error("a different board was mistaken for ours")
	}
	// Without a serial number there is nothing to correlate on, so this must
	// not guess — a CH340 with no serial would otherwise match anything.
	noSerial := app
	noSerial.SerialNumber = ""
	if IsRebootedInto(noSerial, boot) {
		t.Error("matched despite having no serial number to correlate on")
	}
	// Case differences in the reported hex must not defeat the match.
	lower := boot
	lower.SerialNumber = "eb300c0db2b2863c"
	if !IsRebootedInto(app, lower) {
		t.Error("serial number comparison is case sensitive")
	}
}
