package sindex

import (
	"errors"
	"math"
	"testing"
)

func TestReadOnlyTxnRejectsWrites(t *testing.T) {
	db := New()
	tx := db.BeginReadOnly()
	if err := tx.Insert(rec("k", "A", 1, "x")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("insert on read-only: %v", err)
	}
	if err := tx.Put(rec("k", "A", 1, "x")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("put on read-only: %v", err)
	}
	if err := tx.Delete([]byte("k")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("delete on read-only: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("read-only commit: %v", err)
	}
}

func TestReadTxnHelper(t *testing.T) {
	db := New()
	must(t, db.UpdateTxn(func(tx *Txn) error { return tx.Insert(rec("k1", "A", 1, "x")) }))
	if err := db.ReadTxn(func(tx *Txn) error {
		r, err := tx.Get([]byte("k1"))
		if err != nil {
			return err
		}
		if v, _ := r.Get("city"); v.String() != "A" {
			t.Fatalf("bad city %v", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// ReadTxn rolls back when fn errors.
	sentinel := errors.New("boom")
	if err := db.ReadTxn(func(tx *Txn) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("want propagated error, got %v", err)
	}
}

func TestTxnReadYourWritesAndDeleteShadow(t *testing.T) {
	db := New()
	must(t, db.UpdateTxn(func(tx *Txn) error { return tx.Insert(rec("k1", "A", 1, "x")) }))
	tx := db.Begin()
	if _, err := tx.Get([]byte("k1")); err != nil {
		t.Fatal(err)
	}
	must(t, tx.Delete([]byte("k1")))
	if _, err := tx.Get([]byte("k1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want NotFound after buffered delete, got %v", err)
	}
	must(t, tx.Put(rec("k1", "B", 2, "y")))
	r, err := tx.Get([]byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Get("city"); v.String() != "B" {
		t.Fatal("read-your-write failed")
	}
	must(t, tx.Commit())
	// second commit is closed
	if err := tx.Commit(); !errors.Is(err, ErrTxnClosed) {
		t.Fatalf("want ErrTxnClosed, got %v", err)
	}
}

func TestScanValidation(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "age", Fields: []string{"age"}}))
	if _, err := db.Scan(RangeOptions{IndexName: "nope"}); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("missing index: %v", err)
	}
	if _, err := db.Scan(RangeOptions{
		IndexName: "age", PrefixValues: []Value{IntValue(1)}, StartValues: []Value{IntValue(2)},
	}); err == nil {
		t.Fatal("prefix + range must be rejected")
	}
	if _, err := db.Scan(RangeOptions{
		IndexName: "age", StartValues: []Value{IntValue(1), IntValue(2)},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("too many bound values: %v", err)
	}
	if _, err := db.Scan(RangeOptions{
		IndexName: "age", StartValues: []Value{FloatValue(math.NaN())},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("NaN bound: %v", err)
	}
}

func TestIndexDefsSortedAndRebuildMissing(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "z", Fields: []string{"a"}}))
	must(t, db.CreateIndex(IndexDef{Name: "a", Fields: []string{"b"}}))
	defs := db.IndexDefs()
	if len(defs) != 2 || defs[0].Name != "a" || defs[1].Name != "z" {
		t.Fatalf("defs not sorted: %+v", defs)
	}
	if err := db.CreateIndex(IndexDef{Name: "z", Fields: []string{"c"}}); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("dup index: %v", err)
	}
	if err := db.RebuildIndex("nope"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("rebuild missing: %v", err)
	}
	if _, err := db.VerifyIndex("nope"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("verify missing: %v", err)
	}
}
