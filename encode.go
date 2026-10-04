package engine

import (
	"encoding/binary"
	"math"
	"sort"
)

// ---- 保序键编码 ----------------------------------------------------------
//
// 每个键列编码为：tag(1B) | escaped(payload) | terminator(0x00)。
// payload 按类型保证同类型内的字节序与值序一致；转义保证自界定：
//
//	原始 0x00 -> 0x01 0x01
//	原始 0x01 -> 0x01 0x02
//	列终止符 -> 0x00（转义后的 payload 中不会出现裸 0x00）
//
// 因此复合键按原始字节比较即为逐字段比较；NULL 的 tag 为 0x00，排在最前。
// 不同 Kind 的列之间按 tag 序排列（同一列的 Kind 由 Schema 固定，故不影响查询语义）。

const (
	tagNull    = 0x00
	tagBool    = 0x02
	tagInt64   = 0x03
	tagUint64  = 0x04
	tagFloat64 = 0x05
	tagString  = 0x06
	tagBytes   = 0x07
	keyTerm    = 0x00
)

func escapePayload(p []byte) []byte {
	out := make([]byte, 0, len(p)+8)
	for _, c := range p {
		switch c {
		case 0x00:
			out = append(out, 0x01, 0x01)
		case 0x01:
			out = append(out, 0x01, 0x02)
		default:
			out = append(out, c)
		}
	}
	return out
}

func orderedInt64(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v)^(1<<63))
	return b
}

func orderedUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// orderedFloat64 返回 IEEE754 double 的保序变换：
// 负数全部取反，正数仅翻转符号位。
func orderedFloat64(f float64) []byte {
	u := binary.BigEndian.Uint64(float64RawBytes(f))
	if u&(1<<63) != 0 {
		u = ^u
	} else {
		u ^= 1 << 63
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, u)
	return b
}

func float64RawBytes(f float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(f))
	return b
}

// encodeKeyPart 把单个字段值编码为保序字节串（tag + escaped payload，不含终止符）。
func encodeKeyPart(v Value) []byte {
	switch v.kind {
	case KindNull:
		return []byte{tagNull}
	case KindBool:
		x := byte(0)
		if v.b {
			x = 1
		}
		return append([]byte{tagBool}, escapePayload([]byte{x})...)
	case KindInt64:
		return append([]byte{tagInt64}, escapePayload(orderedInt64(v.i))...)
	case KindUint64:
		return append([]byte{tagUint64}, escapePayload(orderedUint64(v.u))...)
	case KindFloat64:
		return append([]byte{tagFloat64}, escapePayload(orderedFloat64(v.f))...)
	case KindString:
		return append([]byte{tagString}, escapePayload([]byte(v.s))...)
	case KindBytes:
		return append([]byte{tagBytes}, escapePayload(v.raw)...)
	}
	return []byte{tagNull}
}

// partFull 返回含终止符的完整列编码。
func partFull(v Value) []byte {
	p := encodeKeyPart(v)
	return append(p, keyTerm)
}

// encodeKey 把多个字段拼接为自界定复合键，字节序等于逐字段值序。
func encodeKey(parts []Value) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, partFull(p)...)
	}
	return out
}

// encodePrefix 编码等值前缀（复合索引最左 N 列的等值匹配），
// 输出与 encodeKey 相同，配合 prefixEnd 使用。
func encodePrefix(parts []Value) []byte {
	return encodeKey(parts)
}

// byteUpper 返回字典序上严格大于所有以 b 为前缀的字节串的最短上界；
// 若不存在（b 全为 0xFF）返回 nil，表示 +∞。
func byteUpper(b []byte) []byte {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xFF {
			out := make([]byte, i+1)
			copy(out, b[:i+1])
			out[i]++
			return out
		}
	}
	return nil
}

// prefixEnd 返回等值前缀的排他上界。入参为 encodePrefix 的输出（含末尾终止符）。
func prefixEnd(prefix []byte) []byte {
	if len(prefix) == 0 {
		return nil
	}
	return byteUpper(prefix[:len(prefix)-1])
}

// ---- 存储编码（仅需可逆，无序要求） --------------------------------------

func appendUvarint(b []byte, x uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], x)
	return append(b, buf[:n]...)
}

