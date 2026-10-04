package sindex

import (
	"bytes"
	"math"
	"testing"
)

func TestEncodingOrderPreservation(t *testing.T) {
	vals := []Value{
		NilValue(),
		BoolValue(false), BoolValue(true),
		IntValue(math.MinInt64), IntValue(-1), IntValue(0), IntValue(1), IntValue(math.MaxInt64),
		FloatValue(-1e300), FloatValue(-2.5), FloatValue(0), FloatValue(3.14), FloatValue(1e300),
		StringValue(""), StringValue("a"), StringValue("a\x00"), StringValue("a\x00b"),
		StringValue("a\x00c"), StringValue("ab"), StringValue("b"),
		BytesValue(nil), BytesValue([]byte{0}),
		BytesValue([]byte{0, 0}), BytesValue([]byte{0, 1}), BytesValue([]byte{1}),
	}
	var prev []byte
	for i, v := range vals {
		enc := encodeValue(nil, v)
		if i > 0 {
			if bytes.Compare(enc, prev) <= 0 {
				t.Fatalf("byte order broken at %d: %x <= %x (%v)", i, enc, prev, v)
			}
			if Compare(vals[i-1], v) >= 0 {
				t.Fatalf("Compare order broken at %d", i)
			}
		}
		prev = enc
	}
}

func TestEncodingPrefixSemantics(t *testing.T) {
	lo := encodeColumnPrefix(nil, StringValue("a"))
	hi, ok := prefixUpperBound(lo)
	if !ok {
		t.Fatal("expected upper bound")
	}
	inside := []string{"a", "a\x00", "a\x00b", "a\xff", "ab", "abc"}
	outside := []string{"", "Z", "b"}
	for _, s := range inside {
		k := encodeValue(nil, StringValue(s))
		if bytes.Compare(k, lo) < 0 || bytes.Compare(k, hi) >= 0 {
			t.Fatalf("expected %q inside prefix", s)
		}
	}
	for _, s := range outside {
		k := encodeValue(nil, StringValue(s))
		if bytes.Compare(k, lo) >= 0 && bytes.Compare(k, hi) < 0 {
			t.Fatalf("expected %q outside prefix", s)
		}
	}
}

func TestPrefixUpperBoundDegenerate(t *testing.T) {
	if _, ok := prefixUpperBound([]byte{0xFF, 0xFF}); ok {
		t.Fatal("all-0xFF prefix has no finite successor")
	}
}
