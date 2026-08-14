package nrfdfu

import "fmt"

// Wire-level constants for the Nordic legacy (SDK 0.5-era) DFU transport that
// the Adafruit nRF52 bootloader speaks. Every frame is an HCI packet wrapped
// in SLIP framing with a trailing CRC-16.
const (
	slipEnd    = 0xC0 // frame delimiter
	slipEsc    = 0xDB
	slipEscEnd = 0xDC // 0xDB 0xDC decodes to 0xC0
	slipEscEsc = 0xDD // 0xDB 0xDD decodes to 0xDB

	hciPacketType    = 14 // "vendor specific" — the DFU channel
	dataIntegrityOn  = 1
	reliablePacketOn = 1
)

// DFU opcodes, sent as the first little-endian uint32 of each frame payload.
const (
	opInitPacket     uint32 = 1
	opStartPacket    uint32 = 3
	opDataPacket     uint32 = 4
	opStopDataPacket uint32 = 5
)

// Update modes. These double as the HexType passed to the start packet.
const (
	ModeNone        uint32 = 0
	ModeSoftDevice  uint32 = 1
	ModeBootloader  uint32 = 2
	ModeApplication uint32 = 4
)

// maxDFUPayload is the firmware chunk size per data packet. The bootloader's
// receive buffer is sized for this; larger frames are dropped.
const maxDFUPayload = 512

// sequencer tracks the 3-bit reliable-packet sequence number across a session.
type sequencer struct{ n uint8 }

// next advances and returns the sequence number for the packet about to be
// sent, along with the acknowledgement number the device should reply with.
func (s *sequencer) next() (seq, expectAck uint8) {
	s.n = (s.n + 1) % 8
	return s.n, (s.n + 1) % 8
}

func (s *sequencer) reset() { s.n = 0 }

// slipHeader builds the 4-byte HCI header preceding the payload.
//
//	byte 0: seq(3) | next-expected(3) | data-integrity(1) | reliable(1)
//	byte 1: packet type(4) | length low nibble(4)
//	byte 2: length bits 4..11
//	byte 3: two's-complement checksum of bytes 0..2
func slipHeader(seq uint8, payloadLen int) [4]byte {
	var h [4]byte
	h[0] = seq | (((seq + 1) % 8) << 3) | (dataIntegrityOn << 6) | (reliablePacketOn << 7)
	h[1] = byte(hciPacketType) | byte((payloadLen&0x000F)<<4)
	h[2] = byte((payloadLen & 0x0FF0) >> 4)
	h[3] = byte((^(uint16(h[0]) + uint16(h[1]) + uint16(h[2])) + 1) & 0xFF)
	return h
}

// buildFrame assembles a complete on-wire frame: header + payload + CRC-16,
// SLIP-escaped and delimited.
//
// The length field is 12 bits, so payload is capped at 4095 bytes. Our largest
// frame is a data packet at 4 + 512, well inside that.
func buildFrame(seq uint8, payload []byte) ([]byte, error) {
	if len(payload) > 0x0FFF {
		return nil, fmt.Errorf("dfu payload %d bytes exceeds 12-bit length field", len(payload))
	}

	h := slipHeader(seq, len(payload))
	body := make([]byte, 0, 4+len(payload)+2)
	body = append(body, h[:]...)
	body = append(body, payload...)

	crc := CRC16(body)
	body = append(body, byte(crc&0xFF), byte(crc>>8))

	// Worst case every byte needs escaping, plus both delimiters.
	out := make([]byte, 0, len(body)*2+2)
	out = append(out, slipEnd)
	out = append(out, slipEscape(body)...)
	out = append(out, slipEnd)
	return out, nil
}

// slipEscape replaces the two reserved bytes with their escape sequences.
func slipEscape(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch b {
		case slipEnd:
			out = append(out, slipEsc, slipEscEnd)
		case slipEsc:
			out = append(out, slipEsc, slipEscEsc)
		default:
			out = append(out, b)
		}
	}
	return out
}

// slipUnescape reverses slipEscape.
func slipUnescape(in []byte) ([]byte, error) {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] != slipEsc {
			out = append(out, in[i])
			continue
		}
		i++
		if i >= len(in) {
			return nil, fmt.Errorf("truncated SLIP escape at end of frame")
		}
		switch in[i] {
		case slipEscEnd:
			out = append(out, slipEnd)
		case slipEscEsc:
			out = append(out, slipEsc)
		default:
			return nil, fmt.Errorf("invalid SLIP escape 0xDB 0x%02X", in[i])
		}
	}
	return out, nil
}

// ackNumber extracts the acknowledgement sequence from a decoded reply frame.
// The device echoes its next-expected counter in bits 3..5 of the header.
func ackNumber(frame []byte) (uint8, error) {
	if len(frame) < 1 {
		return 0, fmt.Errorf("empty DFU acknowledgement frame")
	}
	return (frame[0] >> 3) & 0x07, nil
}

// le32 encodes a little-endian uint32, the DFU protocol's only integer form.
func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}
