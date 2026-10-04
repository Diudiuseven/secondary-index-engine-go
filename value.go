package engine

import "math/big"

// Kind 标识一个字段值的逻辑类型。
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindInt64
	KindUint64
	KindFloat64
	KindString
	KindBytes
)

// Value 是记录字段的强类型值。零值为 NULL。
type Value struct {
	kind Kind
	b    bool
	i    int64
	u    uint64
	f    float64
	s    string
	raw  []byte
}

// Record 是一条记录：字段名 -> 字段值。
type Record map[string]Value

func NullValue() Value             { return Value{} }
func BoolValue(v bool) Value       { return Value{kind: KindBool, b: v} }
func Int64Value(v int64) Value     { return Value{kind: KindInt64, i: v} }
func Uint64Value(v uint64) Value   { return Value{kind: KindUint64, u: v} }
func Float64Value(v float64) Value { return Value{kind: KindFloat64, f: v} }
func StringValue(v string) Value   { return Value{kind: KindString, s: v} }
func BytesValue(v []byte) Value {
	vv := append([]byte(nil), v...)
	return Value{kind: KindBytes, raw: vv}
}
func (v Value) Kind() Kind   { return v.kind }
func (v Value) IsNull() bool { return v.kind == KindNull }
func (v Value) Bool() (bool, bool) {
	if v.kind != KindBool {
		return false, false
	}
	return v.b, true
}
func (v Value) Int64() (int64, bool) {
	if v.kind != KindInt64 {
		return 0, false
	}
	return v.i, true
}
func (v Value) Uint64() (uint64, bool) {
	if v.kind != KindUint64 {
		return 0, false
	}
	return v.u, true
}
func (v Value) Float64() (float64, bool) {
	if v.kind != KindFloat64 {
		return 0, false
	}
	return v.f, true
}
func (v Value) String() (string, bool) {
	if v.kind != KindString {
		return "", false
	}
	return v.s, true
}
func (v Value) Bytes() ([]byte, bool) {
	if v.kind != KindBytes {
		return nil, false
	}
	return v.raw, true
}

// isNumeric 判断类型是否参与跨数值类型的数值排序。
func isNumeric(k Kind) bool {
	return k == KindInt64 || k == KindUint64 || k == KindFloat64
}

func numericFloat(v Value) *big.Float {
	switch v.kind {
	case KindInt64:
		return new(big.Float).SetInt64(v.i)
	case KindUint64:
		return new(big.Float).SetUint64(v.u)
	case KindFloat64:
		return new(big.Float).SetFloat64(v.f)
	}
	return nil
}

// valuesEqual 判断两个值是否逻辑相等。数值类型按数值比较，
// 其余类型 Kind 不同即不相等。
func valuesEqual(a, b Value) bool {
	if a.kind != b.kind {
		if isNumeric(a.kind) && isNumeric(b.kind) {
			return compareValues(a, b) == 0
		}
		return false
	}
	switch a.kind {
	case KindNull:
		return true
	case KindBool:
		return a.b == b.b
	case KindInt64:
		return a.i == b.i
	case KindUint64:
		return a.u == b.u
	case KindFloat64:
		return a.f == b.f
	case KindString:
		return a.s == b.s
	case KindBytes:
		return bytesEqual(a.raw, b.raw)
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compareValues 定义字段值的全序，返回 -1/0/1。
// 排序规则：NULL 最小；其后按 Kind 顺序；三种数值类型按数值大小混排；
// 布尔 false<true；字符串与字节串按字节序。
func compareValues(a, b Value) int {
	if a.kind == b.kind {
		switch a.kind {
		case KindNull:
			return 0
		case KindBool:
			if a.b == b.b {
				return 0
			}
			if !a.b {
				return -1
			}
			return 1
		case KindInt64:
			return compareInt64(a.i, b.i)
		case KindUint64:
			return compareUint64(a.u, b.u)
		case KindFloat64:
			return compareFloat64(a.f, b.f)
		case KindString:
			return compareBytes([]byte(a.s), []byte(b.s))
		case KindBytes:
			return compareBytes(a.raw, b.raw)
		}
	}
	if isNumeric(a.kind) && isNumeric(b.kind) {
		return numericFloat(a).Cmp(numericFloat(b))
	}
	return compareInt64(int64(a.kind), int64(b.kind))
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareUint64(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareFloat64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return compareInt64(int64(len(a)), int64(len(b)))
}

// withPrimary 返回包含主键字段的记录副本（若记录本身已含该字段则以入参 pk 为准）。
func (r Record) withPrimary(pkName string, pk Value) Record {
	cp := make(Record, len(r)+1)
	for k, v := range r {
		cp[k] = v
	}
	cp[pkName] = pk
	return cp
}
