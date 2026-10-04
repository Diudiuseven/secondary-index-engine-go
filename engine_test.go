package sindex

import (
	"fmt"
	"testing"
)

func rec(key, city string, age int64, dept string) Record {
	return NewRecord([]byte(key), map[string]Value{
		"city": StringValue(city),
		"age":  IntValue(age),
		"dept": StringValue(dept),
	})
}

func TestBasicCRUDMaintainsIndex(t *testing.T) {
	db := New()
	if err := db.CreateIndex(IndexDef{Name: "city_age", Fields: []string{"city", "age"}}); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	must(db.UpdateTxn(func(tx *Txn) error {
		must(tx.Insert(rec("k1", "BJ", 30, "eng")))
		must(tx.Insert(rec("k2", "BJ", 25, "sales")))
		must(tx.Insert(rec("k3", "SH", 40, "eng")))
		return nil
	}))
	ix, _ := db.Index("city_age")
	if ix.Len() != 3 || db.Len() != 3 {
		t.Fatalf("counts ix=%d table=%d", ix.Len(), db.Len())
	}

	// Update moves the entry.
	must(db.UpdateTxn(func(tx *Txn) error {
		return tx.Put(rec("k2", "SH", 25, "sales"))
	}))
	if d, _ := db.VerifyIndex("city_age"); len(d) != 0 {
		t.Fatalf("drift after update: %+v", d)
	}

	// Delete removes the entry.
	must(db.UpdateTxn(func(tx *Txn) error { return tx.Delete([]byte("k3")) }))
	if d, _ := db.VerifyIndex("city_age"); len(d) != 0 {
		t.Fatalf("drift after delete: %+v", d)
	}
	if ix.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", ix.Len())
	}
}

func TestNonUniqueStableOrder(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "city", Fields: []string{"city"}}))
	keys := []string{"zeta", "alpha", "mid", "beta", "gamma"}
	err := db.UpdateTxn(func(tx *Txn) error {
		for i, k := range keys {
			if err := tx.Insert(rec(k, "BJ", int64(i), "x")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Scan(RangeOptions{
		IndexName:    "city",
		PrefixValues: []Value{StringValue("BJ")},
		Fields:       []string{"city"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(res.Records))
	for _, r := range res.Records {
		got = append(got, string(r.Key()))
	}
	// Same index value -> ascending primary key, stable and reproducible.
	want := []string{"alpha", "beta", "gamma", "mid", "zeta"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v want %v", got, want)
	}

	// Repeat and confirm identical order.
	res2, _ := db.Scan(RangeOptions{IndexName: "city", PrefixValues: []Value{StringValue("BJ")}})
	got2 := make([]string, 0, len(res2.Records))
	for _, r := range res2.Records {
		got2 = append(got2, string(r.Key()))
	}
	if fmt.Sprint(got2) != fmt.Sprint(want) {
		t.Fatalf("non-reproducible order: %v", got2)
	}
}

func TestInsertDuplicatePKRejected(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "city", Fields: []string{"city"}, Unique: true}))
	err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Insert(rec("k1", "BJ", 1, "x")); err != nil {
			return err
		}
		return tx.Insert(rec("k1", "SH", 2, "y"))
	})
	if err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if db.Len() != 0 {
		t.Fatal("rolled-back txn left data in table")
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
