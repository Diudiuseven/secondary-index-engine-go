package sindex

import (
	"fmt"
	"testing"
)

func TestTableOrderedScan(t *testing.T) {
	db := New()
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for _, k := range []string{"k9", "k2", "k5", "k1", "k7"} {
			if err := tx.Insert(rec(k, "BJ", 1, "x")); err != nil {
				return err
			}
		}
		return nil
	}))
	var got []string
	db.ScanTable(nil, nil, func(r Record) bool {
		got = append(got, string(r.Key()))
		return true
	})
	want := []string{"k1", "k2", "k5", "k7", "k9"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("table order=%v want %v", got, want)
	}

	// Half-open range [k2, k7) -> k2, k5
	got = nil
	db.ScanTable([]byte("k2"), []byte("k7"), func(r Record) bool {
		got = append(got, string(r.Key()))
		return true
	})
	if fmt.Sprint(got) != fmt.Sprint([]string{"k2", "k5"}) {
		t.Fatalf("range order=%v", got)
	}
}

func TestVersionBumpsOnCommit(t *testing.T) {
	db := New()
	v0 := db.Version()
	if v0 != 0 {
		t.Fatalf("initial version=%d", v0)
	}
	must(t, db.UpdateTxn(func(tx *Txn) error { return tx.Insert(rec("k1", "A", 1, "x")) }))
	if db.Version() != 1 {
		t.Fatalf("version after insert=%d", db.Version())
	}
	// A rolled-back txn must not bump the version.
	tx := db.Begin()
	must(t, tx.Insert(rec("k2", "B", 2, "y")))
	tx.Rollback()
	if db.Version() != 1 {
		t.Fatalf("rollback changed version to %d", db.Version())
	}
}

func TestMissingFieldIsNil(t *testing.T) {
	r := NewRecord([]byte("k"), map[string]Value{"a": IntValue(1)})
	if v, ok := r.Get("missing"); ok || v.Kind() != KindNil {
		t.Fatal("missing field should be Nil with ok=false")
	}
}
