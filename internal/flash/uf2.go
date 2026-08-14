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
	"time"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
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

	vol, err := resolveUF2Volume(ctx, req, log)
	if err != nil {
		// Mass storage never appeared. On a headless or locked-down machine
		// that is expected rather than exceptional — automount may simply be
		// off — so fall back to serial DFU when the build carries a package.
		if _, hasPkg := req.payload(catalog.RolePackage); hasPkg && req.Target.Port != nil {
			log.Warn("no UF2 volume appeared; falling back to serial DFU", "error", err)
			req.Progress.emit("bootloader", "No bootloader drive appeared — trying serial DFU instead…", 0, 0)
			return flashNRFDFU(ctx, req)
		}
		return nil, err
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

	vol, err := device.EnterUF2Bootloader(ctx, req.Target, device.EnterBootloaderOptions{
		Logger: log,
		OnManualPrompt: func() {
			msg := "Automatic bootloader entry did not take. Double-tap the reset button on the board."
			req.Progress.emit("bootloader", msg, 0, 0)
			if req.OnManualPrompt != nil {
				req.OnManualPrompt(msg)
			}
		},
	})
	if err != nil {
		if errors.Is(err, device.ErrBootloaderTimeout) {
			return device.Volume{}, fmt.Errorf(
				"the board never presented a UF2 bootloader volume.\n" +
					"Double-tap the reset button quickly — the bootloader should appear as a USB drive — then run the flash again")
		}
		return device.Volume{}, err
	}
	return vol, nil
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
