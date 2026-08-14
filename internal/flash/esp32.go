package flash

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jclement/meshflash/internal/catalog"
	"tinygo.org/x/espflasher/pkg/espflasher"
)

// flashESP32 writes images over the ESP serial bootloader using tinygo's
// espflasher, which implements the same protocol and stub loader as esptool.
func flashESP32(ctx context.Context, req Request) (*Result, error) {
	if req.Target.Port == nil {
		return nil, fmt.Errorf("ESP32 flashing needs a serial port, got %s", req.Target.Describe())
	}
	port := req.Target.Port.Name
	log := req.logger().With("port", port, "device", req.Device.ID)

	selected, err := selectESP32Images(req)
	if err != nil {
		return nil, err
	}

	images := make([]espflasher.ImagePart, 0, len(selected))
	var total int64
	for _, a := range selected {
		data, ok := req.Payloads[a.Name]
		if !ok {
			return nil, fmt.Errorf("missing firmware payload for %s", a.Name)
		}
		images = append(images, espflasher.ImagePart{Data: data, Offset: *a.Offset})
		total += int64(len(data))
		log.Debug("image queued", "name", a.Name, "role", string(a.Role), "offset", fmt.Sprintf("0x%X", *a.Offset), "bytes", len(data))
	}

	req.Progress.emit("connect", "Connecting to the ESP bootloader…", 0, 0)

	opts := espflasher.DefaultOptions()
	opts.Logger = slogLogger{log}
	// ResetAuto tries the classic DTR/RTS sequence, then the USB-JTAG one.
	// Boards in this space use both — a CP2102 bridge wants the former, a
	// native-USB ESP32-S3 the latter — and the operator should not have to know.
	opts.ResetMode = espflasher.ResetAuto
	opts.ConnectStatus = func(phase espflasher.ConnectPhase, attempt, maxAttempts int, message string) {
		msg := message
		if maxAttempts > 0 {
			msg = fmt.Sprintf("%s (attempt %d/%d)", message, attempt, maxAttempts)
		}
		req.Progress.emit("connect", msg, 0, 0)
		log.Debug("bootloader handshake", "phase", string(phase), "attempt", attempt, "message", message)
	}

	f, err := espflasher.New(port, opts)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", port, explainESPError(err))
	}
	defer f.Close()

	chip := f.ChipName()
	log.Info("connected", "chip", chip)
	req.Progress.emit("connect", "Connected to "+chip, 0, 0)

	res := &Result{Chip: chip, BytesWritten: total}

	if mac, err := f.MAC(); err == nil {
		log.Info("device identity", "mac", mac.String())
	}

	if req.Erase {
		// The caller is responsible for having warned about NVS loss; by the
		// time we are here it is a deliberate choice.
		log.Warn("erasing entire flash; NVS and any stored node identity will be lost")
		req.Progress.emit("erase", "Erasing flash (this can take a minute)…", 0, 0)
		if err := f.EraseFlash(func(elapsed, estimated int) {
			req.Progress.emit("erase", "Erasing flash…", int64(elapsed), int64(estimated))
		}); err != nil {
			return nil, fmt.Errorf("erase flash: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	req.Progress.emit("write", "Writing firmware…", 0, total)
	if err := f.FlashImages(images, func(current, t int) {
		req.Progress.emit("write", "Writing firmware…", int64(current), int64(t))
	}); err != nil {
		return nil, fmt.Errorf("write firmware: %w", err)
	}

	if req.Verify {
		if err := verifyESP32(f, req, images, res, log); err != nil {
			return nil, err
		}
	}

	req.Progress.emit("reset", "Resetting device…", 0, 0)
	f.Reset()
	log.Info("flash complete", "chip", chip, "bytes", total)

	if req.Erase {
		res.Warnings = append(res.Warnings,
			"Flash was erased, so this node has a new identity: re-set its region, and expect peers to need the new public key.")
	} else {
		res.Warnings = append(res.Warnings,
			"Settings and node identity were preserved (only the app partition was written). Pass --erase for a factory-clean flash.")
	}
	return res, nil
}

// selectESP32Images chooses which images to write, which is where the
// "optional wipe" decision actually lands.
//
// Upstream ships two forms of the same firmware and its own scripts treat them
// very differently:
//
//   - the application image at 0x10000 (device-update.sh). Only the app
//     partition is touched, so NVS at 0x9000 survives and the node keeps its
//     private key, region and channels. This is the default.
//   - the factory image at 0x0 (device-install.sh, after erase_flash). It
//     spans NVS, so the node comes back with a new identity.
//
// Choosing the wrong one silently is the single most damaging thing a flasher
// can do to a deployed mesh, so the choice is explicit and the default is the
// non-destructive one.
func selectESP32Images(req Request) ([]catalog.Artifact, error) {
	artifacts := orderedArtifacts(req.Build)

	var app, factory []catalog.Artifact
	for _, a := range artifacts {
		if a.Offset == nil {
			return nil, fmt.Errorf("artifact %s has no flash offset", a.Name)
		}
		switch a.Role {
		case catalog.RoleApp:
			app = append(app, a)
		case catalog.RoleMerged:
			factory = append(factory, a)
		case catalog.RoleLittleFS:
			// Only written as part of a factory install; rewriting it during
			// an update would discard whatever the node had stored.
			if req.Erase {
				factory = append(factory, a)
			}
		default:
			// bootloader / partition table, when a project ships them apart.
			factory = append(factory, a)
		}
	}

	if req.Erase {
		if len(factory) > 0 {
			return factory, nil
		}
		if len(app) > 0 {
			// No factory image published; erasing and writing the app alone
			// would leave no bootloader, so refuse rather than brick it.
			return nil, fmt.Errorf(
				"--erase needs a full factory image, but %s only publishes an application image for this board.\n"+
					"Re-run without --erase to update the application in place", req.Build.DeviceID)
		}
		return nil, fmt.Errorf("build has no flashable ESP32 images")
	}

	if len(app) > 0 {
		return app, nil
	}
	// Only a factory image exists. It is still the right thing to write; the
	// caller is warned that this wipes settings.
	if len(factory) > 0 {
		return factory, nil
	}
	return nil, fmt.Errorf("build has no flashable ESP32 images")
}

// verifyESP32 compares an MD5 of each written region against the local image.
func verifyESP32(f *espflasher.Flasher, req Request, images []espflasher.ImagePart, res *Result, log *slog.Logger) error {
	req.Progress.emit("verify", "Verifying written flash…", 0, 0)

	for _, img := range images {
		got, err := f.GetFlashMD5(img.Offset, uint32(len(img.Data)), nil)
		if err != nil {
			// Verification is best-effort: some ROM/stub combinations do not
			// implement the MD5 command. A failure to verify is not a failure
			// to flash, so record it and move on.
			log.Warn("could not verify region", "offset", fmt.Sprintf("0x%X", img.Offset), "error", err)
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("could not verify the region at 0x%X: %v", img.Offset, err))
			continue
		}
		want := md5Hex(img.Data)
		if got != want {
			return fmt.Errorf("verification failed at 0x%X: device has %s, image is %s", img.Offset, got, want)
		}
		log.Debug("region verified", "offset", fmt.Sprintf("0x%X", img.Offset), "md5", got)
	}
	return nil
}

// explainESPError turns the common connect failure into an actionable message.
// "Failed to connect" is the single most reported problem with these boards and
// almost always means one of three concrete things.
func explainESPError(err error) error {
	var syncErr *espflasher.SyncError
	if !asError(err, &syncErr) {
		return err
	}
	return fmt.Errorf("%w\n\nThe chip did not respond to the bootloader handshake. Usually one of:\n"+
		"  • another program is holding the port (a serial monitor, or the Meshtastic app)\n"+
		"  • the board needs its BOOT button held while you plug it in\n"+
		"  • the USB cable is charge-only — try a different cable\n"+
		"Run `meshflash doctor` to check drivers and permissions.", err)
}

// slogLogger adapts slog to espflasher's Logger interface.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Logf(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
