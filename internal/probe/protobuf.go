package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A minimal protobuf wire-format reader.
//
// meshflash needs three fields out of two messages, so pulling in the protobuf
// runtime and generating Meshtastic's whole schema would be a lot of machinery
// for very little. Field numbers and wire types are the stable part of
// protobuf — they cannot change without breaking every existing client — so
// reading them directly is durable, and unknown fields are skipped by
// construction, which is exactly how a protobuf parser is supposed to behave
// when the schema grows.
//
// See https://protobuf.dev/programming-guides/encoding/.

// Wire types.
const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireI32    = 5
)

// ErrTruncated means the buffer ended mid-field.
var ErrTruncated = errors.New("truncated protobuf message")

// field is one decoded key/value pair.
type field struct {
	num  int
	wire int
	// varint holds the value for wireVarint, wireI32 and wireI64.
	varint uint64
	// bytes holds the payload for wireBytes.
	bytes []byte
}

// nextField decodes one field and returns the remaining buffer.
func nextField(b []byte) (field, []byte, error) {
	key, n := binary.Uvarint(b)
	if n <= 0 {
		return field{}, nil, ErrTruncated
	}
	b = b[n:]

	f := field{num: int(key >> 3), wire: int(key & 0x7)}
	if f.num <= 0 {
		return field{}, nil, fmt.Errorf("invalid protobuf field number %d", f.num)
	}

	switch f.wire {
	case wireVarint:
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return field{}, nil, ErrTruncated
		}
		f.varint = v
		return f, b[n:], nil

	case wireBytes:
		length, n := binary.Uvarint(b)
		if n <= 0 {
			return field{}, nil, ErrTruncated
		}
		b = b[n:]
		if uint64(len(b)) < length {
			return field{}, nil, ErrTruncated
		}
		f.bytes = b[:length]
		return f, b[length:], nil

	case wireI64:
		if len(b) < 8 {
			return field{}, nil, ErrTruncated
		}
		f.varint = binary.LittleEndian.Uint64(b)
		return f, b[8:], nil

	case wireI32:
		if len(b) < 4 {
			return field{}, nil, ErrTruncated
		}
		f.varint = uint64(binary.LittleEndian.Uint32(b))
		return f, b[4:], nil

	default:
		// Group wire types (3 and 4) are deprecated and unused here; without
		// knowing the schema there is no way to skip one safely.
		return field{}, nil, fmt.Errorf("unsupported protobuf wire type %d", f.wire)
	}
}

// walk calls fn for each top-level field, stopping on error.
func walk(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		f, rest, err := nextField(b)
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
		b = rest
	}
	return nil
}

// appendVarintField encodes a varint field, which is all meshflash ever needs
// to write.
func appendVarintField(dst []byte, num int, v uint64) []byte {
	dst = binary.AppendUvarint(dst, uint64(num)<<3|wireVarint)
	return binary.AppendUvarint(dst, v)
}
