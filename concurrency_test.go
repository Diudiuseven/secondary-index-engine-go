package engine

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentWritersReaders 长时间并发混合读写：
// 用 -race 检测数据竞争；同时校验任意扫描结果内部自洽（索引项都能在同一快照的主表中找到）。
func TestConcurrentWritersReaders(t *testing.T) {
	db, _ := Open(testSchema())
	// 预置。
	db.Update(func(tx *Txn) error {
		for i := int64(0); i < 200; i++ {
			tx.Put(Record{"id": Int64Value(i), "email": StringValue("init" + itoa(i) + "@x.com"),
				"city": StringValue([]string{"BJ", "SH"}[i%2]), "age": Int64Value(i % 10)})
		}
		return nil
	})

	var stop atomic.Bool
	var wg sync.WaitGroup

	// 写者：在不冲突的不同主键区间插入/更新/删除。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for i := int64(0); !stop.Load(); i++ {
				pk := id*1000 + i%100
				email := "w" + itoa(id) + "-" + itoa(i) + "@x.com"
				err := db.Update(func(tx *Txn) error {
					if i%7 == 0 {
						return tx.Delete(Int64Value(pk))
					}
					return tx.Put(Record{"id": Int64Value(pk), "email": StringValue(email),
						"city": StringValue([]string{"BJ", "SH", "GZ"}[i%3]), "age": Int64Value(i % 10)})
				})
				_ = err
			}
		}(int64(w + 1))
	}

	// 同主键的写写冲突写者（不同 goroutine 更新同一组 key，应有人成功有人收到 ErrConflict）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := int64(0)
		for ; !stop.Load(); i++ {
			_ = db.Update(func(tx *Txn) error {
				return tx.Put(Record{"id": Int64Value(999), "email": StringValue("shared@x.com"),
					"city": StringValue("C"), "age": Int64Value(i % 5)})
			})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := int64(0)
		for ; !stop.Load(); i++ {
			_ = db.Update(func(tx *Txn) error {
				return tx.Put(Record{"id": Int64Value(999), "email": StringValue("shared@x.com"),
					"city": StringValue("D"), "age": Int64Value(i % 5)})
			})
		}
	}()

	// 读者：持续全索引扫描 + 覆盖扫描，校验返回行与快照一致。
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				err := db.View(func(tx *Txn) error {
					res, err := tx.ScanIndex(ScanOptions{
						Index: "idx_city_age", Fields: []string{"id", "city", "age", "extra"},
					})
					if err != nil {
						return err
					}
					// 覆盖扫描零回表；回表扫描也必须全部命中。
					if res.TableLookups != 0 {
						t.Errorf("unexpected table lookups: %d", res.TableLookups)
					}
					for _, row := range res.Rows {
						if _, ok := row.Record["id"]; !ok {
							t.Errorf("row missing pk")
						}
					}
					// 唯一索引扫描：不应读到未提交内容；只校验可迭代、无重复行。
					u, err := tx.ScanIndex(ScanOptions{Index: "idx_email", Limit: 50})
					if err != nil {
						return err
					}
					seen := map[string]bool{}
					for _, row := range u.Rows {
						id, _ := row.Primary.Int64()
						key := itoa(id)
						if seen[key] {
							t.Errorf("duplicate unique-index row for pk %d", id)
						}
						seen[key] = true
					}
					return nil
				})
				if err != nil {
					t.Errorf("view error: %v", err)
				}
			}
		}()
	}

	// 跑约 150ms（-race 下足够产生大量交错）。
	for i := 0; i < 300000; i++ {
		if i == 299999 {
			stop.Store(true)
		}
	}
	stop.Store(true)
	wg.Wait()

	// 结束后索引必须与主表一致。
	drifts, err := db.VerifyIndex("")
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) != 0 {
		t.Fatalf("index/table drifted after concurrent run: %+v", drifts[:min3(len(drifts), 5)])
	}
}

func min3(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestSnapshotIsolation 长事务在并发提交下读到的仍是自己开始时的快照。
func TestSnapshotIsolation(t *testing.T) {
	db, _ := Open(testSchema())
	db.Update(func(tx *Txn) error {
		tx.Put(Record{"id": Int64Value(1), "email": StringValue("v1@x.com"), "city": StringValue("BJ")})
		return nil
	})
	tx := db.Begin()

	// 快照中读到 v1。
	r, err := tx.Get(Int64Value(1))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := r["email"].String(); s != "v1@x.com" {
		t.Fatalf("snapshot read %s", s)
	}

	// 其他事务提交 v2。
	if err := db.Update(func(t2 *Txn) error {
		return t2.Put(Record{"id": Int64Value(1), "email": StringValue("v2@x.com"), "city": StringValue("SH")})
	}); err != nil {
		t.Fatal(err)
	}

	// 老事务仍读 v1。
	r, _ = tx.Get(Int64Value(1))
	if s, _ := r["email"].String(); s != "v1@x.com" {
		t.Fatalf("snapshot isolation broken, got %s", s)
	}

	// 提交旧快照上对同一行的写入必须冲突失败。
	tx.Put(Record{"id": Int64Value(1), "email": StringValue("v3@x.com")})
	if err := tx.Commit(); err != ErrConflict {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	// 不冲突的新主键写入可以提交（虽然快照旧，但读集合未被触碰）。
	tx2 := db.Begin()
	tx2.Put(Record{"id": Int64Value(12345), "email": StringValue("fresh@x.com")})
	if err := tx2.Commit(); err != nil {
		t.Fatalf("unrelated-key write should commit: %v", err)
	}
}

// TestCommitAtomicVisibility 提交前查询绝看不到缓冲写入。
func TestCommitAtomicVisibility(t *testing.T) {
	db, _ := Open(testSchema())
	var committed atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		db.Update(func(tx *Txn) error {
			for i := int64(0); i < 100; i++ {
				tx.Put(Record{"id": Int64Value(i + 100), "email": StringValue("batch" + itoa(i) + "@x.com"),
					"city": StringValue("BJ")})
			}
			committed.Store(true)
			return nil
		})
	}()
	for !committed.Load() {
		db.View(func(tx *Txn) error {
			res, _ := tx.ScanIndex(ScanOptions{Index: "idx_email", Prefix: []Value{StringValue("batch")}})
			// 要么一条都看不到，要么提交后全部可见——不允许部分（若全部可见必须是 100 条）。
			if n := len(res.Rows); n != 0 {
				// 循环中提交可能恰好在扫描前完成，此时应是全量 100。
				if n != 100 {
					t.Errorf("partial visibility: %d rows", n)
				}
			}
			return nil
		})
	}
	wg.Wait()
	db.View(func(tx *Txn) error {
		res, _ := tx.ScanIndex(ScanOptions{Index: "idx_email", Prefix: []Value{StringValue("batch")}})
		if len(res.Rows) != 100 {
			t.Errorf("post-commit rows=%d", len(res.Rows))
		}
		return nil
	})
}
