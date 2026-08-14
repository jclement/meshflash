package nrfdfu

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// UF2 block layout. Every block is exactly 512 bytes.
const (
	uf2BlockSize   = 512
	uf2MagicStart0 = 0x0A324655 // "UF2\n"
	uf2MagicStart1 = 0x9E5D5157
	uf2MagicEnd    = 0x0AB16F30
	uf2MaxPayload  = 476
)

// UF2 block flags.
const (
	uf2FlagNotMainFlash  = 0x00000001
	uf2FlagFileContainer = 0x00001000
)

// Adafruit's bootloader validates the init packet before accepting an image.
// From Adafruit_nRF52_Bootloader/src/dfu_init.c:
//
//   - device_type must equal ADAFRUIT_DEVICE_TYPE (0x0052), unconditionally.
//   - device_rev is only checked when updating the SoftDevice or bootloader,
//     which this never does.
//   - the softdevice list must contain either the installed SoftDevice's
//     firmware id or DFU_SOFTDEVICE_ANY (0xFFFE).
//   - the trailing CRC-16 is checked against the received image.
const (
	adafruitDeviceType = 0x0052
	dfuSoftDeviceAny   = 0xFFFE
	anyDeviceRev       = 0xFFFF
	anyAppVersion      = 0xFFFFFFFF
)

// PackageFromUF2 builds a DFU package from a .uf2 image.
//
// Meshtastic ships nRF52 firmware only as UF2, which normally means dragging
// it onto a mass-storage volume. That volume does not exist when the board was
// put into its bootloader by a 1200-baud touch: the Adafruit bootloader brings
// up CDC only in that mode, and mass storage appears solely after a double-tap
// reset. Converting the image and pushing it over serial DFU is what makes
// automatic bootloader entry actually useful on those boards.
//
// The init packet is synthesised rather than shipped, using the wildcards the
// bootloader documents, because there is no .dat to read.
func PackageFromUF2(uf2 []byte) (*Package, error) {
	firmware, base, err := UF2ToBin(uf2)
	if err != nil {
		return nil, err
	}

	init := make([]byte, 0, 14)
	init = binary.LittleEndian.AppendUint16(init, adafruitDeviceType)
	init = binary.LittleEndian.AppendUint16(init, anyDeviceRev)
	init = binary.LittleEndian.AppendUint32(init, anyAppVersion)
	init = binary.LittleEndian.AppendUint16(init, 1) // one SoftDevice entry
	init = binary.LittleEndian.AppendUint16(init, dfuSoftDeviceAny)
	init = binary.LittleEndian.AppendUint16(init, CRC16(firmware))

	return &Package{
		DFUVersion: 0.5,
		Images: []Image{{
			Mode:       ModeApplication,
			Name:       fmt.Sprintf("application (converted from UF2 at 0x%X)", base),
			Firmware:   firmware,
			InitPacket: init,
		}},
	}, nil
}

// UF2ToBin flattens a UF2 image into the raw bytes it would write to flash,
// returning the image and the flash address it starts at.
//
// Blocks are sorted by address and gaps padded with 0xFF, matching erased
// flash, because a UF2 may legitimately describe non-contiguous regions.
func UF2ToBin(uf2 []byte) (image []byte, base uint32, err error) {
	if len(uf2) == 0 || len(uf2)%uf2BlockSize != 0 {
		return nil, 0, fmt.Errorf("UF2 image is %d bytes, not a multiple of the %d byte block size",
			len(uf2), uf2BlockSize)
	}

	type chunk struct {
		addr uint32
		data []byte
	}
	var chunks []chunk

	for off := 0; off < len(uf2); off += uf2BlockSize {
		b := uf2[off : off+uf2BlockSize]

		if binary.LittleEndian.Uint32(b[0:4]) != uf2MagicStart0 ||
			binary.LittleEndian.Uint32(b[4:8]) != uf2MagicStart1 ||
			binary.LittleEndian.Uint32(b[508:512]) != uf2MagicEnd {
			return nil, 0, fmt.Errorf("UF2 block %d has bad magic", off/uf2BlockSize)
		}

		flags := binary.LittleEndian.Uint32(b[8:12])
		// Blocks not destined for flash carry metadata, not firmware.
		if flags&(uf2FlagNotMainFlash|uf2FlagFileContainer) != 0 {
			continue
		}

		addr := binary.LittleEndian.Uint32(b[12:16])
		size := binary.LittleEndian.Uint32(b[16:20])
		if size > uf2MaxPayload {
			return nil, 0, fmt.Errorf("UF2 block %d claims a %d byte payload, over the %d byte maximum",
				off/uf2BlockSize, size, uf2MaxPayload)
		}

		payload := make([]byte, size)
		copy(payload, b[32:32+size])
		chunks = append(chunks, chunk{addr: addr, data: payload})
	}

	if len(chunks) == 0 {
		return nil, 0, fmt.Errorf("UF2 image contains no flashable blocks")
	}

	sort.Slice(chunks, func(i, j int) bool { return chunks[i].addr < chunks[j].addr })

	base = chunks[0].addr
	end := base
	for _, c := range chunks {
		if e := c.addr + uint32(len(c.data)); e > end {
			end = e
		}
	}

	span := int(end - base)
	// A malformed image with wild addresses would otherwise allocate wildly.
	const maxImage = 8 << 20
	if span <= 0 || span > maxImage {
		return nil, 0, fmt.Errorf("UF2 image spans %d bytes from 0x%X, which is not plausible", span, base)
	}

	image = make([]byte, span)
	for i := range image {
		image[i] = 0xFF // erased flash
	}
	for _, c := range chunks {
		copy(image[c.addr-base:], c.data)
	}
	return image, base, nil
}
