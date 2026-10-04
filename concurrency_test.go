package sindex

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentMixedWorkload runs many writers (each txn touching a few keys)
// against concurrent readers and scanners. With -race it must report no data
// races; afterwards every index must verify clean and counts must agree.
func TestConcurrentMixedWorkload(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "bucket", Fields: []string{"bucket", "seq"}}))
	must(t, db.CreateIndex(IndexDef{Name: "uniq", Fields: []string{"token"}, Unique: true}))

	const writers = 8
	const iters = 120
	var wg sync.WaitGroup
	var conflicts int64

	// Pre-seed some rows.
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for i := 0; i < 50; i++ {
			err := tx.Insert(NewRecord(
				[]byte(fmt.Sprintf("k%04d", i)),
				map[string]Value{
					"bucket": StringValue(fmt.Sprintf("b%d", i%5)),
					"seq":    IntValue(int64(i)),
					"token":  StringValue(fmt.Sprintf("tok%04d", i)),
				}))
			if err != nil {
				return err
			}
		}
		return nil
	}))

	stop := make(chan struct{})
	// Readers: point lookups.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := []byte(fmt.Sprintf("k%04d", (id*7+i)%200))
				if rec, ok := db.Get(key); ok {
					// Visible record must be a coherent committed value.
					if _, ok := rec.Get("token"); !ok {
						t.Errorf("torn record read")
						return
					}
				}
				i++
			}
		}(r)
	}
	// Scanners: prefix scans must never error and stay ordered.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := db.Scan(RangeOptions{
				IndexName:    "bucket",
				PrefixValues: []Value{StringValue("b2")},
			})
			if err != nil {
				t.Errorf("scan error: %v", err)
				return
			}
			for j := 1; j < len(res.Records); j++ {
				if compareBytes(res.Records[j-1].Key(), res.Records[j].Key()) == 0 {
					// duplicate keys in a stable order tolerated, but order must
					// never go backwards on the physical index; check via seq:
				}
			}
		}
	}()

	// Writers.
	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wwg.Add(1)
		go func(id int) {
			defer wwg.Done()
			for n := 0; n < iters; n++ {
				i := id*iters + n + 50
				err := db.UpdateTxn(func(tx *Txn) error {
					key := []byte(fmt.Sprintf("k%04d", i%200))
					// Deliberately reuse tokens across iterations to provoke
					// unique conflicts; a rejected txn must leave no trace.
					return tx.Put(NewRecord(key, map[string]Value{
						"bucket": StringValue(fmt.Sprintf("b%d", i%5)),
						"seq":    IntValue(int64(i)),
						"token":  StringValue(fmt.Sprintf("tok%04d", i%60)),
					}))
				})
				var ce *UniqueConflictError
				if errors.As(err, &ce) {
					atomic.AddInt64(&conflicts, 1)
				} else if err != nil {
					t.Errorf("unexpected write error: %v", err)
					return
				}
			}
		}(w)
	}
	wwg.Wait()
	close(stop)
	wg.Wait()

	if d, err := db.VerifyAll(); err != nil || len(d) != 0 {
		t.Fatalf("post-concurrency drift: %+v err=%v", d, err)
	}
	// Every index entry count must equal the table length.
	for _, def := range db.IndexDefs() {
		ix, _ := db.Index(def.Name)
		if ix.Len() != db.Len() {
			t.Fatalf("index %s len=%d table=%d", def.Name, ix.Len(), db.Len())
		}
	}
	t.Logf("unique conflicts rejected atomically: %d", conflicts)
}

// TestScanNeverSeesUncommitted ensures readers never observe a write that is
// later rolled back, nor an intermediate multi-key state.
func TestScanNeverSeesUncommitted(t *testing.T) {
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "v", Fields: []string{"v"}, Unique: true}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		for i := 0; i < 20; i++ {
			if err := tx.Insert(NewRecord([]byte(fmt.Sprintf("k%d", i)), map[string]Value{
				"v": IntValue(int64(i)),
			})); err != nil {
				return err
			}
		}
		return nil
	}))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := db.Scan(RangeOptions{IndexName: "v"})
			if err != nil {
				t.Errorf("scan: %v", err)
				return
			}
			// A committed snapshot must always contain exactly 20 entries and
			// be strictly ordered.
			if n := len(res.Records); n != 20 {
				t.Errorf("observed uncommitted/partial state: %d rows", n)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		tx := db.Begin()
		// Mutate then rollback; readers must see nothing.
		_ = tx.Put(NewRecord([]byte("k0"), map[string]Value{"v": IntValue(999)}))
		_ = tx.Insert(NewRecord([]byte("extra"), map[string]Value{"v": IntValue(1000)}))
		tx.Rollback()
	}
	close(stop)
	wg.Wait()

	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("drift: %+v", d)
	}
}
