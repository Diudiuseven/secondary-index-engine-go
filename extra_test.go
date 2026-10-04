package sindex

import (
	"errors"
	"fmt"
	"testing"
)

func TestCompositeNonUniqueTieBreak(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "ca", Fields: []string{"city", "age"}}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		// identical (BJ,30) for three keys inserted out of order.
		for _, k := range []string{"z9", "a1", "m5"} {
			if err := tx.Insert(rec(k, "BJ", 30, "x")); err != nil {
				return err
			}
		}
		return tx.Insert(rec("n0", "BJ", 29, "x"))
	}))
	res, err := db.Scan(RangeOptions{
		IndexName:    "ca",
		PrefixValues: []Value{StringValue("BJ"), IntValue(30)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"a1", "m5", "z9"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tie-break=%v want %v", got, want)
	}
}

func TestDeleteThenReuseUniqueValue(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "u", Fields: []string{"v"}, Unique: true}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		return tx.Insert(NewRecord([]byte("k1"), map[string]Value{"v": StringValue("x")}))
	}))
	// Delete then insert same value with a new key in one txn.
	if err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Delete([]byte("k1")); err != nil {
			return err
		}
		return tx.Insert(NewRecord([]byte("k2"), map[string]Value{"v": StringValue("x")}))
	}); err != nil {
		t.Fatalf("delete+reuse should succeed: %v", err)
	}
	if _, ok := db.Get([]byte("k1")); ok {
		t.Fatal("k1 should be gone")
	}
	if _, ok := db.Get([]byte("k2")); !ok {
		t.Fatal("k2 should exist")
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("drift: %+v", d)
	}
	// Reusing the value AGAIN without delete must conflict.
	err := db.UpdateTxn(func(tx *Txn) error {
		return tx.Insert(NewRecord([]byte("k3"), map[string]Value{"v": StringValue("x")}))
	})
	if !errors.Is(err, ErrUniqueConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestDropIndexDoesNotTouchTable(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "a", Fields: []string{"city"}}))
	must(t, db.CreateIndex(IndexDef{Name: "b", Fields: []string{"age"}}))
	must(t, db.UpdateTxn(func(tx *Txn) error { return tx.Insert(rec("k1", "BJ", 1, "x")) }))
	if err := db.DropIndex("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Index("a"); ok {
		t.Fatal("index a should be gone")
	}
	if db.Len() != 1 {
		t.Fatal("dropping index changed table")
	}
	if d, _ := db.VerifyIndex("b"); len(d) != 0 {
		t.Fatalf("other index drifted: %+v", d)
	}
	if err := db.DropIndex("nope"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("want ErrIndexNotFound, got %v", err)
	}
}

func TestInvalidDefs(t *testing.T) {
	db := New()
	for _, def := range []IndexDef{
		{Name: "", Fields: []string{"a"}},
		{Name: "x", Fields: nil},
		{Name: "x", Fields: []string{"a", "a"}},
		{Name: "x", Fields: []string{"a"}, Covered: []string{""}},
	} {
		if err := db.CreateIndex(def); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("def=%+v want ErrInvalidArgument, got %v", def, err)
		}
	}
}
