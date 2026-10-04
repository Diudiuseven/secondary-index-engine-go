package engine

import "testing"

func TestCompositeFullKeyAndLimit(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		data := []struct {
			id   int64
			city string
			age  int64
		}{
			{1, "BJ", 20}, {2, "BJ", 20}, {3, "BJ", 30}, {4, "BJ", 30}, {5, "SH", 20},
		}
		for _, d := range data {
			tx.Put(Record{"id": Int64Value(d.id), "city": StringValue(d.city),
				"age": Int64Value(d.age), "email": StringValue("e" + itoa(d.id) + "@x.com")})
		}
		return nil
	})
	ids := func(opts ScanOptions) []int64 {
		t.Helper()
		var out []int64
		err := db.View(func(tx *Txn) error {
			r, err := tx.ScanIndex(opts)
			if err != nil {
				return err
			}
			for _, row := range r.Rows {
				id, _ := row.Primary.Int64()
				out = append(out, id)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// 完整复合键前缀：city=BJ,age=20 → 主键 1,2（按 PK 稳定排序）。
	got := ids(ScanOptions{Index: "idx_city_age",
		Prefix: []Value{StringValue("BJ"), Int64Value(20)}})
	if !equalInt64(got, []int64{1, 2}) {
		t.Fatalf("full key prefix = %v", got)
	}
	// city=BJ 全前缀。
	got = ids(ScanOptions{Index: "idx_city_age", Prefix: []Value{StringValue("BJ")}})
	if !equalInt64(got, []int64{1, 2, 3, 4}) {
		t.Fatalf("one-col prefix = %v", got)
	}
	// Limit 稳定截断。
	got = ids(ScanOptions{Index: "idx_city_age", Prefix: []Value{StringValue("BJ")}, Limit: 3})
	if !equalInt64(got, []int64{1, 2, 3}) {
		t.Fatalf("limit = %v", got)
	}
	// 在 age 列上 (20, 30]：只含 age=30 的两条（20 整段被排除）。
	got = ids(ScanOptions{Index: "idx_city_age", Prefix: []Value{StringValue("BJ")},
		Start: &Bound{Value: Int64Value(20), Strict: true},
		End:   &Bound{Value: Int64Value(30)}})
	if !equalInt64(got, []int64{3, 4}) {
		t.Fatalf("(20,30] = %v", got)
	}
	// 只给下界 age>=30，不应越过 BJ 前缀读到 SH。
	got = ids(ScanOptions{Index: "idx_city_age", Prefix: []Value{StringValue("BJ")},
		Start: &Bound{Value: Int64Value(30)}})
	if !equalInt64(got, []int64{3, 4}) {
		t.Fatalf("start-only must stay within prefix, got %v", got)
	}
}
