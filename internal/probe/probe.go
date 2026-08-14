package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Firmware identifies what a device is running.
type Firmware struct {
	// Project is "meshtastic" or "meshcore", matching a catalog project id.
	Project string
	// Version is the firmware version in the form the catalog uses.
	Version string
	// Variant is the MeshCore role, empty for Meshtastic.
	Variant string
	// Board is the model as the firmware names it, when it says.
	Board string
	// NodeName is the operator-assigned name, when the firmware exposes it.
	NodeName string

	// Meshtastic and MeshCore carry the full reply when that project answered.
	Meshtastic *MeshtasticInfo
	MeshCore   *MeshCoreInfo
}

func (f Firmware) String() string {
	parts := []string{f.Project}
	if f.Version != "" {
		parts = append(parts, f.Version)
	}
	if f.Variant != "" {
		parts = append(parts, "("+f.Variant+")")
	}
	return strings.Join(parts, " ")
}

// ErrUnknownFirmware means neither project answered.
var ErrUnknownFirmware = errors.New("device did not identify its firmware")

// Identify asks a device what it is running.
//
// Both probes are read-only: Meshtastic's asks for config over its stream API,
// MeshCore's issues a fixed set of query commands. Neither writes flash,
// changes settings, or transmits on the mesh.
//
// MeshCore is tried first because it settles in milliseconds and its plain
// text is inert to Meshtastic, which resynchronises on framed magic bytes and
// ignores everything else. The reverse order is worse in both directions:
// Meshtastic's probe waits out its whole timeout on a device that is not
// running it, and its wake burst leaves an unterminated line in the MeshCore
// CLI that swallows the next command.
func Identify(ctx context.Context, opts Options) (*Firmware, error) {
	opts.applyDefaults()

	if mc, err := MeshCore(ctx, opts); err == nil {
		return &Firmware{
			Project:  "meshcore",
			Version:  mc.ShortVersion(),
			Variant:  normaliseRole(mc.Role),
			Board:    mc.Board,
			NodeName: mc.Name,
			MeshCore: mc,
		}, nil
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if mt, err := Meshtastic(ctx, opts); err == nil {
		return &Firmware{
			Project:    "meshtastic",
			Version:    mt.FirmwareVersion,
			Meshtastic: mt,
		}, nil
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return nil, fmt.Errorf("%w on %s", ErrUnknownFirmware, opts.Port)
}

// normaliseRole maps what the CLI reports onto the catalog's variant names.
//
// The CLI says "repeater"; the catalog calls that build "repeater" too, but
// companion and room server differ, and the CLI cannot distinguish the BLE and
// USB companion builds — both simply report "companion". That ambiguity is
// left in place rather than guessed at: a wrong variant flashes the wrong
// firmware.
func normaliseRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "repeater":
		return "repeater"
	case "room server", "roomserver", "room_server":
		return "room_server"
	default:
		// "companion" cannot be resolved to companion_radio_ble or
		// companion_radio_usb from here.
		return ""
	}
}
