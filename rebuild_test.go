package sindex

import "testing"

func seedRebuildDB(t *testing.T) *Engine {
	t.Helper()
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "email", Fields: []string{"email"}, Unique: true}))
	must(t, db.CreateIndex(IndexDef{Name: "city_age", Fields: []string{"city", "age"}, Covered: []string{"name"}}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for _, r := range []Record{
			NewRecord([]byte("k1"), map[string]Value{"email": StringValue("a@x"), "city": StringValue("BJ"), "age": IntValue(10), "name": StringValue("ann")}),
			NewRecord([]byte("k2"), map[string]Value{"email": StringValue("b@x"), "city": StringValue("SH"), "age": IntValue(20), "name": StringValue("bob")}),
			NewRecord([]byte("k3"), map[string]Value{"email": StringValue("c@x"), "city": StringValue("BJ"), "age": IntValue(30), "name": StringValue("cyd")}),
		} {
			if err := tx.Insert(r); err != nil {
				return err
			}
		}
		return nil
	}))
	return db
}

func snapshotEntries(ix *Index) [][]byte {
	out := make([][]byte, len(ix.data.entries))
	for i, e := range ix.data.entries {
		out[i] = append([]byte(nil), e.SortKey...)
	}
	return out
}

func TestRebuildMatchesIncremental(t *testing.T) {
	db := seedRebuildDB(t)
	ix, _ := db.Index("city_age")
	before := snapshotEntries(ix)

	if err := db.RebuildIndex("city_age"); err != nil {
		t.Fatal(err)
	}
	after := snapshotEntries(ix)
	if len(before) != len(after) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if compareBytes(before[i], after[i]) != 0 {
			t.Fatalf("rebuilt index differs at %d: % x vs % x", i, before[i], after[i])
		}
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("fresh rebuild reports drift: %+v", d)
	}
}

func TestCreateIndexOnPopulatedTable(t *testing.T) {
	db := New()
	must(t, db.UpdateTxn(func(tx *Txn) error {
		return tx.Insert(rec("k1", "BJ", 1, "x"))
	}))
	must(t, db.CreateIndex(IndexDef{Name: "later", Fields: []string{"city"}}))
	ix, _ := db.Index("later")
	if ix.Len() != 1 {
		t.Fatalf("new index should be built from existing rows, len=%d", ix.Len())
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("drift: %+v", d)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	db := seedRebuildDB(t)
	ix, _ := db.Index("city_age")

	// (a) inject an extra entry pointing at a missing record.
	bogus := Entry{
		SortKey:   encodeKey(append([]byte(nil), ix.data.entries[0].SortKey...), nil),
		RecordKey: []byte("ghost"),
	}
	// Build a well-formed bogus key: city=ZZ,age=99 + sep + ghost pk.
	cols := encodeKey(nil, []Value{StringValue("ZZ"), IntValue(99)})
	bogus.SortKey = entrySortKey(cols, []byte("ghost"))
	ix.data.insert(bogus)

	// (b) remove a legitimate entry (missing_entry).
	victim := ix.data.entries[0].SortKey
	ix.data.remove(victim)

	drifts, err := db.VerifyIndex("city_age")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[DriftKind]int{}
	for _, d := range drifts {
		kinds[d.Kind]++
	}
	if kinds[DriftExtraEntry] == 0 {
		t.Fatalf("expected extra-entry drift, got %+v", drifts)
	}
	if kinds[DriftMissingEntry] == 0 {
		t.Fatalf("expected missing-entry drift, got %+v", drifts)
	}

	// Rebuilding must repair everything.
	if err := db.RebuildAllIndexes(); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("rebuild did not repair drift: %+v", d)
	}
}

func TestVerifyDetectsUniqueDuplicate(t *testing.T) {
	db := seedRebuildDB(t)
	ix, _ := db.Index("email")
	// Physically force a second entry with the same encoded email.
	base := ix.data.entries[0]
	dup := cloneEntry(base)
	dup.RecordKey = []byte("rogue")
	dup.SortKey = entrySortKey(base.SortKey[:len(base.SortKey)-len(base.RecordKey)-1], dup.RecordKey)
	ix.data.insert(dup)
	drifts, err := db.VerifyIndex("email")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range drifts {
		if d.Kind == DriftDuplicateUnique {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected duplicate-unique drift, got %+v", drifts)
	}
}
