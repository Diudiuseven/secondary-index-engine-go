package sindex

import (
	"errors"
	"testing"
)

func setupUniqueDB(t *testing.T) *Engine {
	t.Helper()
	db := New()
	must(t, db.CreateIndex(IndexDef{Name: "email", Fields: []string{"email"}, Unique: true}))
	must(t, db.CreateIndex(IndexDef{Name: "city_age", Fields: []string{"city", "age"}}))
	must(t, db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Insert(NewRecord([]byte("u1"), map[string]Value{
			"email": StringValue("a@x"), "city": StringValue("BJ"), "age": IntValue(30),
		})); err != nil {
			return err
		}
		return tx.Insert(NewRecord([]byte("u2"), map[string]Value{
			"email": StringValue("b@x"), "city": StringValue("SH"), "age": IntValue(40),
		}))
	}))
	return db
}

func TestUniqueConflictCommitted(t *testing.T) {
	db := setupUniqueDB(t)
	err := db.UpdateTxn(func(tx *Txn) error {
		return tx.Insert(NewRecord([]byte("u3"), map[string]Value{"email": StringValue("a@x")}))
	})
	var ce *UniqueConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want UniqueConflictError, got %v", err)
	}
	if ce.Index != "email" || string(ce.ExistingKey) != "u1" {
		t.Fatalf("conflict detail wrong: %+v", ce)
	}
	if db.Len() != 2 {
		t.Fatal("rejected txn changed table length")
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("rejected txn left index traces: %+v", d)
	}
}

func TestUniqueConflictWithinTxn(t *testing.T) {
	db := setupUniqueDB(t)
	err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Insert(NewRecord([]byte("u3"), map[string]Value{"email": StringValue("c@x")})); err != nil {
			return err
		}
		return tx.Insert(NewRecord([]byte("u4"), map[string]Value{"email": StringValue("c@x")}))
	})
	if !errors.Is(err, ErrUniqueConflict) {
		t.Fatalf("want unique conflict, got %v", err)
	}
	if db.Len() != 2 {
		t.Fatal("intra-txn conflict left partial writes")
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("drift after intra-txn conflict: %+v", d)
	}
}

func TestUniqueMoveFreesOldValue(t *testing.T) {
	db := setupUniqueDB(t)
	// Move u1 away from a@x, then claim a@x with a new row: must succeed.
	err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Put(NewRecord([]byte("u1"), map[string]Value{
			"email": StringValue("z@x"), "city": StringValue("BJ"), "age": IntValue(30),
		})); err != nil {
			return err
		}
		return tx.Insert(NewRecord([]byte("u3"), map[string]Value{
			"email": StringValue("a@x"), "city": StringValue("GZ"), "age": IntValue(10),
		}))
	})
	if err != nil {
		t.Fatalf("move+insert should succeed: %v", err)
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("drift: %+v", d)
	}
}

func TestUniqueSwapValues(t *testing.T) {
	db := setupUniqueDB(t)
	// Swap emails between u1 and u2 in one txn: allowed.
	err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Put(NewRecord([]byte("u1"), map[string]Value{
			"email": StringValue("b@x"), "city": StringValue("BJ"), "age": IntValue(30),
		})); err != nil {
			return err
		}
		return tx.Put(NewRecord([]byte("u2"), map[string]Value{
			"email": StringValue("a@x"), "city": StringValue("SH"), "age": IntValue(40),
		}))
	})
	if err != nil {
		t.Fatalf("swap should succeed: %v", err)
	}
}

func TestRollbackLeavesNoTrace(t *testing.T) {
	db := setupUniqueDB(t)
	tx := db.Begin()
	must(t, tx.Insert(NewRecord([]byte("u9"), map[string]Value{
		"email": StringValue("new@x"), "city": StringValue("X"), "age": IntValue(1),
	})))
	must(t, tx.Delete([]byte("u1")))
	tx.Rollback()
	if db.Len() != 2 {
		t.Fatal("rollback changed table length")
	}
	if d, _ := db.VerifyAll(); len(d) != 0 {
		t.Fatalf("rollback left index traces: %+v", d)
	}
	// Closed txn must reject further use.
	if err := tx.Insert(rec("q", "Y", 1, "")); err != ErrTxnClosed {
		t.Fatalf("want ErrTxnClosed, got %v", err)
	}
}

func TestCommitFailureLeavesPriorState(t *testing.T) {
	db := setupUniqueDB(t)
	err := db.UpdateTxn(func(tx *Txn) error {
		if err := tx.Insert(NewRecord([]byte("u9"), map[string]Value{
			"email": StringValue("ok@x"), "city": StringValue("X"), "age": IntValue(1),
		})); err != nil {
			return err
		}
		// This conflicts; first insert must not become visible.
		return tx.Insert(NewRecord([]byte("u10"), map[string]Value{"email": StringValue("ok@x")}))
	})
	if !errors.Is(err, ErrUniqueConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, ok := db.Get([]byte("u9")); ok {
		t.Fatal("failed commit exposed earlier buffered insert")
	}
}