func appendString(b []byte, s string) []byte {
	b = appendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// encodeValueStorage 是字段值的紧凑可逆编码（tag=Kind + 长度 + 载荷）。
func encodeValueStorage(v Value) []byte {
	var out []byte
	out = appendUvarint(out, uint64(v.kind))
	switch v.kind {
	case KindNull:
	case KindBool:
		x := byte(0)
		if v.b {
			x = 1
		}
		out = append(out, x)
	case KindInt64:
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v.i))
		out = append(out, buf[:]...)
	case KindUint64:
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v.u)
		out = append(out, buf[:]...)
	case KindFloat64:
		out = append(out, float64RawBytes(v.f)...)
	case KindString:
		out = appendString(out, v.s)
	case KindBytes:
		out = appendUvarint(out, uint64(len(v.raw)))
		out = append(out, v.raw...)
	}
	return out
}

func decodeUvarint(b []byte) (uint64, int) {
	return binary.Uvarint(b)
}

func decodeValueStorage(b []byte) (Value, int) {
	k, n := decodeUvarint(b)
	if n <= 0 || n > len(b) {
		return NullValue(), 0
	}
	rest := b[n:]
	total := n
	switch Kind(k) {
	case KindNull:
	case KindBool:
		if len(rest) < 1 {
			return NullValue(), 0
		}
		v := BoolValue(rest[0] != 0)
		return v, total + 1
	case KindInt64:
		if len(rest) < 8 {
			return NullValue(), 0
		}
		v := Int64Value(int64(binary.BigEndian.Uint64(rest[:8])))
		return v, total + 8
	case KindUint64:
		if len(rest) < 8 {
			return NullValue(), 0
		}
		v := Uint64Value(binary.BigEndian.Uint64(rest[:8]))
		return v, total + 8
	case KindFloat64:
		if len(rest) < 8 {
			return NullValue(), 0
		}
		v := Float64Value(math.Float64frombits(binary.BigEndian.Uint64(rest[:8])))
		return v, total + 8
	case KindString:
		l, m := decodeUvarint(rest)
		if m <= 0 || uint64(len(rest)-m) < l {
			return NullValue(), 0
		}
		v := StringValue(string(rest[m : m+int(l)]))
		return v, total + m + int(l)
	case KindBytes:
		l, m := decodeUvarint(rest)
		if m <= 0 || uint64(len(rest)-m) < l {
			return NullValue(), 0
		}
		raw := append([]byte(nil), rest[m:m+int(l)]...)
		return BytesValue(raw), total + m + int(l)
	default:
		return NullValue(), 0
	}
	return NullValue(), total
}

// encodeFields 按给定字段集合编码（字段按名排序以保证确定性）。
func encodeFields(r Record, fields []string, all bool) []byte {
	names := fields
	if all {
		names = make([]string, 0, len(r))
		for k := range r {
			names = append(names, k)
		}
		sortStrings(names)
	} else {
		names = append([]string(nil), names...)
		sortStrings(names)
	}
	var out []byte
	out = appendUvarint(out, uint64(len(names)))
	for _, name := range names {
		out = appendString(out, name)
		v, ok := r[name]
		if !ok {
			v = NullValue()
		}
		out = appendUvarint(out, 1)
		out = append(out, encodeValueStorage(v)...)
	}
	return out
}

func sortStrings(s []string) { sort.Strings(s) }

// encodeRecord 序列化整条记录（全部字段，按名排序）。
func encodeRecord(r Record) []byte {
	return encodeFields(r, nil, true)
}

func decodeFields(b []byte) (Record, int) {
	cnt, n := decodeUvarint(b)
	if n <= 0 {
		return nil, 0
	}
	pos := n
	r := Record{}
	for i := uint64(0); i < cnt; i++ {
		l, m := decodeUvarint(b[pos:])
		if m <= 0 || uint64(len(b)-pos-m) < l {
			return nil, 0
		}
		pos += m
		name := string(b[pos : pos+int(l)])
		pos += int(l)
		if pos >= len(b) || b[pos] != 1 {
			return nil, 0
		}
		pos++
		v, used := decodeValueStorage(b[pos:])
		if used <= 0 {
			return nil, 0
		}
		pos += used
		r[name] = v
	}
	return r, pos
}

func decodeRecord(b []byte) Record {
	r, _ := decodeFields(b)
	return r
}

// encodeCovered 编码覆盖列（按 Covered 字段名集合的确定顺序编码）。
func encodeCovered(fields []string, r Record) []byte {
	return encodeFields(r, fields, false)
}

// decodeCovered 解码覆盖列。
func decodeCovered(keys []string, b []byte) Record {
	r, _ := decodeFields(b)
	if r == nil {
		return Record{}
	}
	return r
}

// beUint64 大端解码 8 字节。
func beUint64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}

// float64FromOrderedBits 是 orderedFloat64 的逆变换。
func float64FromOrderedBits(u uint64) float64 {
	if u&(1<<63) != 0 {
		u ^= 1 << 63
	} else {
		u = ^u
	}
	return math.Float64frombits(u)
}
