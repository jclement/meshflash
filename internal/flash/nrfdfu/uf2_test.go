package nrfdfu

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// makeUF2Block builds one 512-byte UF2 block.
func makeUF2Block(t *testing.T, addr uint32, payload []byte, flags, blockNo, numBlocks uint32) []byte {
	t.Helper()
	if len(payload) > uf2MaxPayload {
		t.Fatalf("payload of %d exceeds %d", len(payload), uf2MaxPayload)
	}
	b := make([]byte, uf2BlockSize)
	binary.LittleEndian.PutUint32(b[0:4], uf2MagicStart0)
	binary.LittleEndian.PutUint32(b[4:8], uf2MagicStart1)
	binary.LittleEndian.PutUint32(b[8:12], flags)
	binary.LittleEndian.PutUint32(b[12:16], addr)
	binary.LittleEndian.PutUint32(b[16:20], uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[20:24], blockNo)
	binary.LittleEndian.PutUint32(b[24:28], numBlocks)
	copy(b[32:], payload)
	binary.LittleEndian.PutUint32(b[508:512], uf2MagicEnd)
	return b
}

func TestUF2ToBin(t *testing.T) {
	// Two contiguous 256-byte blocks starting at the usual nRF52840
	// application base above the SoftDevice.
	const base = 0x26000
	first := bytes.Repeat([]byte{0xAA}, 256)
	second := bytes.Repeat([]byte{0xBB}, 256)

	var uf2 []byte
	uf2 = append(uf2, makeUF2Block(t, base, first, 0, 0, 2)...)
	uf2 = append(uf2, makeUF2Block(t, base+256, second, 0, 1, 2)...)

	img, got, err := UF2ToBin(uf2)
	if err != nil {
		t.Fatalf("UF2ToBin: %v", err)
	}
	if got != base {
		t.Errorf("base = 0x%X, want 0x%X", got, base)
	}
	want := append(append([]byte{}, first...), second...)
	if !bytes.Equal(img, want) {
		t.Errorf("image mismatch: got %d bytes, want %d", len(img), len(want))
	}
}

// Blocks marked as not-main-flash carry metadata and must not become firmware.
func TestUF2ToBinSkipsNonFlashBlocks(t *testing.T) {
	const base = 0x26000
	payload := bytes.Repeat([]byte{0x11}, 256)

	var uf2 []byte
	uf2 = append(uf2, makeUF2Block(t, base, payload, 0, 0, 2)...)
	uf2 = append(uf2, makeUF2Block(t, 0x10000000,
		bytes.Repeat([]byte{0x99}, 256), uf2FlagNotMainFlash, 1, 2)...)

	img, got, err := UF2ToBin(uf2)
	if err != nil {
		t.Fatalf("UF2ToBin: %v", err)
	}
	if got != base {
		t.Errorf("base = 0x%X, want 0x%X — a metadata block was treated as firmware", got, base)
	}
	if len(img) != 256 {
		t.Errorf("image is %d bytes, want 256", len(img))
	}
}

// A gap between regions must read as erased flash, not as zeroes or a shift.
func TestUF2ToBinPadsGapsWithErasedFlash(t *testing.T) {
	const base = 0x26000
	a := bytes.Repeat([]byte{0x01}, 16)
	b := bytes.Repeat([]byte{0x02}, 16)

	var uf2 []byte
	uf2 = append(uf2, makeUF2Block(t, base, a, 0, 0, 2)...)
	uf2 = append(uf2, makeUF2Block(t, base+64, b, 0, 1, 2)...)

	img, _, err := UF2ToBin(uf2)
	if err != nil {
		t.Fatalf("UF2ToBin: %v", err)
	}
	if len(img) != 80 {
		t.Fatalf("image is %d bytes, want 80", len(img))
	}
	for i := 16; i < 64; i++ {
		if img[i] != 0xFF {
			t.Fatalf("gap byte %d is 0x%02X, want 0xFF", i, img[i])
		}
	}
	if !bytes.Equal(img[64:80], b) {
		t.Error("the second region landed at the wrong offset")
	}
}

// Blocks may arrive out of order; the flattened image must still be correct.
func TestUF2ToBinSortsByAddress(t *testing.T) {
	const base = 0x26000
	a := bytes.Repeat([]byte{0xA1}, 32)
	b := bytes.Repeat([]byte{0xB2}, 32)

	var uf2 []byte
	uf2 = append(uf2, makeUF2Block(t, base+32, b, 0, 1, 2)...)
	uf2 = append(uf2, makeUF2Block(t, base, a, 0, 0, 2)...)

	img, got, err := UF2ToBin(uf2)
	if err != nil {
		t.Fatalf("UF2ToBin: %v", err)
	}
	if got != base {
		t.Errorf("base = 0x%X, want 0x%X", got, base)
	}
	if !bytes.Equal(img[:32], a) || !bytes.Equal(img[32:], b) {
		t.Error("out-of-order blocks were not reassembled in address order")
	}
}

func TestUF2ToBinRejectsGarbage(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"short":          make([]byte, 100),
		"not a multiple": make([]byte, uf2BlockSize+1),
		"bad magic":      make([]byte, uf2BlockSize),
	}
	for name, data := range cases {
		if _, _, err := UF2ToBin(data); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The synthesised init packet must satisfy the checks in the Adafruit
// bootloader's dfu_init.c, or the device rejects the transfer outright.
func TestPackageFromUF2InitPacket(t *testing.T) {
	const base = 0x26000
	payload := bytes.Repeat([]byte{0x5A}, 256)
	uf2 := makeUF2Block(t, base, payload, 0, 0, 1)

	pkg, err := PackageFromUF2(uf2)
	if err != nil {
		t.Fatalf("PackageFromUF2: %v", err)
	}
	if len(pkg.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(pkg.Images))
	}
	img := pkg.Images[0]

	if img.Mode != ModeApplication {
		t.Errorf("mode = %d, want application (%d)", img.Mode, ModeApplication)
	}
	if !bytes.Equal(img.Firmware, payload) {
		t.Error("firmware payload does not match the UF2 contents")
	}

	init := img.InitPacket
	if len(init) != 14 {
		t.Fatalf("init packet is %d bytes, want the 14-byte legacy layout", len(init))
	}

	// device_type must be exactly ADAFRUIT_DEVICE_TYPE; the bootloader returns
	// NRF_ERROR_FORBIDDEN for anything else.
	if dt := binary.LittleEndian.Uint16(init[0:2]); dt != adafruitDeviceType {
		t.Errorf("device_type = 0x%04X, want 0x%04X", dt, adafruitDeviceType)
	}
	// The SoftDevice list must contain the wildcard, or the bootloader returns
	// NRF_ERROR_INVALID_DATA for a mismatched SoftDevice.
	if n := binary.LittleEndian.Uint16(init[8:10]); n != 1 {
		t.Errorf("softdevice_len = %d, want 1", n)
	}
	if sd := binary.LittleEndian.Uint16(init[10:12]); sd != dfuSoftDeviceAny {
		t.Errorf("softdevice[0] = 0x%04X, want the 0x%04X wildcard", sd, dfuSoftDeviceAny)
	}
	// The trailing CRC-16 is what the bootloader checks after the transfer.
	if crc := binary.LittleEndian.Uint16(init[12:14]); crc != CRC16(payload) {
		t.Errorf("firmware CRC = 0x%04X, want 0x%04X", crc, CRC16(payload))
	}

	// The package must survive its own verification, which is what runs before
	// a real transfer starts.
	if err := pkg.VerifyChecksums(); err != nil {
		t.Errorf("synthesised package fails its own checksum check: %v", err)
	}
}
