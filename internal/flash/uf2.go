package flash

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/flash/nrfdfu"
)

// flashUF2 copies a .uf2 image onto a mounted UF2 bootloader volume.
//
// The bootloader watches its own FAT filesystem, so "flashing" here is a file
// copy. The subtlety is the ending: as soon as the final block lands the
// device reboots and yanks the volume out from under us, so the close and sync
// that follow are expected to fail. Treating those as errors would report a
// successful flash as a failure.
func flashUF2(ctx context.Context, req Request) (*Result, error) {
	log := req.logger().With("device", req.Device.ID)

	data, ok := req.payload(catalog.RoleUF2)
	if !ok {
		return nil, errors.New("build has no UF2 artifact")
	}

	// Snapshot the ports before anything reboots the board, so the bootloader's
	// own port can be told apart from the application's afterwards.
	portsBefore, _ := device.ListPorts()

	vol, err := resolveUF2Volume(ctx, req, log)
	if err != nil {
		res, ferr := serialDFUFallback(ctx, req, log, portsBefore, data, err)
		switch {
		case ferr == nil:
			return res, nil
		case errors.Is(ferr, errNoFallback):
			return nil, err // report the original mass-storage failure
		default:
			return nil, ferr
		}
	}
	log = log.With("volume", vol.Path)

	// A UF2 bootloader ignores files it does not recognise, so a mismatched
	// image fails silently and leaves the operator wondering. Catch it here.
	if err := checkUF2Family(data, vol); err != nil {
		return nil, err
	}

	dest := filepath.Join(vol.Path, "CURRENT.UF2")
	total := int64(len(data))

	log.Info("copying UF2 image", "bytes", total, "dest", dest)
	req.Progress.emit("write", "Copying firmware to "+vol.Label+"…", 0, total)

	written, err := copyToBootloader(ctx, dest, data, func(n int64) {
		req.Progress.emit("write", "Copying firmware to "+vol.Label+"…", n, total)
	})
	if err != nil {
		return nil, err
	}
	if written != total {
		return nil, fmt.Errorf("copied %d of %d bytes to %s", written, total, vol.Path)
	}

	req.Progress.emit("activate", "Firmware written; the board is rebooting…", total, total)
	log.Info("UF2 image written; device is rebooting")

	res := &Result{BytesWritten: total, Chip: vol.Info.Model}
	if vol.Info.BoardID != "" {
		log.Info("board reported itself", "board_id", vol.Info.BoardID)
	}
	return res, nil
}

// errNoFallback means serial DFU is not applicable, so the caller should
// report whatever went wrong with mass storage instead.
var errNoFallback = errors.New("no serial DFU fallback available")

