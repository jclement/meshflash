package flash

import (
	"context"
	"errors"
	"fmt"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/flash/nrfdfu"
)

// flashNRFDFU pushes a Nordic legacy DFU package over serial to an Adafruit
// nRF52 bootloader.
//
// Compared to the UF2 path this needs no mass-storage mount, which matters on
// locked-down machines and headless field units where automount is disabled.
func flashNRFDFU(ctx context.Context, req Request) (*Result, error) {
	if req.Target.Port == nil {
		return nil, fmt.Errorf("serial DFU needs a serial port, got %s", req.Target.Describe())
	}
	port := req.Target.Port.Name
	log := req.logger().With("port", port, "device", req.Device.ID)

	data, ok := req.payload(catalog.RolePackage)
	if !ok {
		return nil, errors.New("build has no DFU package artifact")
	}

	pkg, err := nrfdfu.OpenPackage(data)
	if err != nil {
		return nil, err
	}
	log.Info("DFU package loaded", "contents", pkg.Describe(), "dfu_version", pkg.DFUVersion)

	// Only touch when the device is still running the application. A port that
	// already reports the bootloader's USB ID is in DFU mode, and touching it
	// again would reboot it back into the application.
	touchBaud := 0
	alreadyInBootloader := device.LooksLikeNRF52Bootloader(*req.Target.Port)
	switch {
	case alreadyInBootloader:
		log.Info("device is already in its DFU bootloader")
		req.Progress.emit("bootloader", "Device is already in DFU mode.", 0, 0)
	case req.AutoBootloader:
		touchBaud = device.TouchBaud
		req.Progress.emit("bootloader", "Rebooting the device into DFU mode…", 0, 0)
	default:
		return nil, errors.New(
			"device is not in DFU mode. Double-tap the reset button, or re-run without --no-auto-bootloader")
	}

	total := int64(pkg.TotalBytes())
	req.Progress.emit("connect", "Opening DFU session…", 0, total)

	session, err := nrfdfu.Open(nrfdfu.Options{
		Port:      port,
		TouchBaud: touchBaud,
		Logger:    log,
		Progress: func(sent, t int) {
			req.Progress.emit("write", "Sending firmware over DFU…", int64(sent), int64(t))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("open DFU session on %s: %w", port, err)
	}
	defer session.Close()

	if err := session.Program(ctx, pkg); err != nil {
		if errors.Is(err, nrfdfu.ErrNoAck) {
			return nil, fmt.Errorf("%w\n\nThe bootloader never acknowledged a packet. Usually one of:\n"+
				"  • the board is not actually in DFU mode — double-tap reset and try again\n"+
				"  • another program has the port open\n"+
				"  • this board uses a UF2 bootloader instead; try the uf2 build for it", err)
		}
		return nil, err
	}

	req.Progress.emit("activate", "Firmware sent; the board is applying it…", total, total)
	log.Info("DFU complete", "bytes", total)

	return &Result{BytesWritten: total, Chip: "nRF52"}, nil
}
