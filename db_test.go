package engine

import "testing"

func testSchema() *Schema {
	return &Schema{
		PrimaryKey: "id",
		Fields: []FieldSpec{
			{Name: "id", Kind: KindInt64},
			{Name: "email", Kind: KindString},
			{Name: "city", Kind: KindString},
			{Name: "age", Kind: KindInt64},
			{Name: "extra", Kind: KindString},
		},
		Indexes: []IndexSpec{
			{Name: "idx_email", Key: []string{"email"}, Unique: true},
			{Name: "idx_city_age", Key: []string{"city", "age"}, Covered: []string{"extra"}},
		},
	}
}

func TestPrimaryCRUD(t *testing.T) {
	db, err := Open(testSchema())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(1), "city": StringValue("X")})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Txn) error {
		r, err := tx.Get(Int64Value(1))
		if err != nil {
			return err
		}
		if s, _ := r["city"].String(); s != "X" {
			t.Fatalf("got %v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Txn) error { return tx.Delete(Int64Value(1)) }); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Txn) error {
		_, err := tx.Get(Int64Value(1))
		if err != ErrNotFound {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueViolation(t *testing.T) {
	db, _ := Open(testSchema())
	put := func(id int64, email string) error {
		return db.Update(func(tx *Txn) error {
			return tx.Put(Record{"id": Int64Value(id), "email": StringValue(email)})
		})
	}
	if err := put(1, "a@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := put(2, "a@x.com"); err == nil {
		t.Fatal("expected unique violation")
	}
	// 冲突写入不得留下主表痕迹。
	db.View(func(tx *Txn) error {
		if _, err := tx.Get(Int64Value(2)); err != ErrNotFound {
			t.Fatalf("conflicting record leaked into primary table: %v", err)
		}
		return nil
	})
	// 更新但唯一键不变应允许。
	if err := db.Update(func(tx *Txn) error {
		return tx.Put(Record{"id": Int64Value(1), "email": StringValue("a@x.com"), "city": StringValue("NY")})
	}); err != nil {
		t.Fatalf("update same unique key should succeed: %v", err)
	}
	// 批内唯一冲突也必须整体拒绝。
	err := db.Update(func(tx *Txn) error {
		if err := tx.Put(Record{"id": Int64Value(3), "email": StringValue("dup@x.com")}); err != nil {
			return err
		}
		return tx.Put(Record{"id": Int64Value(4), "email": StringValue("dup@x.com")})
	})
	if err == nil {
		t.Fatal("expected in-batch unique violation")
	}
	db.View(func(tx *Txn) error {
		for _, id := range []int64{3, 4} {
			if _, err := tx.Get(Int64Value(id)); err != ErrNotFound {
				t.Fatalf("rolled-back batch record %d leaked", id)
			}
		}
		return nil
	})
	// 同一事务中先删除占用者再插入同键值应成功。
	err = db.Update(func(tx *Txn) error {
		if err := tx.Delete(Int64Value(1)); err != nil {
			return err
		}
		return tx.Put(Record{"id": Int64Value(5), "email": StringValue("a@x.com")})
	})
	if err != nil {
		t.Fatalf("delete-then-insert same unique key should succeed: %v", err)
	}
}

func TestNonUniqueOrdering(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		for _, c := range []struct {
			id   int64
			city string
			age  int64
		}{
			{3, "BJ", 30}, {1, "BJ", 20}, {2, "BJ", 30}, {4, "SH", 10},
		} {
			tx.Put(Record{"id": Int64Value(c.id), "email": StringValue("e" + string(rune('0'+c.id)) + "@x.com"),
				"city": StringValue(c.city), "age": Int64Value(c.age)})
		}
		return nil
	})
	var first, second []int64
	collect := func() {
		db.View(func(tx *Txn) error {
			res, err := tx.ScanIndex(ScanOptions{
				Index:  "idx_city_age",
				Prefix: []Value{StringValue("BJ")},
				Fields: []string{"id"},
			})
			if err != nil {
				return err
			}
			var ids []int64
			for _, row := range res.Rows {
				id, _ := row.Primary.Int64()
				ids = append(ids, id)
			}
			if first == nil {
				first = ids
			} else {
				second = ids
			}
			return nil
		})
	}
	collect()
	collect()
	want := []int64{1, 2, 3} // BJ 下先按 age 再按主键
	if len(first) != 3 || !equalInt64(first, want) {
		t.Fatalf("order = %v, want %v", first, want)
	}
	if !equalInt64(first, second) {
		t.Fatalf("non-deterministic order: %v vs %v", first, second)
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRollback(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		tx.Put(Record{"id": Int64Value(1), "email": StringValue("e@x.com"), "city": StringValue("BJ")})
		return nil
	})
	db.Update(func(tx *Txn) error {
		tx.Put(Record{"id": Int64Value(2), "email": StringValue("x@y.com"), "city": StringValue("SH")})
		tx.Delete(Int64Value(1))
		return errRollbackSentinel
	})
	db.View(func(tx *Txn) error {
		if _, err := tx.Get(Int64Value(1)); err != nil {
			t.Fatalf("delete leaked after rollback: %v", err)
		}
		if _, err := tx.Get(Int64Value(2)); err != ErrNotFound {
			t.Fatalf("put leaked after rollback: %v", err)
		}
		res, err := tx.ScanIndex(ScanOptions{Index: "idx_email", Prefix: []Value{StringValue("x@y.com")}})
		if err != nil {
			return err
		}
		if len(res.Rows) != 0 {
			t.Fatalf("rolled-back index entry leaked: %d rows", len(res.Rows))
		}
		return nil
	})
}

var errRollbackSentinel = newSentinelErr()

type sentinelErr struct{}

func (sentinelErr) Error() string { return "sentinel rollback" }
func newSentinelErr() error       { return sentinelErr{} }
