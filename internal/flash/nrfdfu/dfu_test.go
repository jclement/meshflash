package nrfdfu

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// The golden frames below were produced by running Adafruit_nRF52_nrfutil's
// own HciPacket/crc16 code over the same inputs. They pin the wire format so a
// refactor here cannot silently start emitting frames a bootloader will reject.

func TestCRC16(t *testing.T) {
	// "123456789" => 0x29B1 is the standard CRC-16/CCITT-FALSE check value,
	// which is the variant Nordic's legacy DFU uses.
	cases := []struct {
		in   string
		want uint16
	}{
		{"", 0xFFFF},
		{"123456789", 0x29B1},
		{"abc", 0x514A},
	}
	for _, c := range cases {
		if got := CRC16([]byte(c.in)); got != c.want {
			t.Errorf("CRC16(%q) = 0x%04X, want 0x%04X", c.in, got, c.want)
		}
	}
}

func TestCRC16SeedIsChunkable(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	whole := CRC16(data)
	part := CRC16Seed(data[:10], 0xFFFF)
	part = CRC16Seed(data[10:], part)
	if whole != part {
		t.Errorf("chunked CRC 0x%04X != whole 0x%04X", part, whole)
	}
}

func TestSlipHeader(t *testing.T) {
	// seq=1, payload length 4: reference output D1 4E 00 E1.
	got := slipHeader(1, 4)
	want := [4]byte{0xD1, 0x4E, 0x00, 0xE1}
	if got != want {
		t.Errorf("slipHeader(1,4) = % X, want % X", got, want)
	}

	// The fourth byte is a two's-complement checksum: the low byte of the
	// first four must sum to zero.
	for _, tc := range []struct{ seq, length int }{{1, 4}, {2, 20}, {7, 516}, {0, 0}} {
		h := slipHeader(uint8(tc.seq), tc.length)
		if sum := byte(h[0] + h[1] + h[2] + h[3]); sum != 0 {
			t.Errorf("slipHeader(%d,%d) checksum byte wrong: sum=%d", tc.seq, tc.length, sum)
		}
	}

	// Length is split across a nibble and a byte; check the 12-bit packing.
	h := slipHeader(1, 20)
	if h[1]&0xF0 != 4<<4 || h[2] != 1 {
		t.Errorf("length packing wrong for 20: h1=0x%02X h2=0x%02X", h[1], h[2])
	}
}

func TestBuildFrameGolden(t *testing.T) {
	tests := []struct {
		name    string
		seq     uint8
		payload []byte
		want    string
	}{
		{
			name:    "stop data packet",
			seq:     1,
			payload: le32(opStopDataPacket),
			want:    "C0D14E00E10500000074 82C0",
		},
		{
			name: "start packet for a 464616 byte application",
			seq:  1,
			payload: concat(
				le32(opStartPacket),
				le32(ModeApplication),
				le32(0), le32(0), le32(464616),
			),
			want: "C0D14E01E003000000040000000000000000000000E816070016C6C0",
		},
		{
			name: "init packet from a real MeshCore package",
			seq:  2,
			payload: concat(
				le32(opInitPacket),
				mustHex(t, "5200ffffffffffff0100b6003950"),
				[]byte{0x00, 0x00},
			),
			want: "C0DA4E01D70100000052 00FFFFFFFFFFFF0100B60039500000 3AACC0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFrame(tc.seq, tc.payload)
			if err != nil {
				t.Fatalf("buildFrame: %v", err)
			}
			want := mustHex(t, tc.want)
			if !bytes.Equal(got, want) {
				t.Errorf("frame mismatch\n got: % X\nwant: % X", got, want)
			}
		})
	}
}

func TestBuildFrameRejectsOversizePayload(t *testing.T) {
	if _, err := buildFrame(1, make([]byte, 0x1000)); err == nil {
		t.Fatal("expected an error for a payload beyond the 12-bit length field")
	}
}

func TestSlipEscapeRoundTrip(t *testing.T) {
	// Both reserved bytes plus ordinary data.
	in := []byte{0x00, 0xC0, 0xDB, 0xFF, 0xC0, 0xDB, 0xDB, 0x42}
	esc := slipEscape(in)
	// A raw 0xC0 must not survive escaping, or the device would see the frame
	// end early.
	if bytes.IndexByte(esc, slipEnd) != -1 {
		t.Errorf("escaped output still contains 0xC0: % X", esc)
	}
	out, err := slipUnescape(esc)
	if err != nil {
		t.Fatalf("slipUnescape: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("round trip: got % X, want % X", out, in)
	}
}

func TestSlipUnescapeRejectsBadInput(t *testing.T) {
	if _, err := slipUnescape([]byte{0x01, slipEsc}); err == nil {
		t.Error("expected error on truncated escape")
	}
	if _, err := slipUnescape([]byte{slipEsc, 0x99}); err == nil {
		t.Error("expected error on invalid escape byte")
	}
}

func TestExtractFrame(t *testing.T) {
	body := []byte{0xD1, 0x4E, 0x00, 0xE1}
	buf := append(append([]byte{slipEnd}, body...), slipEnd)
	got, ok := extractFrame(buf)
	if !ok || !bytes.Equal(got, body) {
		t.Fatalf("extractFrame = % X, %v", got, ok)
	}

	// Leading noise before the opening delimiter is discarded.
	noisy := append([]byte{0xAA, 0xBB}, buf...)
	got, ok = extractFrame(noisy)
	if !ok || !bytes.Equal(got, body) {
		t.Fatalf("extractFrame with leading noise = % X, %v", got, ok)
	}

	// An incomplete frame is not yet extractable.
	if _, ok := extractFrame([]byte{slipEnd, 0xD1}); ok {
		t.Error("extractFrame accepted an unterminated frame")
	}

	// Back-to-back delimiters must not yield an empty frame.
	if _, ok := extractFrame([]byte{slipEnd, slipEnd}); ok {
		t.Error("extractFrame accepted an empty frame")
	}
}

func TestAckNumber(t *testing.T) {
	// Header byte 0xD1 carries next-expected = 2 in bits 3..5.
	got, err := ackNumber([]byte{0xD1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("ackNumber = %d, want 2", got)
	}
	if _, err := ackNumber(nil); err == nil {
		t.Error("expected error on empty frame")
	}
}

func TestSequencerWrapsAtEight(t *testing.T) {
	var s sequencer
	var seen []uint8
	for i := 0; i < 9; i++ {
		seq, ack := s.next()
		seen = append(seen, seq)
		if ack != (seq+1)%8 {
			t.Errorf("ack %d does not follow seq %d", ack, seq)
		}
	}
	want := []uint8{1, 2, 3, 4, 5, 6, 7, 0, 1}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("sequence = %v, want %v", seen, want)
		}
	}
}

