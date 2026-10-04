package sindex

import (
	"fmt"
	"testing"
)

func TestStringPrefixWithinColumn(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "name", Fields: []string{"name"}}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for i, n := range []string{"a", "abc", "ab", "abd", "b", "Az", "aaa"} {
			if err := tx.Insert(NewRecord([]byte(fmt.Sprintf("k%d", i)),
				map[string]Value{"name": StringValue(n)})); err != nil {
				return err
			}
		}
		return nil
	}))
	// Prefix "ab" within the single string column: ab, abc, abd (ordered by
	// full value, then pk).
	res, err := db.Scan(RangeOptions{
		IndexName:    "name",
		PrefixValues: []Value{StringValue("ab")},
		Fields:       []string{"name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := keysOf(res)
	want := []string{"k2", "k1", "k3"} // ab, abc, abd in insertion labels
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("within-column prefix=%v want %v", got, want)
	}
	if !res.Covered || res.TableLookups != 0 {
		t.Fatalf("prefix scan should be covered: %+v", res)
	}

	// Prefix "a" covers a, aaa, ab, abc, abd but NOT Az (capital A) or b.
	res, err = db.Scan(RangeOptions{
		IndexName:    "name",
		PrefixValues: []Value{StringValue("a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got = keysOf(res)
	want = []string{"k0", "k6", "k2", "k1", "k3"} // a, aaa, ab, abc, abd
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("a-prefix=%v want %v", got, want)
	}
}
