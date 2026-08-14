package probe

import (
	"encoding/binary"
	"testing"
)

// Replies captured verbatim from a Heltec T114 running MeshCore 1.17.1.
func TestParseMeshCoreReply(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"version", "version\r\n  -> v1.17.1-d929643 (Build: 14-Aug-2026)\r\n",
			"v1.17.1-d929643 (Build: 14-Aug-2026)", true},
		{"board", "board\r\n  -> Heltec T114\r\n", "Heltec T114", true},
		// `get` queries add a second marker before the value.
		{"get role", "get role\r\n  -> > repeater\r\n", "repeater", true},
		{"get name", "get name\r\n  -> > Jeff\r\n", "Jeff", true},
		{"rejection", "ver\r\n  -> Unknown command\r\n", "Unknown command", true},
		{"echo only", "version\r\n", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := parseMeshCoreReply(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// The CLI answers unrecognised input rather than staying silent, so a reply
// alone is not evidence the device is MeshCore.
func TestIsMeshCoreRejection(t *testing.T) {
	for _, s := range []string{"Unknown command", "unknown command", "??: ver", "??: bat"} {
		if !isMeshCoreRejection(s) {
			t.Errorf("%q should be treated as a rejection", s)
		}
	}
	for _, s := range []string{"Heltec T114", "repeater", "v1.17.1-d929643"} {
		if isMeshCoreRejection(s) {
			t.Errorf("%q is a real answer, not a rejection", s)
		}
	}
}

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"v1.17.1-d929643 (Build: 14-Aug-2026)": "1.17.1",
		"v1.17.1":                              "1.17.1",
		"1.17.1":                               "1.17.1",
		"":                                     "",
	}
	for in, want := range cases {
		if got := (MeshCoreInfo{Version: in}).ShortVersion(); got != want {
			t.Errorf("ShortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// A companion build cannot be resolved to its BLE or USB variant from the CLI,
// and guessing would flash the wrong firmware.
func TestNormaliseRole(t *testing.T) {
	cases := map[string]string{
		"repeater":    "repeater",
		"Repeater":    "repeater",
		"room server": "room_server",
		"companion":   "",
		"":            "",
	}
	for in, want := range cases {
		if got := normaliseRole(in); got != want {
			t.Errorf("normaliseRole(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Meshtastic stream framing -------------------------------------------

func TestExtractFrame(t *testing.T) {
	payload := []byte{0x18, 0xE6, 0xDA, 0x01}
	frame := append([]byte{start1, start2, 0x00, byte(len(payload))}, payload...)

	got, rest, ok := extractFrame(frame)
	if !ok || string(got) != string(payload) || len(rest) != 0 {
		t.Fatalf("clean frame: got %X, rest %X, ok %v", got, rest, ok)
	}

	// Devices interleave plain-text logging on the same port, so the reader
	// has to resynchronise rather than give up.
	noisy := append([]byte("INFO some log line\r\n"), frame...)
	got, _, ok = extractFrame(noisy)
	if !ok || string(got) != string(payload) {
		t.Errorf("did not resynchronise past log output: %X, %v", got, ok)
	}

	// A partial frame must be retained, not consumed.
	_, rest, ok = extractFrame(frame[:6])
	if ok {
		t.Error("accepted an incomplete frame")
	}
	if len(rest) == 0 {
		t.Error("discarded a partial frame instead of keeping it for the next read")
	}

	// A bogus length must not be mistaken for a header.
	bogus := []byte{start1, start2, 0xFF, 0xFF, 0x00}
	if _, _, ok := extractFrame(bogus); ok {
		t.Error("accepted a frame claiming an impossible length")
	}
}

func TestProtobufRoundTrip(t *testing.T) {
	// ToRadio{want_config_id: 0x6D66} — the exact bytes the probe sends.
	got := appendVarintField(nil, toRadioWantConfigID, 0x6D66)
	want := []byte{0x18, 0xE6, 0xDA, 0x01}
	if string(got) != string(want) {
		t.Fatalf("encoded % X, want % X", got, want)
	}

	f, rest, err := nextField(got)
	if err != nil {
		t.Fatal(err)
	}
	if f.num != toRadioWantConfigID || f.wire != wireVarint || f.varint != 0x6D66 {
		t.Errorf("decoded %+v", f)
	}
	if len(rest) != 0 {
		t.Errorf("trailing bytes % X", rest)
	}
}

func TestParseMetadata(t *testing.T) {
	// DeviceMetadata{firmware_version: "2.7.26.54e0d8d", hw_model: 82}
	var b []byte
	version := "2.7.26.54e0d8d"
	b = binary.AppendUvarint(b, uint64(metadataFirmwareVersion)<<3|wireBytes)
	b = binary.AppendUvarint(b, uint64(len(version)))
	b = append(b, version...)
	b = appendVarintField(b, metadataHWModel, 82)
	// An unknown field must be skipped, not fatal — the schema keeps growing.
	b = appendVarintField(b, 99, 12345)

	var info MeshtasticInfo
	parseMetadata(b, &info)

	if info.FirmwareVersion != version {
		t.Errorf("firmware_version = %q, want %q", info.FirmwareVersion, version)
	}
	if info.HWModel != 82 {
		t.Errorf("hw_model = %d, want 82", info.HWModel)
	}
}

func TestNextFieldRejectsTruncated(t *testing.T) {
	// Length-delimited field claiming more bytes than are present.
	b := []byte{0x0A, 0x10, 0x01, 0x02}
	if _, _, err := nextField(b); err == nil {
		t.Error("accepted a truncated length-delimited field")
	}
}
