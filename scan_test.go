package engine

import "testing"

func TestCoveredScan(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		tx.Put(Record{"id": Int64Value(1), "email": StringValue("a@x.com"),
			"city": StringValue("BJ"), "age": Int64Value(20), "extra": StringValue("phone-1")})
		tx.Put(Record{"id": Int64Value(2), "email": StringValue("b@x.com"),
			"city": StringValue("BJ"), "age": Int64Value(30), "extra": StringValue("phone-2")})
		return nil
	})

	db.View(func(tx *Txn) error {
		// city/age 是索引键，extra 是覆盖列，id 是主键：全部可覆盖。
		res, err := tx.ScanIndex(ScanOptions{
			Index:  "idx_city_age",
			Prefix: []Value{StringValue("BJ")},
			Fields: []string{"id", "city", "age", "extra"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Covered || res.TableLookups != 0 {
			t.Fatalf("expected covered zero-lookup scan, got covered=%v lookups=%d", res.Covered, res.TableLookups)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("rows=%d", len(res.Rows))
		}
		first := res.Rows[0].Record
		if s, _ := first["extra"].String(); s != "phone-1" {
			t.Fatalf("covered column extra=%q", s)
		}

		// 请求未覆盖字段 email，必须回表。
		res2, err := tx.ScanIndex(ScanOptions{
			Index:  "idx_city_age",
			Prefix: []Value{StringValue("BJ")},
			Fields: []string{"id", "email"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res2.Covered || res2.TableLookups != 2 {
			t.Fatalf("expected 2 table lookups, got covered=%v lookups=%d", res2.Covered, res2.TableLookups)
		}
		if e, _ := res2.Rows[0].Record["email"].String(); e != "a@x.com" {
			t.Fatalf("lookup content wrong: %q", e)
		}
		return nil
	})
}

func TestRangeBounds(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		for _, c := range []struct{ id, age int64 }{
			{10, 10}, {20, 20}, {30, 30}, {40, 40},
		} {
			tx.Put(Record{"id": Int64Value(c.id), "email": StringValue("e@x.com"), // unique email conflict!
				"city": StringValue("C"), "age": Int64Value(c.age)})
		}
		return nil
	})
	// email 重复会导致提交失败；改用不同 email 重建。
	db, _ = Open(testSchema())
	db.Update(func(tx *Txn) error {
		for _, c := range []struct{ id, age int64 }{
			{10, 10}, {20, 20}, {30, 30}, {40, 40},
		} {
			tx.Put(Record{"id": Int64Value(c.id), "email": StringValue("e" + string(rune('a'+c.id)) + "@x.com"),
				"city": StringValue("C"), "age": Int64Value(c.age)})
		}
		return nil
	})
	scan := func(start, end *Bound) []int64 {
		var ids []int64
		err := db.View(func(tx *Txn) error {
			res, err := tx.ScanIndex(ScanOptions{
				Index: "idx_city_age", Prefix: []Value{StringValue("C")},
				Start: start, End: end, Fields: []string{"id"},
			})
			if err != nil {
				return err
			}
			for _, r := range res.Rows {
				id, _ := r.Primary.Int64()
				ids = append(ids, id)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	age := func(v int64) Value { return Int64Value(v) }

	// [20, 30]
	got := scan(&Bound{Value: age(20)}, &Bound{Value: age(30)})
	if !equalInt64(got, []int64{20, 30}) {
		t.Fatalf("inclusive range = %v", got)
	}
	// (20, 40)
	got = scan(&Bound{Value: age(20), Strict: true}, &Bound{Value: age(40), Strict: true})
	if !equalInt64(got, []int64{30}) {
		t.Fatalf("exclusive range = %v", got)
	}
	// >= 30，无上界
	got = scan(&Bound{Value: age(30)}, nil)
	if !equalInt64(got, []int64{30, 40}) {
		t.Fatalf("start-only range = %v", got)
	}
	// < 30
	got = scan(nil, &Bound{Value: age(30), Strict: true})
	if !equalInt64(got, []int64{10, 20}) {
		t.Fatalf("end-only strict range = %v", got)
	}
}

func TestPrefixScan(t *testing.T) {
	schema := &Schema{
		PrimaryKey: "id",
		Fields: []FieldSpec{
			{Name: "id", Kind: KindInt64},
			{Name: "name", Kind: KindString},
		},
		Indexes: []IndexSpec{{Name: "idx_name", Key: []string{"name"}}},
	}
	db, _ := Open(schema)
	db.Update(func(tx *Txn) error {
		for i, n := range []string{"abc", "abcd", "abce", "abd", "x"} {
			tx.Put(Record{"id": Int64Value(int64(i + 1)), "name": StringValue(n)})
		}
		return nil
	})
	var names []string
	db.View(func(tx *Txn) error {
		res, err := tx.ScanIndex(ScanOptions{
			Index:  "idx_name",
			Start:  &Bound{Value: StringValue("abc"), Prefix: true},
			End:    &Bound{Value: StringValue("abc"), Prefix: true},
			Fields: []string{"name"},
		})
		if err != nil {
			return err
		}
		if !res.Covered {
			t.Fatal("name+id should be covered")
		}
		for _, r := range res.Rows {
			s, _ := r.Record["name"].String()
			names = append(names, s)
		}
		return nil
	})
	if len(names) != 3 || names[0] != "abc" || names[1] != "abcd" || names[2] != "abce" {
		t.Fatalf("prefix scan = %v", names)
	}
}

func TestPrimaryScan(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		for i := int64(3); i >= 1; i-- {
			tx.Put(Record{"id": Int64Value(i * 10), "email": StringValue("e@x.com")})
		}
		return nil
	})
	// 上面 email 重复只会留下最后一条提交；换唯一 email 重测。
	db, _ = Open(testSchema())
	db.Update(func(tx *Txn) error {
		for i := int64(3); i >= 1; i-- {
			tx.Put(Record{"id": Int64Value(i * 10), "email": StringValue(string(rune('a'+i)) + "@x.com")})
		}
		return nil
	})
	recs, err := db.Begin().ScanPrimary(PrimaryScanOptions{
		Start: &Bound{Value: Int64Value(15)},
		End:   &Bound{Value: Int64Value(25)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("primary scan rows=%d", len(recs))
	}
	recs, _ = db.Begin().ScanPrimary(PrimaryScanOptions{})
	if len(recs) != 3 {
		t.Fatalf("full primary scan rows=%d", len(recs))
	}
}
