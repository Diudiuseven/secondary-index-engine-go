package engine

import "testing"

func seedRecords(t *testing.T, db *DB) {
	t.Helper()
	err := db.Update(func(tx *Txn) error {
		for i := int64(1); i <= 50; i++ {
			if err := tx.Put(Record{
				"id":    Int64Value(i),
				"email": StringValue(string(rune('a'+(i%26))) + itoa(i) + "@x.com"),
				"city":  StringValue([]string{"BJ", "SH", "GZ"}[i%3]),
				"age":   Int64Value(20 + i%5),
				"extra": StringValue("x"),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func TestVerifyClean(t *testing.T) {
	db, _ := Open(testSchema())
	seedRecords(t, db)
	drifts, err := db.VerifyIndex("")
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) != 0 {
		t.Fatalf("unexpected drift on clean index: %+v", drifts)
	}
}

func TestRebuildEqualsIncremental(t *testing.T) {
	db, _ := Open(testSchema())
	seedRecords(t, db)
	// 增量维护一批增删改后的期望状态。
	db.Update(func(tx *Txn) error {
		tx.Delete(Int64Value(2))
		tx.Delete(Int64Value(30))
		tx.Put(Record{"id": Int64Value(1), "email": StringValue("updated1@x.com"),
			"city": StringValue("SZ"), "age": Int64Value(99), "extra": StringValue("y")})
		tx.Put(Record{"id": Int64Value(100), "email": StringValue("new100@x.com"),
			"city": StringValue("BJ"), "age": Int64Value(1), "extra": StringValue("z")})
		return nil
	})
	before := db.snapshot()

	if err := db.RebuildIndex(""); err != nil {
		t.Fatal(err)
	}
	after := db.snapshot()
	for name, bt := range before.indexes {
		at := after.indexes[name]
		if !treapsEqual(bt, at) {
			t.Fatalf("rebuilt index %q differs from incrementally maintained one", name)
		}
	}

	drifts, err := db.VerifyIndex("")
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) != 0 {
		t.Fatalf("drift after rebuild: %+v", drifts)
	}
}

func treapsEqual(a, b *treap) bool {
	if a.len() != b.len() {
		return false
	}
	eq := true
	a.ascIter(nil, func(k, v []byte) bool {
		bv, ok := b.get(k)
		if !ok || string(bv) != string(v) {
			eq = false
			return true
		}
		return false
	})
	return eq
}

func TestVerifyDetectsCorruption(t *testing.T) {
	db, _ := Open(testSchema())
	seedRecords(t, db)

	// 人为破坏索引：直接在当前 state 上删掉一个索引项，模拟偏差。
	st := db.snapshot()
	bad := st.clone()
	bad.gen = st.gen + 1
	spec, _ := db.schema.index("idx_email")
	var victimKey []byte
	st.primary.ascIter(nil, func(k, v []byte) bool {
		rec := decodeRecord(v)
		victimKey = buildIndexKey(spec, rec, rec["id"])
		return true
	})
	bad.indexes["idx_email"] = bad.indexes["idx_email"].delete(victimKey)
	db.state.Store(bad)

	drifts, err := db.VerifyIndex("idx_email")
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) == 0 {
		t.Fatal("expected drift to be detected")
	}
	foundMissing := false
	for _, d := range drifts {
		if d.Kind == DriftMissingEntry {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("expected missing-entry drift, got %+v", drifts)
	}

	// 重建后偏差消失。
	if err := db.RebuildIndex("idx_email"); err != nil {
		t.Fatal(err)
	}
	drifts, _ = db.VerifyIndex("idx_email")
	if len(drifts) != 0 {
		t.Fatalf("drift remains after rebuild: %+v", drifts)
	}
}
