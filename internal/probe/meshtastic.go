// Package probe asks a running device what it is.
//
// Every other identification signal meshflash has is indirect — a USB product
// string, a bootloader's Board-ID, a VID/PID that names the UART bridge. This
// package asks the firmware itself, which is the only source that can report
// both the board model and the firmware version actually installed.
package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

// Meshtastic's stream API framing: every message is preceded by two magic
// bytes and a big-endian 16-bit length. The magic exists because the device
// also writes plain-text debug logging to the same port, so a reader has to be
// able to resynchronise on the frame start.
const (
	start1        = 0x94
	start2        = 0xC3
	maxFrameBytes = 512
)

// Field numbers from meshtastic/mesh.proto. These are the stable contract.
const (
	toRadioWantConfigID = 3

	fromRadioMyInfo           = 3
	fromRadioConfigCompleteID = 7
	fromRadioMetadata         = 13

	metadataFirmwareVersion = 1
	metadataRole            = 7
	metadataHWModel         = 9

	myInfoNodeNum = 1
)

// MeshtasticInfo is what a running Meshtastic node reports about itself.
type MeshtasticInfo struct {
	// FirmwareVersion is the version string, e.g. "2.7.26.54e0d8d".
	FirmwareVersion string
	// HWModel is the HardwareModel enum value, which maps to a catalog device.
	HWModel uint32
	// Role is the device role enum.
	Role uint32
	// NodeNum is the node's mesh address, unique and stable.
	NodeNum uint32
}

func (i MeshtasticInfo) String() string {
	return fmt.Sprintf("Meshtastic %s (hw_model=%d)", i.FirmwareVersion, i.HWModel)
}

// ErrNoResponse means nothing recognisable came back, so the device is
// probably not running Meshtastic.
var ErrNoResponse = errors.New("no Meshtastic response")

// Options configures a probe.
type Options struct {
	Port string
	// Timeout bounds the whole exchange.
	Timeout time.Duration
	// BaudRate defaults to 115200.
	BaudRate int
}

func (o *Options) applyDefaults() {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if o.BaudRate == 0 {
		o.BaudRate = 115200
	}
}

// Meshtastic asks a device for its metadata over the serial stream API.
//
// This never writes to flash and never reboots the device: it opens the port,
// asks for the config, reads until the device says it is done, and closes.
func Meshtastic(ctx context.Context, opts Options) (*MeshtasticInfo, error) {
	opts.applyDefaults()

	port, err := serial.Open(opts.Port, &serial.Mode{BaudRate: opts.BaudRate})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.Port, err)
	}
	defer port.Close()

	if err := port.SetReadTimeout(250 * time.Millisecond); err != nil {
		return nil, err
	}

	// A burst of start bytes both wakes a sleeping device and resynchronises
	// its parser if it was mid-way through a partial frame. This mirrors what
	// the official clients do on connect.
	wake := make([]byte, 32)
	for i := range wake {
		wake[i] = start1
	}
	if _, err := port.Write(wake); err != nil {
		return nil, fmt.Errorf("wake %s: %w", opts.Port, err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = port.ResetInputBuffer()

	// want_config_id is an arbitrary non-zero nonce; the device echoes it back
	// in config_complete_id when it has finished streaming.
	const nonce = 0x6D66 // "mf"
	if err := writeFrame(port, appendVarintField(nil, toRadioWantConfigID, nonce)); err != nil {
		return nil, err
	}

	return readMetadata(ctx, port, opts.Timeout, nonce)
}

// readMetadata consumes frames until the device reports it is done.
func readMetadata(ctx context.Context, port serial.Port, timeout time.Duration, nonce uint64) (*MeshtasticInfo, error) {
	deadline := time.Now().Add(timeout)
	var (
		buf  []byte
		tmp  = make([]byte, 1024)
		info MeshtasticInfo
		got  bool
	)

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		n, err := port.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)

			for {
				payload, rest, ok := extractFrame(buf)
				if !ok {
					break
				}
				buf = rest

				done, err := parseFromRadio(payload, &info, &got, nonce)
				if err != nil {
					// A malformed frame is not fatal: the port carries debug
					// text too, and a resync will find the next one.
					continue
				}
				if done && got {
					return &info, nil
				}
			}
			// Bound the buffer so a device emitting only logging cannot grow it
			// without limit.
			if len(buf) > 8*maxFrameBytes {
				buf = buf[len(buf)-maxFrameBytes:]
			}
		}
		if err != nil && n == 0 {
			// Read timeout; keep waiting until the deadline.
			continue
		}
	}

	if got {
		return &info, nil
	}
	return nil, ErrNoResponse
}

// parseFromRadio pulls the fields of interest out of one FromRadio message and
// reports whether the device signalled that it has finished.
func parseFromRadio(payload []byte, info *MeshtasticInfo, got *bool, nonce uint64) (bool, error) {
	done := false
	err := walk(payload, func(f field) error {
		switch {
		case f.num == fromRadioMetadata && f.wire == wireBytes:
			parseMetadata(f.bytes, info)
			*got = true
		case f.num == fromRadioMyInfo && f.wire == wireBytes:
			_ = walk(f.bytes, func(g field) error {
				if g.num == myInfoNodeNum && g.wire == wireVarint {
					info.NodeNum = uint32(g.varint)
				}
				return nil
			})
		case f.num == fromRadioConfigCompleteID && f.wire == wireVarint:
			if f.varint == nonce {
				done = true
			}
		}
		return nil
	})
	return done, err
}

func parseMetadata(b []byte, info *MeshtasticInfo) {
	_ = walk(b, func(f field) error {
		switch {
		case f.num == metadataFirmwareVersion && f.wire == wireBytes:
			info.FirmwareVersion = strings.TrimSpace(string(f.bytes))
		case f.num == metadataHWModel && f.wire == wireVarint:
			info.HWModel = uint32(f.varint)
		case f.num == metadataRole && f.wire == wireVarint:
			info.Role = uint32(f.varint)
		}
		return nil
	})
}

// writeFrame wraps a payload in the stream API framing.
func writeFrame(port serial.Port, payload []byte) error {
	if len(payload) > maxFrameBytes {
		return fmt.Errorf("frame of %d bytes exceeds the %d byte limit", len(payload), maxFrameBytes)
	}
	frame := make([]byte, 0, len(payload)+4)
	frame = append(frame, start1, start2, byte(len(payload)>>8), byte(len(payload)))
	frame = append(frame, payload...)
	_, err := port.Write(frame)
	return err
}

// extractFrame finds the next complete frame, discarding anything before it.
//
// Returning the remainder rather than consuming in place is what lets the
// caller keep partial frames across reads, and skipping leading noise is what
// makes this work on a port that is also carrying debug logging.
func extractFrame(buf []byte) (payload, rest []byte, ok bool) {
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] != start1 || buf[i+1] != start2 {
			continue
		}
		if i+4 > len(buf) {
			return nil, buf[i:], false // header not fully arrived
		}
		length := int(buf[i+2])<<8 | int(buf[i+3])
		if length > maxFrameBytes {
			// Not a real header; keep scanning past it.
			continue
		}
		end := i + 4 + length
		if end > len(buf) {
			return nil, buf[i:], false // body not fully arrived
		}
		return buf[i+4 : end], buf[end:], true
	}

	// Keep the last byte in case it is the first half of a magic pair.
	if len(buf) > 0 {
		return nil, buf[len(buf)-1:], false
	}
	return nil, buf, false
}
