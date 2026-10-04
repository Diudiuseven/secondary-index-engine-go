package sindex

// opKind is the kind of buffered table mutation.
type opKind uint8

const (
	opInsert opKind = iota // key must not exist at that point
	opPut                  // upsert
	opDelete               // remove if present
)

type bufferedOp struct {
	kind opKind
	rec  Record // present for insert/put
}

// Txn is a read-write transaction.
//
// Isolation: reads observe committed data shadowed by the transaction's own
// writes (read-your-writes). Buffered writes are private until Commit. Commit
// validates every constraint (unique indexes, insert-if-absent) against the
// post-replay state and, under a single engine write lock, applies table and
// index changes atomically: other goroutines see all of them or none.
// Rollback simply drops the private buffer without touching committed state.
type Txn struct {
	db       *Engine
	closed   bool
	readOnly bool
	seq      uint64

	// history is the per-key ordered list of buffered operations; commit
	// replays it to evaluate insert-if-absent semantics precisely.
	history map[string][]bufferedOp
	// final is the current buffered state of each key.
	final map[string]bufferedOp
}

func (db *Engine) newTxn(readOnly bool) *Txn {
	db.mu.RLock()
	seq := db.seq
	db.mu.RUnlock()
	return &Txn{
		db:       db,
		readOnly: readOnly,
		seq:      seq,
		history:  make(map[string][]bufferedOp),
		final:    make(map[string]bufferedOp),
	}
}

func (tx *Txn) checkWritable() error {
	if tx.closed {
		return ErrTxnClosed
	}
	if tx.readOnly {
		return ErrInvalidArgument
	}
	return nil
}

// Get reads committed data unless shadowed by a buffered write.
func (tx *Txn) Get(key []byte) (Record, error) {
	if tx.closed {
		return Record{}, ErrTxnClosed
	}
	if op, ok := tx.final[string(key)]; ok {
		if op.kind == opDelete {
			return Record{}, ErrNotFound
		}
		return cloneRecord(op.rec), nil
	}
	r, ok := tx.db.tbl.get(key)
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

// Insert buffers an insert that fails if the key exists at that moment.
func (tx *Txn) Insert(r Record) error {
	if err := tx.checkWritable(); err != nil {
		return err
	}
	k := string(r.key)
	op := bufferedOp{kind: opInsert, rec: cloneRecord(r)}
	tx.history[k] = append(tx.history[k], op)
	tx.final[k] = op
	return nil
}

// Put buffers an upsert.
func (tx *Txn) Put(r Record) error {
	if err := tx.checkWritable(); err != nil {
		return err
	}
	k := string(r.key)
	op := bufferedOp{kind: opPut, rec: cloneRecord(r)}
	tx.history[k] = append(tx.history[k], op)
	tx.final[k] = op
	return nil
}

// Delete buffers removal of key.
func (tx *Txn) Delete(key []byte) error {
	if err := tx.checkWritable(); err != nil {
		return err
	}
	kc := make([]byte, len(key))
	copy(kc, key)
	k := string(kc)
	op := bufferedOp{kind: opDelete}
	tx.history[k] = append(tx.history[k], op)
	tx.final[k] = op
	return nil
}

// Commit applies buffered writes atomically; see type docs.
func (tx *Txn) Commit() error {
	if tx.closed {
		return ErrTxnClosed
	}
	tx.closed = true
	if tx.readOnly || len(tx.final) == 0 {
		return nil
	}
	return tx.db.commit(tx)
}

// Rollback discards all buffered writes.
func (tx *Txn) Rollback() {
	if tx.closed {
		return
	}
	tx.closed = true
	tx.history = nil
	tx.final = nil
}
