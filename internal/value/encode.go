package value

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Key encoding: a byte string whose bytewise order matches Compare, so the
// B-tree can order index entries with bytes.Compare and never decode them.
//
//	NULL     0x05
//	number   0x10, 8 bytes of order-preserving float64, 8 bytes of int64
//	text     0x20, bytes with 0x00 escaped as 0x00 0xFF, then 0x00 0x01
//	blob     0x30, same escaping as text
//
// Numbers carry both a float (so integers and reals interleave correctly)
// and an exact integer part that breaks ties between large integers that
// round to the same float.

const (
	tagNull   = 0x05
	tagNumber = 0x10
	tagText   = 0x20
	tagBlob   = 0x30
)

// AppendKey appends the key encoding of v to dst.
func AppendKey(dst []byte, v Value) []byte {
	switch v.K {
	case Null:
		return append(dst, tagNull)
	case Int, Bool:
		dst = append(dst, tagNumber)
		dst = appendFloatKey(dst, float64(v.I))
		return appendIntKey(dst, v.I)
	case Real:
		dst = append(dst, tagNumber)
		dst = appendFloatKey(dst, v.F)
		var tie int64
		switch {
		case v.F >= 9.223372036854775807e18:
			tie = math.MaxInt64
		case v.F < -9.223372036854775808e18:
			tie = math.MinInt64
		default:
			tie = int64(math.Trunc(v.F))
		}
		return appendIntKey(dst, tie)
	case Text:
		dst = append(dst, tagText)
		return appendEscaped(dst, []byte(v.S))
	case Blob:
		dst = append(dst, tagBlob)
		return appendEscaped(dst, v.B)
	}
	panic("unknown kind")
}

// EncodeKey encodes a tuple of values as one key.
func EncodeKey(vals ...Value) []byte {
	var dst []byte
	for _, v := range vals {
		dst = AppendKey(dst, v)
	}
	return dst
}

func appendFloatKey(dst []byte, f float64) []byte {
	bits := math.Float64bits(f)
	if f == 0 {
		bits = 0 // -0 and +0 are the same key
	}
	if bits>>63 == 0 {
		bits |= 1 << 63
	} else {
		bits = ^bits
	}
	return binary.BigEndian.AppendUint64(dst, bits)
}

func appendIntKey(dst []byte, i int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(i)^(1<<63))
}

func appendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		if c == 0 {
			dst = append(dst, 0, 0xFF)
		} else {
			dst = append(dst, c)
		}
	}
	return append(dst, 0, 1)
}

// RowidKey is the 8-byte key of a table row: its rowid, order-preserving.
func RowidKey(rowid int64) []byte {
	return appendIntKey(make([]byte, 0, 8), rowid)
}

// RowidFromKey decodes a RowidKey (or the last 8 bytes of an index key).
func RowidFromKey(k []byte) int64 {
	return int64(binary.BigEndian.Uint64(k[len(k)-8:]) ^ (1 << 63))
}

// Record encoding: a row as a sequence of values.
//
//	uvarint count, then per value: kind byte + payload
//	INTEGER/BOOLEAN varint, REAL 8 bytes, TEXT/BLOB uvarint length + bytes

var ErrCorrupt = errors.New("corrupt record")

// EncodeRecord encodes a row.
func EncodeRecord(vals []Value) []byte {
	dst := binary.AppendUvarint(nil, uint64(len(vals)))
	for _, v := range vals {
		dst = append(dst, byte(v.K))
		switch v.K {
		case Int, Bool:
			dst = binary.AppendVarint(dst, v.I)
		case Real:
			dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(v.F))
		case Text:
			dst = binary.AppendUvarint(dst, uint64(len(v.S)))
			dst = append(dst, v.S...)
		case Blob:
			dst = binary.AppendUvarint(dst, uint64(len(v.B)))
			dst = append(dst, v.B...)
		}
	}
	return dst
}

// DecodeRecord decodes a row. Rows written before a column was added have
// fewer values; the caller pads them with defaults.
func DecodeRecord(b []byte) ([]Value, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return nil, ErrCorrupt
	}
	b = b[k:]
	out := make([]Value, n)
	for i := range out {
		if len(b) == 0 {
			return nil, ErrCorrupt
		}
		kind := Kind(b[0])
		b = b[1:]
		switch kind {
		case Null:
		case Int, Bool:
			x, k := binary.Varint(b)
			if k <= 0 {
				return nil, ErrCorrupt
			}
			out[i] = Value{K: kind, I: x}
			b = b[k:]
		case Real:
			if len(b) < 8 {
				return nil, ErrCorrupt
			}
			out[i] = Value{K: Real, F: math.Float64frombits(binary.BigEndian.Uint64(b))}
			b = b[8:]
		case Text, Blob:
			l, k := binary.Uvarint(b)
			if k <= 0 || l > uint64(len(b)-k) {
				return nil, ErrCorrupt
			}
			data := b[k : k+int(l)]
			if kind == Text {
				out[i] = Value{K: Text, S: string(data)}
			} else {
				out[i] = Value{K: Blob, B: append([]byte(nil), data...)}
			}
			b = b[k+int(l):]
		default:
			return nil, fmt.Errorf("%w: unknown kind %d", ErrCorrupt, kind)
		}
	}
	return out, nil
}
