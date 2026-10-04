package sindex

import (
	"encoding/binary"
	"math"
)

// Order-preserving key encoding
//
// Index rows are addressed by the concatenation of encoded index columns,
// each terminated by a sentinel byte 0x00. The encoding of a column is:
//
//	tag || payload || 0x00
//
// Tags are ordered so that the byte order of encoded columns matches the
// total value order documented on Value:
//
//	nil        0x00 (no payload)
//	bool false 0x01 (no payload)
//	bool true  0x02 (no payload)
//	int64      0x10 || 8-byte big-endian, sign flipped
//	float64    0x20 || 8-byte big-endian, sign-flip IEEE-754
//	string     0x30 || escaped UTF-8 bytes
//	bytes      0x40 || escaped raw bytes
//
// Escaping (a strictly order-preserving bijection):
//
//	0x00 -> 0x00 0xFF
//	0xFF -> 0xFF 0x00
//	other bytes unchanged
//
// so neither the raw 0x00 terminator nor the escape pair can collide with
// payload content, and a prefix can never end on the first byte of an escape
// pair in an ambiguous way. The substitution is monotone, hence byte order of
// escaped payloads equals byte order of the originals. The terminator 0x00
// only ever appears unescaped at the column boundary (an escaped 0x00 is the
// pair 00 FF, never a lone 00), so the "increment last byte" construction for
// prefix successors remains correct.
const (
	tagNil       byte = 0x00
	tagBoolFalse byte = 0x01
	tagBoolTrue  byte = 0x02
	tagInt64     byte = 0x10
	tagFloat64   byte = 0x20
	tagString    byte = 0x30
	tagBytes     byte = 0x40
	term         byte = 0x00
)

// encodeValue appends one fully encoded column (tag + payload + terminator)
// to dst and returns the new slice.
func encodeValue(dst []byte, v Value) []byte {
	switch v.kind {
	case KindNil:
		dst = append(dst, tagNil, term)
	case KindBool:
		if v.b {
			dst = append(dst, tagBoolTrue, term)
		} else {
			dst = append(dst, tagBoolFalse, term)
		}
	case KindInt64:
		dst = append(dst, tagInt64)
		var buf [8]byte
		// Flip the sign bit so big-endian order matches signed order.
		binary.BigEndian.PutUint64(buf[:], uint64(v.i)^1<<63)
		dst = append(dst, buf[:]...)
		dst = append(dst, term)
	case KindFloat64:
		dst = append(dst, tagFloat64)
		u := math.Float64bits(v.f)
		// Positive floats: flip sign bit; negative floats: flip all bits.
		if u&(1<<63) != 0 {
			u = ^u
		} else {
			u ^= 1 << 63
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], u)
		dst = append(dst, buf[:]...)
		dst = append(dst, term)
	case KindString:
		dst = append(dst, tagString)
		dst = appendEscaped(dst, []byte(v.s))
		dst = append(dst, term)
	case KindBytes:
		dst = append(dst, tagBytes)
		// An escaped payload never ends in a lone 0x00, so empty and nil both
		// encode as tag immediately followed by the terminator; they are
		// semantically equal byte strings in this engine.
		dst = appendEscaped(dst, v.x)
		dst = append(dst, term)
	default:
		// Unreachable for validated values; encode as nil.
		dst = append(dst, tagNil, term)
	}
	return dst
}

// encodeKey appends the fully encoded concatenation of several columns.
func encodeKey(dst []byte, vals []Value) []byte {
	for _, v := range vals {
		dst = encodeValue(dst, v)
	}
	return dst
}

// appendEscaped applies the order-preserving escape documented above.
func appendEscaped(dst, raw []byte) []byte {
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case 0x00:
			dst = append(dst, 0x00, 0xFF)
		case 0xFF:
			dst = append(dst, 0xFF, 0x00)
		default:
			dst = append(dst, raw[i])
		}
	}
	return dst
}

// encodeColumnPrefix appends the encoding of a partial trailing column:
//
//	tag || escaped(prefix)
//
// with NO terminator. It is the shared low byte-sequence used by both the
// inclusive lower bound and the exclusive upper bound of a prefix scan on
// that column. All preceding columns must be complete (use encodeValue).
func encodeColumnPrefix(dst []byte, v Value) []byte {
	switch v.kind {
	case KindNil, KindBool, KindInt64, KindFloat64:
		// Fixed-width kinds have no meaningful "prefix": use the full column
		// but drop the terminator.
		full := encodeValue(nil, v)
		dst = append(dst, full[:len(full)-1]...)
	case KindString:
		dst = append(dst, tagString)
		dst = appendEscaped(dst, []byte(v.s))
	case KindBytes:
		dst = append(dst, tagBytes)
		dst = appendEscaped(dst, v.x)
	default:
		dst = append(dst, tagNil)
	}
	return dst
}

// prefixUpperBound returns the smallest byte slice strictly greater than
// every byte slice having prefix as a prefix (successor key). ok is false
// only for the degenerate all-0xFF prefix, for which no finite upper bound
// exists.
func prefixUpperBound(prefix []byte) ([]byte, bool) {
	out := make([]byte, len(prefix))
	copy(out, prefix)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1], true
		}
	}
	return nil, false
}