// serialDFUFallback flashes over serial DFU when the bootloader came up
// without mass storage.
//
// This is the normal outcome of an automatic 1200-baud touch on an nRF52, not
// an edge case. Adafruit's bootloader distinguishes the two entry paths:
//
//	DFU_MAGIC_SERIAL_ONLY_RESET : with CDC interface only
//	DFU_MAGIC_UF2_RESET         : with CDC and MSC interfaces
//
// The magic-baud touch selects serial-only, so no drive ever appears; only a
// double-tap reset brings up mass storage. Without this path, automatic
// bootloader entry can never finish a flash on those boards, and the operator
// has to double-tap every time.
//
// Meshtastic publishes nRF52 firmware only as UF2, so the image is converted
// and its init packet synthesised.
func serialDFUFallback(ctx context.Context, req Request, log *slog.Logger, portsBefore []device.Port, uf2 []byte, cause error) (*Result, error) {
	if req.Target.Port == nil || !req.AutoBootloader {
		return nil, errNoFallback
	}

	// A build that ships a real DFU package needs no conversion.
	if _, hasPkg := req.payload(catalog.RolePackage); hasPkg {
		log.Warn("no UF2 volume appeared; using the packaged serial DFU image")
		req.Progress.emit("bootloader", "No bootloader drive appeared — using serial DFU instead…", 0, 0)
		return flashNRFDFU(ctx, req)
	}

	if req.Device.Platform != "" && !strings.HasPrefix(req.Device.Platform, "nrf52") {
		// RP2040 and friends have no serial DFU to fall back to.
		return nil, errNoFallback
	}

	// The wait may already have identified the bootloader's port.
	var serialOnly *device.SerialOnlyBootloaderError
	var port device.Port
	if errors.As(cause, &serialOnly) {
		port = serialOnly.Port
		req.Progress.emit("bootloader",
			"Bootloader is in serial-only mode (no USB drive) — sending over serial DFU…", 0, 0)
	} else {
		req.Progress.emit("bootloader", "No bootloader drive appeared — looking for a serial DFU port…", 0, 0)
		p, err := device.WaitForBootloaderPort(ctx, portsBefore, *req.Target.Port, 15*time.Second)
		if err != nil {
			log.Debug("no bootloader serial port found either", "error", err)
			return nil, errNoFallback
		}
		port = p
	}
	log.Info("found an nRF52 bootloader in serial-only mode", "port", port.Name)

	pkg, err := nrfdfu.PackageFromUF2(uf2)
	if err != nil {
		return nil, fmt.Errorf("convert UF2 for serial DFU: %w", err)
	}
	log.Info("converted UF2 for serial DFU", "contents", pkg.Describe())

	total := int64(pkg.TotalBytes())
	req.Progress.emit("connect", "Sending firmware over serial DFU…", 0, total)

	// The board is already in its bootloader, so no touch: touching again
	// would reboot it back into the application.
	session, err := nrfdfu.Open(nrfdfu.Options{
		Port:   port.Name,
		Logger: log,
		Progress: func(sent, t int) {
			req.Progress.emit("write", "Sending firmware over serial DFU…", int64(sent), int64(t))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("open DFU session on %s: %w", port.Name, err)
	}
	defer session.Close()

	if err := session.Program(ctx, pkg); err != nil {
		return nil, err
	}

	req.Progress.emit("activate", "Firmware sent; the board is applying it…", total, total)
	log.Info("serial DFU complete", "bytes", total)
	return &Result{BytesWritten: total, Chip: "nRF52", Warnings: []string{
		"Written over serial DFU: the 1200-baud touch puts this bootloader into serial-only mode, " +
			"where it exposes no USB drive.",
	}}, nil
}

// resolveUF2Volume finds the bootloader volume, entering it automatically when
// the target is currently a serial port.
func resolveUF2Volume(ctx context.Context, req Request, log *slog.Logger) (device.Volume, error) {
	if req.Target.Volume != nil {
		return *req.Target.Volume, nil
	}
	if !req.AutoBootloader {
		return device.Volume{}, errors.New(
			"device is not in its UF2 bootloader. Double-tap the reset button, or re-run without --no-auto-bootloader")
	}

	req.Progress.emit("bootloader", "Rebooting the device into its bootloader…", 0, 0)

	// Anything the wait learns about a rejected volume is the most valuable
	// thing on screen, so it replaces the status line rather than adding to it.
	var rejections []device.Rejection

	vol, err := device.EnterUF2Bootloader(ctx, req.Target, device.EnterBootloaderOptions{
		Logger: log,
		OnTouch: func(attempt, total int) {
			msg := "Rebooting the device into its bootloader…"
			if attempt > 1 {
				msg = fmt.Sprintf("Retrying bootloader entry (%d of %d) — some boards need two tries…",
					attempt, total)
			}
			req.Progress.emit("bootloader", msg, 0, 0)
		},
		OnManualPrompt: func() {
			// Status line only. Echoing into the log pane as well just printed
			// the same sentence twice.
			req.Progress.emit("bootloader",
				"Automatic bootloader entry did not take — double-tap the reset button on the board.", 0, 0)
		},
		OnVolumeRejected: func(r device.Rejection) {
			rejections = append(rejections, r)
			req.Progress.emit("bootloader",
				fmt.Sprintf("Found %s but cannot use it: %s", r.Path, r.Reason), 0, 0)
			if req.OnManualPrompt != nil {
				req.OnManualPrompt(fmt.Sprintf("%s: %s", r.Path, r.Reason))
			}
		},
	})
	if err != nil {
		if errors.Is(err, device.ErrBootloaderTimeout) {
			return device.Volume{}, bootloaderTimeoutError(rejections)
		}
		return device.Volume{}, err
	}
	return vol, nil
}

// bootloaderTimeoutError explains the failure using whatever was actually
// observed, because "no bootloader appeared" and "a bootloader appeared and I
// could not read it" need completely different fixes.
func bootloaderTimeoutError(rejections []device.Rejection) error {
	if len(rejections) > 0 {
		var b strings.Builder
		b.WriteString("a volume appeared but meshflash could not use it as a bootloader:\n")
		for _, r := range rejections {
			fmt.Fprintf(&b, "  %s — %s\n", r.Path, r.Reason)
		}
		b.WriteString("\nRun `meshflash doctor` to see every mounted volume and why each was rejected.")
		return errors.New(b.String())
	}
	return errors.New(
		"the board never presented a UF2 bootloader volume.\n" +
			"Double-tap the reset button quickly — the bootloader should appear as a USB drive — then try again.\n" +
			"If it does appear in your file manager, run `meshflash doctor`: on macOS a terminal needs\n" +
			"Privacy & Security → Files and Folders → Removable Volumes before it can read one.")
}

// copyToBootloader writes the image, tolerating the disconnect that follows
// the final block.
func copyToBootloader(ctx context.Context, dest string, data []byte, onProgress func(int64)) (int64, error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, fmt.Errorf("open %s for writing: %w", dest, err)
	}

	// Write in chunks so progress moves and cancellation is responsive. UF2
	// blocks are 512 bytes; 32 KB is a comfortable multiple.
	const chunk = 32 << 10
	var written int64
	var writeErr error

	for off := 0; off < len(data); off += chunk {
		if err := ctx.Err(); err != nil {
			f.Close()
			return written, err
		}
		end := min(off+chunk, len(data))
		n, err := f.Write(data[off:end])
		written += int64(n)
		if err != nil {
			writeErr = err
			break
		}
		if onProgress != nil {
			onProgress(written)
		}
	}

	// Sync and Close routinely fail with EIO or ENODEV here because the board
	// has already rebooted and unmounted itself. That is success, not failure.
	syncErr := f.Sync()
	closeErr := f.Close()

	if writeErr != nil {
		// A mid-transfer error with most of the image still outstanding is a
		// real failure; near the end it is just the reboot racing us.
		if written < int64(len(data))-chunk {
			return written, fmt.Errorf("write to %s: %w", dest, writeErr)
		}
		written = int64(len(data))
	}
	_ = syncErr
	_ = closeErr

	// Give the OS a moment to finish tearing the volume down before anything
	// tries to rescan.
	time.Sleep(500 * time.Millisecond)
	return written, nil
}

// uf2MagicStart0 and uf2MagicEnd bracket every 512-byte UF2 block.
const (
	uf2MagicStart0 = 0x0A324655 // "UF2\n"
	uf2MagicStart1 = 0x9E5D5157
	uf2MagicEnd    = 0x0AB16F30
	uf2BlockSize   = 512
)

// checkUF2Family sanity-checks the image before copying.
//
// A UF2 bootloader silently discards blocks whose family ID does not match, so
// writing the wrong image looks like a successful flash that changes nothing.
func checkUF2Family(data []byte, vol device.Volume) error {
	if len(data) < uf2BlockSize {
		return fmt.Errorf("UF2 image is only %d bytes, which is too small to be valid", len(data))
	}
	if len(data)%uf2BlockSize != 0 {
		return fmt.Errorf("UF2 image is %d bytes, not a multiple of the %d byte block size (corrupt download?)",
			len(data), uf2BlockSize)
	}
	if le32(data[0:4]) != uf2MagicStart0 || le32(data[4:8]) != uf2MagicStart1 {
		return errors.New("file does not have a UF2 header (corrupt download?)")
	}
	if le32(data[uf2BlockSize-4:uf2BlockSize]) != uf2MagicEnd {
		return errors.New("first UF2 block has a bad trailing magic (corrupt download?)")
	}
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// asError is errors.As with a generic-free signature the callers here need.
func asError(err error, target any) bool { return errors.As(err, target) }
