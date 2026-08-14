// Package flash writes firmware to a connected device.
//
// Three transports are supported behind one entry point, because the boards
// Meshtastic and MeshCore target split cleanly three ways:
//
//   - ESP32 family: raw images at fixed offsets over the ROM serial bootloader.
//   - UF2 bootloaders (nRF52840, RP2040/RP2350): copy a .uf2 onto a mass
//     storage volume.
//   - nRF52 serial DFU: a Nordic legacy DFU package pushed over the wire.
//
// Callers hand in a Request with the firmware bytes already resolved from the
// store, so this package never touches the network.
package flash

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
)

// Progress reports flashing progress to the UI.
type Progress struct {
	// Stage is a short machine-readable phase: connect, erase, write, verify,
	// bootloader, activate.
	Stage string
	// Message is a human sentence describing what is happening.
	Message string
	// Current and Total are bytes where meaningful, and 0 otherwise.
	Current int64
	Total   int64
}

// ProgressFunc receives Progress updates.
type ProgressFunc func(Progress)

func (f ProgressFunc) emit(stage, msg string, cur, total int64) {
	if f != nil {
		f(Progress{Stage: stage, Message: msg, Current: cur, Total: total})
	}
}

// Request describes one flash operation.
type Request struct {
	// Target is the port or bootloader volume to write to.
	Target device.Target
	// Device is the catalog entry the operator selected.
	Device catalog.Device
	// Build is the firmware build to write.
	Build catalog.Build
	// Payloads maps each artifact to its bytes, resolved from the store.
	Payloads map[string][]byte

	// Erase requests a full chip erase before writing (ESP32 only).
	//
	// This wipes NVS along with everything else. On Meshtastic that destroys
	// the node's private key: it comes back with a new identity, remote admin
	// stops working and PKC direct messages to it fail until peers re-learn
	// the key. Never default this on.
	Erase bool

	// Verify re-reads flash and compares checksums after writing.
	Verify bool

	// AutoBootloader allows meshflash to reboot the device into its
	// bootloader automatically (1200-baud touch) instead of asking the
	// operator for a double-tap reset.
	AutoBootloader bool

	// ExperimentalSerialDFU allows converting a .uf2 to a raw image and
	// pushing it over serial DFU when the bootloader comes up without mass
	// storage.
	//
	// Off by default. The conversion and the synthesised init packet are
	// verified against real firmware and against the bootloader's own
	// validation source, but no such transfer has been observed to complete on
	// hardware, and a board needed recovering while this path was in play.
	// Until it is proven, the safe answer for a serial-only bootloader is to
	// ask for a double-tap reset, which yields mass storage and a flash method
	// that is known to work.
	ExperimentalSerialDFU bool

	Logger   *slog.Logger
	Progress ProgressFunc

	// OnManualPrompt is invoked when automatic bootloader entry fails and the
	// operator needs to intervene.
	OnManualPrompt func(message string)
}

func (r *Request) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(nopHandler{})
	}
	return r.Logger
}

// payload returns the bytes for the first artifact with the given role.
func (r *Request) payload(role catalog.Role) ([]byte, bool) {
	for _, a := range r.Build.Artifacts {
		if a.Role == role {
			b, ok := r.Payloads[a.Name]
			return b, ok
		}
	}
	return nil, false
}

// Result summarises a completed flash.
type Result struct {
	// Chip is what the transport reported, e.g. "ESP32-S3". Empty for UF2.
	Chip string
	// BytesWritten counts firmware payload, excluding protocol overhead.
	BytesWritten int64
	Duration     time.Duration
	// Warnings are non-fatal issues worth surfacing after success.
	Warnings []string
}

// Flash writes firmware to a device, dispatching on the build's method.
func Flash(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()

	if len(req.Build.Artifacts) == 0 {
		return nil, fmt.Errorf("build for %s has no artifacts", req.Device.ID)
	}

	var (
		res *Result
		err error
	)
	switch req.Build.Method {
	case catalog.MethodESP32:
		res, err = flashESP32(ctx, req)
	case catalog.MethodUF2:
		res, err = flashUF2(ctx, req)
	case catalog.MethodNRFDFU:
		res, err = flashNRFDFU(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported flash method %q", req.Build.Method)
	}
	if err != nil {
		return nil, err
	}

	res.Duration = time.Since(start)
	return res, nil
}

// orderedArtifacts returns ESP32 artifacts sorted by flash offset, which is
// the order esptool writes them and the order that reads sensibly in a log.
func orderedArtifacts(b catalog.Build) []catalog.Artifact {
	out := append([]catalog.Artifact(nil), b.Artifacts...)
	sort.SliceStable(out, func(i, j int) bool {
		var oi, oj uint32
		if out[i].Offset != nil {
			oi = *out[i].Offset
		}
		if out[j].Offset != nil {
			oj = *out[j].Offset
		}
		return oi < oj
	})
	return out
}

type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (nopHandler) WithAttrs([]slog.Attr) slog.Handler        { return nopHandler{} }
func (nopHandler) WithGroup(string) slog.Handler             { return nopHandler{} }
