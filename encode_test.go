package engine

import (
	"bytes"
	"math"
	"testing"
)

func TestEncodeOrdering(t *testing.T) {
	// 同类型值：编码字节序与 compareValues 顺序一致。
	check := func(vals []Value) {
		t.Helper()
		for i := 0; i < len(vals); i++ {
			for j := 0; j < len(vals); j++ {
				a, b := encodeKey([]Value{vals[i]}), encodeKey([]Value{vals[j]})
				got := bytes.Compare(a, b)
				want := compareValues(vals[i], vals[j])
				if (got < 0 && want >= 0) || (got > 0 && want <= 0) || (got == 0 && want != 0) {
					t.Fatalf("order mismatch for %v vs %v: bytes=%d cmp=%d", vals[i], vals[j], got, want)
				}
			}
		}
	}
	check([]Value{NullValue(), BoolValue(false), BoolValue(true)})
	check([]Value{Int64Value(math.MinInt64), Int64Value(-1), Int64Value(0), Int64Value(1), Int64Value(math.MaxInt64)})
	check([]Value{Uint64Value(0), Uint64Value(1), Uint64Value(math.MaxUint64)})
	check([]Value{Float64Value(-math.Inf(1)), Float64Value(-1.5), Float64Value(0), Float64Value(1.5), Float64Value(math.Inf(1))})
	check([]Value{StringValue(""), StringValue("a"), StringValue("a\x00b"), StringValue("a\x01b"), StringValue("ab"), StringValue("b")})
	check([]Value{BytesValue(nil), BytesValue([]byte{0}), BytesValue([]byte{1}), BytesValue([]byte{0, 0}), BytesValue([]byte{0, 1}), BytesValue([]byte{0xff})})
}

func TestEncodeKeySelfDelimiting(t *testing.T) {
	// ["a", ""] 与 ["a"] 的裸拼接相同，但加了终止符后必须可区分。
	k1 := encodeKey([]Value{StringValue("a"), StringValue("")})
	k2 := encodeKey([]Value{StringValue("a")})
	if bytes.Equal(k1, k2) {
		t.Fatal("self-delimiting failed")
	}
	parts, err := decodeKeyParts(k1)
	if err != nil || len(parts) != 2 {
		t.Fatalf("decode k1: %v %v", parts, err)
	}
	parts2, err := decodeKeyParts(k2)
	if err != nil || len(parts2) != 1 {
		t.Fatalf("decode k2: %v %v", parts2, err)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	r := Record{
		"id":  Int64Value(7),
		"s":   StringValue("hello\x00"),
		"b":   BytesValue([]byte{1, 2, 3}),
		"nil": NullValue(),
		"u":   Uint64Value(99),
		"f":   Float64Value(2.5),
		"on":  BoolValue(true),
	}
	got := decodeRecord(encodeRecord(r))
	for k, v := range r {
		if !valuesEqual(got[k], v) {
			t.Fatalf("field %s mismatch: %v vs %v", k, got[k], v)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	p := encodePrefix([]Value{StringValue("abc")})
	end := prefixEnd(p)
	if end == nil {
		t.Fatal("expected finite upper bound")
	}
	for _, s := range []string{"abc", "abcd", "abc\x00", "abc\xff"} {
		k := encodeKey([]Value{StringValue(s)})
		if bytes.Compare(k, p) < 0 || bytes.Compare(k, end) >= 0 {
			t.Fatalf("%q should be within prefix range", s)
		}
	}
	if k := encodeKey([]Value{StringValue("abd")}); bytes.Compare(k, end) < 0 {
		t.Fatal("abd should be outside prefix range")
	}
}