func TestOpenPackage(t *testing.T) {
	firmware := bytes.Repeat([]byte{0xAB, 0xCD}, 512)
	crc := CRC16(firmware)
	// A legacy init packet ends with the firmware CRC-16, little endian.
	dat := append(mustHex(t, "5200ffffffffffff0100b600"), byte(crc), byte(crc>>8))

	data := buildTestPackage(t, map[string][]byte{
		"firmware.bin": firmware,
		"firmware.dat": dat,
	}, map[string]any{
		"manifest": map[string]any{
			"dfu_version": 0.5,
			"application": map[string]any{
				"bin_file": "firmware.bin",
				"dat_file": "firmware.dat",
			},
		},
	})

	pkg, err := OpenPackage(data)
	if err != nil {
		t.Fatalf("OpenPackage: %v", err)
	}
	if len(pkg.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(pkg.Images))
	}
	img := pkg.Images[0]
	if img.Mode != ModeApplication {
		t.Errorf("mode = %d, want %d", img.Mode, ModeApplication)
	}
	if !bytes.Equal(img.Firmware, firmware) {
		t.Error("firmware payload mismatch")
	}
	if pkg.TotalBytes() != len(firmware) {
		t.Errorf("TotalBytes = %d, want %d", pkg.TotalBytes(), len(firmware))
	}
	if err := pkg.VerifyChecksums(); err != nil {
		t.Errorf("VerifyChecksums on a good package: %v", err)
	}
}

func TestPackageRejectsCorruptFirmware(t *testing.T) {
	firmware := bytes.Repeat([]byte{0x11}, 256)
	// Deliberately wrong CRC, as a truncated download would produce.
	dat := append(mustHex(t, "5200ffffffffffff0100b600"), 0x00, 0x00)

	data := buildTestPackage(t, map[string][]byte{
		"firmware.bin": firmware,
		"firmware.dat": dat,
	}, map[string]any{
		"manifest": map[string]any{
			"dfu_version": 0.5,
			"application": map[string]any{"bin_file": "firmware.bin", "dat_file": "firmware.dat"},
		},
	})

	pkg, err := OpenPackage(data)
	if err != nil {
		t.Fatalf("OpenPackage: %v", err)
	}
	if err := pkg.VerifyChecksums(); err == nil {
		t.Fatal("expected a checksum failure on corrupt firmware")
	}
}

func TestPackageRejectsSecureDFU(t *testing.T) {
	data := buildTestPackage(t, map[string][]byte{
		"firmware.bin": {0x01},
		"firmware.dat": {0x00, 0x00},
	}, map[string]any{
		"manifest": map[string]any{
			"dfu_version": 0.8,
			"application": map[string]any{"bin_file": "firmware.bin", "dat_file": "firmware.dat"},
		},
	})
	_, err := OpenPackage(data)
	if err == nil {
		t.Fatal("expected Secure DFU packages to be refused")
	}
}

func TestEraseAndActivateWaits(t *testing.T) {
	s := &Session{totalSize: 464616}
	// ~114 pages at 89.7ms each.
	if got := s.eraseWait(); got < 9e9 || got > 12e9 {
		t.Errorf("eraseWait for 464616 bytes = %v, expected roughly 10s", got)
	}
	// A tiny image still waits the floor.
	small := &Session{totalSize: 16}
	if got := small.eraseWait(); got < minEraseWait {
		t.Errorf("eraseWait = %v, want at least %v", got, minEraseWait)
	}
	// Single-bank app-only updates skip the bank copy entirely.
	sb := &Session{totalSize: 464616, singleBank: true}
	if sb.activateWait() >= s.activateWait() {
		t.Error("single-bank activation should be shorter than dual-bank")
	}
}

// --- helpers --------------------------------------------------------------

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// mustHex decodes hex, ignoring spaces used for readability in golden strings.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			clean = append(clean, s[i])
		}
	}
	b, err := hex.DecodeString(string(clean))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func buildTestPackage(t *testing.T, files map[string][]byte, manifest any) []byte {
	t.Helper()
	mj, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", mj)
	for name, data := range files {
		write(name, data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
