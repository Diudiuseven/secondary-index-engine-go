package sindex

import (
	"fmt"
	"testing"
)

func seedQueryDB(t *testing.T) *Engine {
	t.Helper()
	db := New()
	// city, age index with "name" as a covering column.
	must(t, db.CreateIndex(IndexDef{
		Name:    "city_age",
		Fields:  []string{"city", "age"},
		Covered: []string{"name"},
	}))
	must(t, db.CreateIndex(IndexDef{Name: "age", Fields: []string{"age"}}))
	rows := []struct {
		key, city, name string
		age             int64
	}{
		{"k1", "BJ", "ann", 20},
		{"k2", "BJ", "bob", 30},
		{"k3", "BJ", "cyd", 40},
		{"k4", "SH", "dan", 20},
		{"k5", "SH", "eve", 35},
		{"k6", "GZ", "fay", 50},
	}
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for _, r := range rows {
			err := tx.Insert(NewRecord([]byte(r.key), map[string]Value{
				"city": StringValue(r.city), "age": IntValue(r.age), "name": StringValue(r.name),
			}))
			if err != nil {
				return err
			}
		}
		return nil
	}))
	return db
}

func keysOf(res ScanResult) []string {
	out := make([]string, 0, len(res.Records))
	for _, r := range res.Records {
		out = append(out, string(r.Key()))
	}
	return out
}

func TestPrefixScan(t *testing.T) {
	db := seedQueryDB(t)
	res, err := db.Scan(RangeOptions{
		IndexName:    "city_age",
		PrefixValues: []Value{StringValue("BJ")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"k1", "k2", "k3"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prefix=%v want %v", got, want)
	}
}

func TestRangeBoundsInclusiveExclusive(t *testing.T) {
	db := seedQueryDB(t)
	// age in [30, 40] on single-column index -> 30(k2),35(k5),40(k3)
	res, err := db.Scan(RangeOptions{
		IndexName:   "age",
		StartValues: []Value{IntValue(30)}, StartBound: Inclusive,
		EndValues: []Value{IntValue(40)}, EndBound: Inclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"k2", "k5", "k3"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("inclusive range=%v want %v", got, want)
	}

	// age in (30, 40) -> 35 only
	res, err = db.Scan(RangeOptions{
		IndexName:   "age",
		StartValues: []Value{IntValue(30)}, StartBound: Exclusive,
		EndValues: []Value{IntValue(40)}, EndBound: Exclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"k5"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exclusive range=%v want %v", got, want)
	}

	// age >= 40 -> 40(k3), 50(k6)
	res, err = db.Scan(RangeOptions{
		IndexName:   "age",
		StartValues: []Value{IntValue(40)}, StartBound: Inclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"k3", "k6"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("open end range=%v want %v", got, want)
	}

	// age < 30 -> 20,20 (k1,k4)
	res, err = db.Scan(RangeOptions{
		IndexName: "age",
		EndValues: []Value{IntValue(30)}, EndBound: Exclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(res), []string{"k1", "k4"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("open start range=%v want %v", got, want)
	}
}

func TestCompositePrefixRange(t *testing.T) {
	db := seedQueryDB(t)
	// (BJ, age >= 30): partial lower tuple on leading columns via prefix city=BJ
	// and full range end on age. Use range mode with both columns.
	res, err := db.Scan(RangeOptions{
		IndexName:   "city_age",
		StartValues: []Value{StringValue("BJ"), IntValue(30)}, StartBound: Inclusive,
		EndValues: []Value{StringValue("BJ")}, EndBound: Exclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// BJ/30=k2, BJ/40=k3; SH excluded
	if got, want := keysOf(res), []string{"k2", "k3"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("composite range=%v want %v", got, want)
	}
}

func TestCoveringScanNoTableLookup(t *testing.T) {
	db := seedQueryDB(t)
	res, err := db.Scan(RangeOptions{
		IndexName:    "city_age",
		PrefixValues: []Value{StringValue("BJ")},
		Fields:       []string{"city", "age", "name"}, // all index/covered + pk
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Covered || res.TableLookups != 0 || res.IndexItems != 3 {
		t.Fatalf("expected fully covered: %+v", res)
	}
	// Verify projected values came from index.
	r0 := res.Records[0]
	if v, _ := r0.Get("name"); v.String() != "ann" {
		t.Fatalf("covered name wrong: %v", v)
	}
	if v, _ := r0.Get("age"); v.Int() != 20 {
		t.Fatalf("covered age wrong: %v", v)
	}
	if len(r0.FieldNames()) != 3 {
		t.Fatalf("projected record should have 3 fields, got %v", r0.FieldNames())
	}
}

func TestNonCoveringScanDoesTableLookup(t *testing.T) {
	db := seedQueryDB(t)
	res, err := db.Scan(RangeOptions{
		IndexName:    "city_age",
		PrefixValues: []Value{StringValue("BJ")},
		Fields:       []string{"name", "secret"}, // secret not in index
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Covered {
		t.Fatal("scan requesting non-indexed field must not be covered")
	}
	if res.TableLookups != 3 {
		t.Fatalf("want 3 table lookups, got %d", res.TableLookups)
	}
}

func TestScanLimit(t *testing.T) {
	db := seedQueryDB(t)
	res, err := db.Scan(RangeOptions{IndexName: "age", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 || !res.Limited {
		t.Fatalf("limit not honoured: %+v", res)
	}
}
