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
		{"239a", "8029", false}, // T114 running its application (no BOOT marker)
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
// incomplete by construction — a Heltec T114 reports 239a:0071, and a Seeed
// T1000-E's bootloader reports 239a:8029, which is the *application* id on the
// T114. No list can satisfy both.
//
// Two boards, two different behaviours, both observed on hardware:
//
//	T114     app 239a:8029 serial EB300C0DB2B2863C  ->  boot 239a:0071 same serial
//	T1000-E  app 2886:0057 serial 33E89E16F14744EB  ->  boot 239a:8029 serial FEA5A1A48C126E65
//
// The T114 keeps its chip serial across the reboot; the T1000-E changes it
// completely. What both keep is the port name, and this is only ever asked
// moments after a reboot was requested on that exact port.
func TestIsRebootedInto(t *testing.T) {
	t114App := Port{
		Name: "/dev/cu.usbmodem2101", IsUSB: true,
		VID: "239A", PID: "8029", SerialNumber: "EB300C0DB2B2863C", Product: "HT-n5262",
	}
	t114Boot := Port{
		Name: "/dev/cu.usbmodem2101", IsUSB: true,
		VID: "239A", PID: "0071", SerialNumber: "EB300C0DB2B2863C", Product: "HT-n5262",
	}
	if !IsRebootedInto(t114App, t114Boot) {
		t.Error("missed a T114 rebooting into its bootloader (stable serial)")
	}

	t1000App := Port{
		Name: "/dev/cu.usbmodem112401", IsUSB: true,
		VID: "2886", PID: "0057", SerialNumber: "33E89E16F14744EB", Product: "T1000-E",
	}
	t1000Boot := Port{
		Name: "/dev/cu.usbmodem112401", IsUSB: true,
		VID: "239A", PID: "8029", SerialNumber: "FEA5A1A48C126E65", Product: "T1000-E-BOOT",
	}
	if !IsRebootedInto(t1000App, t1000Boot) {
		t.Error("missed a T1000-E rebooting into its bootloader (serial changes too)")
	}

	// Nothing changed, so nothing rebooted.
	if IsRebootedInto(t114App, t114App) {
		t.Error("an unchanged port was reported as rebooted")
	}

	// A board on a different port is a different board. The port name is the
	// discriminator, since two devices cannot hold one name at once.
	elsewhere := t1000Boot
	elsewhere.Name = "/dev/cu.usbmodem99999"
	if IsRebootedInto(t114App, elsewhere) {
		t.Error("a board on another port was mistaken for ours")
	}

	// A stable serial still confirms a match even across a port rename.
	renamed := t114Boot
	renamed.Name = "/dev/cu.usbmodem2102"
	if !IsRebootedInto(t114App, renamed) {
		t.Error("a stable chip serial should identify the board even if the port name moves")
	}
}

// A "-BOOT" product marker is the one bootloader signal that works on both
// boards, since the T1000-E's bootloader product id collides with the T114's
// application id.
func TestBootloaderProductMarker(t *testing.T) {
	if !LooksLikeNRF52Bootloader(Port{VID: "239a", PID: "8029", Product: "T1000-E-BOOT"}) {
		t.Error("did not recognise a bootloader from its product string")
	}
	// The same ids without the marker are a running T114, not a bootloader.
	if LooksLikeNRF52Bootloader(Port{VID: "239a", PID: "8029", Product: "HT-n5262"}) {
		t.Error("mistook a running board for a bootloader")
	}
}
