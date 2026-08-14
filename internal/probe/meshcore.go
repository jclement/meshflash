package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

// MeshCoreInfo is what a running MeshCore node reports about itself.
type MeshCoreInfo struct {
	// Board is the model as the firmware names it, e.g. "Heltec T114".
	Board string
	// Version is the firmware version, e.g. "v1.17.1-d929643 (Build: ...)".
	Version string
	// Role is the firmware variant: repeater, companion, room server.
	Role string
	// Name is the operator-assigned node name.
	Name string
}

func (i MeshCoreInfo) String() string {
	parts := []string{"MeshCore"}
	if i.Version != "" {
		parts = append(parts, i.Version)
	}
	if i.Board != "" {
		parts = append(parts, "on "+i.Board)
	}
	if i.Role != "" {
		parts = append(parts, "("+i.Role+")")
	}
	return strings.Join(parts, " ")
}

// ShortVersion strips the build suffix, so "v1.17.1-d929643 (Build: …)"
// becomes "1.17.1" — the form the catalog uses.
func (i MeshCoreInfo) ShortVersion() string {
	v := strings.TrimPrefix(strings.TrimSpace(i.Version), "v")
	if idx := strings.IndexAny(v, " -"); idx > 0 {
		v = v[:idx]
	}
	return v
}

// ErrNotMeshCore means the device did not answer the CLI.
var ErrNotMeshCore = errors.New("no MeshCore response")

// meshCoreQueries are the read-only commands the probe issues.
//
// Strictly read-only, and deliberately a fixed list. The same CLI accepts
// commands with real side effects — `advert` transmits a packet on the mesh —
// so this must never become a general command runner reachable from
// identification.
var meshCoreQueries = []struct {
	cmd    string
	assign func(*MeshCoreInfo, string)
}{
	{"version", func(i *MeshCoreInfo, v string) { i.Version = v }},
	{"board", func(i *MeshCoreInfo, v string) { i.Board = v }},
	{"get role", func(i *MeshCoreInfo, v string) { i.Role = v }},
	{"get name", func(i *MeshCoreInfo, v string) { i.Name = v }},
}

// MeshCore asks a device for its identity over the serial CLI.
//
// Which MeshCore builds answer anything at all over serial, established on a
// Heltec T114:
//
//   - repeater: answers this CLI. Framed binary commands come back echoed
//     byte for byte, so the companion protocol is not served.
//   - companion_radio_ble: answers nothing. No CLI, no binary protocol, no
//     unprompted output, and not even an echo — silent under every
//     DTR/RTS combination. Its USB CDC is inert; it talks over BLE only.
//   - companion_radio_usb: expected to serve the binary companion protocol,
//     which is what that build exists for. Untested.
//
// So serial identification covers some MeshCore builds and not others, and a
// silent port is a legitimate answer rather than a fault. The gap is covered
// by bindings: a board meshflash has flashed is recognised by its USB serial
// number without needing to ask it anything.
func MeshCore(ctx context.Context, opts Options) (*MeshCoreInfo, error) {
	opts.applyDefaults()

	port, err := serial.Open(opts.Port, &serial.Mode{BaudRate: opts.BaudRate})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.Port, err)
	}
	defer port.Close()

	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		return nil, err
	}

	// Terminate anything already sitting half-typed in the CLI's line buffer.
	// A previous probe — Meshtastic's wake burst, say — leaves bytes with no
	// newline, and the CLI would otherwise glue them onto our first command
	// and reject the pair.
	_, _ = port.Write([]byte("\r\n"))
	time.Sleep(120 * time.Millisecond)
	_ = port.ResetInputBuffer()

	deadline := time.Now().Add(opts.Timeout)
	var info MeshCoreInfo
	answered := false

	for _, q := range meshCoreQueries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}

		reply, err := meshCoreAsk(port, q.cmd, time.Until(deadline))
		if err != nil || reply == "" {
			continue
		}
		// The CLI answers unrecognised input rather than staying silent, so a
		// reply is not by itself proof that this is MeshCore.
		if isMeshCoreRejection(reply) {
			continue
		}
		q.assign(&info, reply)
		answered = true

		// `version` runs first and is the confirmation that this is MeshCore.
		// Bailing here keeps the probe cheap on a device that is not.
		if info.Version == "" {
			return nil, ErrNotMeshCore
		}
	}

	// The version string is the one field that confirms what we are talking to.
	if !answered || info.Version == "" {
		return nil, ErrNotMeshCore
	}
	return &info, nil
}

// meshCoreAsk sends one command and returns the value from its reply.
func meshCoreAsk(port serial.Port, cmd string, budget time.Duration) (string, error) {
	if budget <= 0 {
		return "", context.DeadlineExceeded
	}
	_ = port.ResetInputBuffer()

	if _, err := port.Write([]byte(cmd + "\r\n")); err != nil {
		return "", err
	}

	// The CLI echoes the command before answering, so read until a reply line
	// appears rather than taking the first thing that arrives.
	deadline := time.Now().Add(min(budget, 1500*time.Millisecond))
	var out []byte
	buf := make([]byte, 512)

	for time.Now().Before(deadline) {
		n, _ := port.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			if v, ok := parseMeshCoreReply(string(out)); ok {
				return v, nil
			}
		}
	}
	if v, ok := parseMeshCoreReply(string(out)); ok {
		return v, nil
	}
	return "", nil
}

// parseMeshCoreReply pulls the value out of a CLI response.
//
// Replies look like:
//
//	version\r\n  -> v1.17.1-d929643 (Build: 14-Aug-2026)\r\n
//	get role\r\n  -> > repeater\r\n
//
// The echoed command comes first, then an arrow-prefixed line; `get` queries
// add a second "> " marker.
func parseMeshCoreReply(s string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		rest, found := strings.CutPrefix(line, "->")
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ">"))
		if rest == "" {
			continue
		}
		return rest, true
	}
	return "", false
}

// isMeshCoreRejection reports whether the CLI turned the command down, which
// it does verbosely rather than by staying quiet.
func isMeshCoreRejection(reply string) bool {
	l := strings.ToLower(reply)
	return strings.HasPrefix(l, "unknown command") || strings.HasPrefix(l, "??")
}
