package sindex

import "math"

// Kind enumerates the scalar value kinds supported by index key encoding.
type Kind uint8

// Supported value kinds. The numeric order here is also the global ordering
// used by the order-preserving encoder: nil < bool < int64 < float64 <
// string < bytes.
const (
	KindNil Kind = iota
	KindBool
	KindInt64
	KindFloat64
	KindString
	KindBytes
)

// Value is a typed scalar field value. The zero value is Nil.
//
// Values are comparable in the total order
//
//	Nil < Bool(false) < Bool(true) < Int64 < Float64 < String < Bytes,
//
// with int64 and float64 compared by their numeric values within their own
// kinds; a Go nil interface / missing field is treated as Nil.
type Value struct {
	kind Kind
	b    bool
	i    int64
	f    float64
	s    string
	x    []byte
}

// Kind returns the value kind.
func (v Value) Kind() Kind { return v.kind }

// Constructors -------------------------------------------------------------

// NilValue returns the nil value.
func NilValue() Value { return Value{kind: KindNil} }

// BoolValue constructs a boolean value.
func BoolValue(b bool) Value { return Value{kind: KindBool, b: b} }

// IntValue constructs an int64 value.
func IntValue(i int64) Value { return Value{kind: KindInt64, i: i} }

// FloatValue constructs a float64 value. NaN is rejected.
func FloatValue(f float64) Value { return Value{kind: KindFloat64, f: f} }

// StringValue constructs a UTF-8 string value.
func StringValue(s string) Value { return Value{kind: KindString, s: s} }

// BytesValue constructs an opaque byte-slice value. The returned value keeps
// a copy of b so callers may reuse the slice.
func BytesValue(b []byte) Value {
	cp := make([]byte, len(b))
	copy(cp, b)
	return Value{kind: KindBytes, x: cp}
}

// Accessors ----------------------------------------------------------------

func (v Value) Bool() bool     { return v.b }
func (v Value) Int() int64     { return v.i }
func (v Value) Float() float64 { return v.f }
func (v Value) String() string { return v.s }
func (v Value) Bytes() []byte  { return v.x }

// IsValid reports whether v carries a supported kind.
func (v Value) IsValid() bool { return v.kind <= KindBytes }

// Compare returns -1, 0 or 1 according to the total order documented on Value.
// Values of different kinds are ordered by kind; values never sort equal
// across kinds.
func Compare(a, b Value) int {
	if a.kind != b.kind {
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	switch a.kind {
	case KindNil:
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
		return compareOrdered(a.i, b.i)
	case KindFloat64:
		if a.f < b.f {
			return -1
		}
		if a.f > b.f {
			return 1
		}
		return 0
	case KindString:
		if a.s < b.s {
			return -1
		}
		if a.s > b.s {
			return 1
		}
		return 0
	case KindBytes:
		return compareBytes(a.x, b.x)
	default:
		return 0
	}
}

func compareOrdered[T ~int64 | ~float64](a, b T) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareBytes(a, b []byte) int {
	// Lexicographic order, with a proper prefix being smaller.
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// Less reports whether a sorts before b.
func Less(a, b Value) bool { return Compare(a, b) < 0 }

// Equal reports exact value equality (kind and value).
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// IsNaN reports whether the value is a floating-point NaN, which is not a
// legal indexable value.
func (v Value) IsNaN() bool { return v.kind == KindFloat64 && math.IsNaN(v.f) }

// Record -------------------------------------------------------------------

// Record is a stored document addressed by an opaque primary key.
type Record struct {
	key    []byte
	fields map[string]Value
}

// NewRecord constructs a record with a copy of key and the given fields.
// The fields map is copied; later mutations by the caller do not affect the
// record.
func NewRecord(key []byte, fields map[string]Value) Record {
	kc := make([]byte, len(key))
	copy(kc, key)
	fm := make(map[string]Value, len(fields))
	for k, v := range fields {
		fm[k] = cloneValue(v)
	}
	return Record{key: kc, fields: fm}
}

// Key returns a copy of the record primary key.
func (r Record) Key() []byte {
	out := make([]byte, len(r.key))
	copy(out, r.key)
	return out
}

// Get returns the named field value and whether it is present. A missing
// field is also represented by Nil with ok == false.
func (r Record) Get(name string) (Value, bool) {
	if r.fields == nil {
		return NilValue(), false
	}
	v, ok := r.fields[name]
	if !ok {
		return NilValue(), false
	}
	return v, true
}

// Field returns the value of name, or Nil when absent.
func (r Record) Field(name string) Value {
	v, ok := r.Get(name)
	if !ok {
		return NilValue()
	}
	return v
}

// Fields returns the field names carried by the record.
func (r Record) FieldNames() []string {
	out := make([]string, 0, len(r.fields))
	for k := range r.fields {
		out = append(out, k)
	}
	return out
}

func cloneValue(v Value) Value {
	if v.kind == KindBytes && v.x != nil {
		cp := make([]byte, len(v.x))
		copy(cp, v.x)
		v.x = cp
	}
	return v
}
