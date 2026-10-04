package engine

import "testing"

func TestIndexReflectsUpdateAndDelete(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(1), "email": StringValue("old@x.com"),
			"city": StringValue("BJ"), "age": Int64Value(20), "extra": StringValue("p")})
	})
	// 更新：旧索引项消失，新索引项出现；覆盖列随之更新。
	db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(1), "email": StringValue("new@x.com"),
			"city": StringValue("SH"), "age": Int64Value(40), "extra": StringValue("q")})
	})
	db.View(func(tx *Txn) error {
		if r, err := tx.ScanIndex(ScanOptions{Index: "idx_email", Prefix: []Value{StringValue("old@x.com")}}); err != nil || len(r.Rows) != 0 {
			t.Fatalf("old unique entry still present: %d rows, err=%v", len(r.Rows), err)
		}
		r, err := tx.ScanIndex(ScanOptions{Index: "idx_city_age", Prefix: []Value{StringValue("SH")},
			Fields: []string{"id", "extra"}})
		if err != nil {
			return err
		}
		if len(r.Rows) != 1 {
			t.Fatalf("new entry missing: %d", len(r.Rows))
		}
		if s, _ := r.Rows[0].Record["extra"].String(); s != "q" {
			t.Fatalf("covered column not updated: %q", s)
		}
		return nil
	})
	// 删除：索引项全部消失。
	db.Update(func(tx *Txn) error { return tx.Delete(Int64Value(1)) })
	db.View(func(tx *Txn) error {
		for _, in := range []string{"idx_email", "idx_city_age"} {
			r, err := tx.ScanIndex(ScanOptions{Index: in})
			if err != nil {
				return err
			}
			if len(r.Rows) != 0 {
				t.Fatalf("index %s still has %d rows after delete", in, len(r.Rows))
			}
		}
		return nil
	})
	drifts, _ := db.VerifyIndex("")
	if len(drifts) != 0 {
		t.Fatalf("drift after delete: %+v", drifts)
	}
}

func TestUniqueCoveredScan(t *testing.T) {
	schema := &Schema{
		PrimaryKey: "id",
		Fields: []FieldSpec{
			{Name: "id", Kind: KindInt64},
			{Name: "login", Kind: KindString},
			{Name: "display", Kind: KindString},
		},
		Indexes: []IndexSpec{{Name: "uq_login", Key: []string{"login"}, Unique: true, Covered: []string{"display"}}},
	}
	db, _ := Open(schema)
	db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(42), "login": StringValue("alice"), "display": StringValue("Alice")})
	})
	db.View(func(tx *Txn) error {
		r, err := tx.ScanIndex(ScanOptions{Index: "uq_login", Prefix: []Value{StringValue("alice")},
			Fields: []string{"id", "login", "display"}})
		if err != nil {
			return err
		}
		if !r.Covered || r.TableLookups != 0 {
			t.Fatalf("unique index should cover scan: covered=%v lookups=%d", r.Covered, r.TableLookups)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("rows=%d", len(r.Rows))
		}
		if id, _ := r.Rows[0].Primary.Int64(); id != 42 {
			t.Fatalf("pk from unique index entry = %d", id)
		}
		if d, _ := r.Rows[0].Record["display"].String(); d != "Alice" {
			t.Fatalf("covered display=%q", d)
		}
		return nil
	})
}

func TestNullUniqueSemantics(t *testing.T) {
	db, _ := Open(testSchema())
	// email=NULL 视为一个普通的可比较值：唯一索引中 NULL 至多出现一次。
	db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(1), "city": StringValue("BJ")})
	})
	err := db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(2), "city": StringValue("SH")})
	})
	if err == nil {
		t.Fatal("two NULLs in a unique index must conflict")
	}
	// 扫描 NULL 等值前缀应能命中唯一一条。
	db.View(func(tx *Txn) error {
		r, err := tx.ScanIndex(ScanOptions{Index: "idx_email", Prefix: []Value{NullValue()}})
		if err != nil {
			return err
		}
		if len(r.Rows) != 1 {
			t.Fatalf("null prefix scan rows=%d", len(r.Rows))
		}
		return nil
	})
}

func TestEmptyTransaction(t *testing.T) {
	db, _ := Open(testSchema())
	if err := db.Update(func(tx *Txn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("double commit must fail")
	}
}
