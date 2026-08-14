package device

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.bug.st/serial"
)

// TouchBaud is the magic bit rate that tells an Arduino-style USB stack to
// reboot into its bootloader. Opening the port at 1200 baud and closing it is
// the conventional signal; the Adafruit nRF52 and RP2040 cores both honour it.
//
// This is what makes auto-DFU work: without it the operator has to double-tap
// the reset button and race the bootloader's timeout.
const TouchBaud = 1200

// touchAttempts is how many times the magic-baud reboot is tried before the
// operator is asked to double-tap reset. Meshtastic's installer notes that
// "some hardware requires this twice", and the Heltec T114 is one of them.
const touchAttempts = 2

// touchSettleBudget is how long each attempt waits for a volume to mount
// before moving on. Long enough for a slow re-enumeration, short enough that
// two attempts plus the prompt arrive quickly.
const touchSettleBudget = 6 * time.Second

// ErrBootloaderTimeout means the device never presented a bootloader volume.
var ErrBootloaderTimeout = errors.New("device did not enter its UF2 bootloader")

// Touch opens a serial port at 1200 baud and closes it, requesting a reboot
// into the bootloader.
//
// The port disappearing during or right after this is the expected outcome, so
// close errors are not treated as failures.
func Touch(portName string) error {
	p, err := serial.Open(portName, &serial.Mode{BaudRate: TouchBaud})
	if err != nil {
		return fmt.Errorf("open %s at %d baud: %w", portName, TouchBaud, err)
	}
	// Let the host USB stack settle so the open is actually observed before
	// the close; some stacks coalesce an immediate open/close pair.
	time.Sleep(100 * time.Millisecond)

	// Deasserting DTR before closing makes the reboot reliable on stacks that
	// gate the magic-baud reset on the line dropping.
	_ = p.SetDTR(false)
	_ = p.Close()
	return nil
}

// EnterBootloaderOptions configures EnterUF2Bootloader.
type EnterBootloaderOptions struct {
	// Timeout bounds the whole operation.
	Timeout time.Duration
	// PollInterval is how often to rescan for a new volume.
	PollInterval time.Duration
	// Logger receives progress. Nil discards.
	Logger *slog.Logger
	// OnManualPrompt is called once if the automatic touch does not produce a
	// bootloader, so the UI can tell the operator to double-tap reset. The
	// scan keeps running afterwards, so a manual entry is still picked up.
	OnManualPrompt func()
	// OnTouch is called before each automatic bootloader-entry attempt.
	OnTouch func(attempt, total int)
	// OnVolumeRejected is called the first time a newly-appeared volume is
	// examined and turned down, so the UI can say why rather than continuing
	// to claim nothing has happened.
	OnVolumeRejected func(Rejection)
}

func (o *EnterBootloaderOptions) applyDefaults() {
	if o.Timeout == 0 {
		// Generous, because the operator is standing there with the board and
		// a double-tap reset can take a couple of tries to land. Giving up
		// after half a minute means starting the whole command again.
		o.Timeout = 3 * time.Minute
	}
	if o.PollInterval == 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.New(nopHandler{})
	}
}

// EnterUF2Bootloader gets a device into its UF2 bootloader and returns the
// volume it mounted as.
//
// If the target is already a bootloader volume this returns immediately. For a
// serial port it touches at 1200 baud and waits for a new volume to appear,
// falling back to prompting for a manual double-tap reset.
func EnterUF2Bootloader(ctx context.Context, t Target, opts EnterBootloaderOptions) (Volume, error) {
	opts.applyDefaults()

	if t.Volume != nil {
		return *t.Volume, nil
	}
	if t.Port == nil {
		return Volume{}, errors.New("target is neither a serial port nor a bootloader volume")
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Snapshot first so a bootloader that was already mounted for a different
	// board is not mistaken for the one we just rebooted.
	before, err := ScanVolumes()
	if err != nil {
		opts.Logger.Debug("pre-touch volume scan failed", "error", err)
	}
	known := volumeSet(before)

	// Two attempts before asking for help.
	//
	// Meshtastic's own installer documents this: "Some hardware requires this
	// twice." The first touch often only knocks the board out of its
	// application, and the second is what lands it in the bootloader.
	for attempt := 1; attempt <= touchAttempts; attempt++ {
		opts.Logger.Info("requesting bootloader entry",
			"port", t.Port.Name, "baud", TouchBaud, "attempt", attempt)
		if opts.OnTouch != nil {
			opts.OnTouch(attempt, touchAttempts)
		}

		if err := Touch(t.Port.Name); err != nil {
			// A busy or vanished port is common here and not fatal: the device
			// may already be rebooting. Keep scanning and let the wait decide.
			opts.Logger.Debug("touch did not complete cleanly", "attempt", attempt, "error", err)
		}

		vol, err := waitForNewVolume(ctx, known, opts, touchSettleBudget)
		if err == nil {
			return vol, nil
		}
		if !errors.Is(err, ErrBootloaderTimeout) {
			return Volume{}, err
		}
	}

	// The touch did not take. Some boards need the button.
	if opts.OnManualPrompt != nil {
		opts.OnManualPrompt()
	}
	opts.Logger.Warn("automatic bootloader entry did not take; waiting for a manual double-tap reset")

	return waitForNewVolume(ctx, known, opts, 0)
}

// waitForNewVolume polls until a volume appears that was not in `known`.
// A non-zero budget bounds this phase separately from the context deadline.
func waitForNewVolume(ctx context.Context, known map[string]bool, opts EnterBootloaderOptions, budget time.Duration) (Volume, error) {
	deadline := time.Time{}
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}

	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	// Rejections are reported once each. A board that mounts but is not
	// recognised — the macOS removable-volume permission case — otherwise
	// looks identical to a board that never rebooted at all.
	reported := map[string]bool{}

	for {
		vols, rejected, err := ScanVolumesVerbose()
		if err != nil {
			opts.Logger.Debug("volume scan failed", "error", err)
		}
		for _, v := range vols {
			if !known[v.Path] {
				opts.Logger.Info("bootloader volume appeared", "path", v.Path, "board", v.Info.BoardID)
				// Give the OS a moment to finish mounting read-write before
				// anything tries to copy onto it.
				time.Sleep(300 * time.Millisecond)
				return v, nil
			}
		}
		for _, r := range rejected {
			if known[r.Path] || reported[r.Path] {
				continue
			}
			reported[r.Path] = true
			// A volume that appeared after the touch and was still rejected is
			// almost certainly the bootloader we are waiting for.
			opts.Logger.Warn("a new volume appeared but was not recognised as a bootloader",
				"path", r.Path, "reason", r.Reason)
			if opts.OnVolumeRejected != nil {
				opts.OnVolumeRejected(r)
			}
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			return Volume{}, ErrBootloaderTimeout
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Volume{}, ErrBootloaderTimeout
			}
			return Volume{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitForPort polls until a serial port with the given name exists, which is
// how meshflash waits out a device re-enumerating after a reset.
func WaitForPort(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ports, err := ListPorts()
		if err == nil {
			for _, p := range ports {
				if p.Name == name {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("serial port %s did not reappear within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func volumeSet(vols []Volume) map[string]bool {
	m := make(map[string]bool, len(vols))
	for _, v := range vols {
		m[v.Path] = true
	}
	return m
}

type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (nopHandler) WithAttrs([]slog.Attr) slog.Handler        { return nopHandler{} }
func (nopHandler) WithGroup(string) slog.Handler             { return nopHandler{} }
