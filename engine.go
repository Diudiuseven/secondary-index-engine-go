package sindex

import (
	"sort"
	"sync"
)

// Engine is the secondary-index engine: one primary table plus any number of
// secondary indexes.
//
// Concurrency model: a single sync.RWMutex. Readers (Get, Scan) take RLock;
// writers (CreateIndex, transaction commit) take Lock. All table and index
// changes of one commit happen inside one Lock critical section, so they
// become visible to other goroutines as one indivisible state transition and
// readers can never observe a half-applied commit.
type Engine struct {
	mu      sync.RWMutex
	tbl     *table
	indexes map[string]*Index
	seq     uint64 // bumped on every committed change; snapshot marker
}

// New creates an empty engine.
func New() *Engine {
	return &Engine{tbl: newTable(), indexes: make(map[string]*Index)}
}

// Begin starts a read-write transaction.
func (db *Engine) Begin() *Txn { return db.newTxn(false) }

// BeginReadOnly starts a transaction that rejects mutating operations.
func (db *Engine) BeginReadOnly() *Txn { return db.newTxn(true) }

// CreateIndex registers a new index and builds it from existing records.
func (db *Engine) CreateIndex(def IndexDef) error {
	if err := validateDef(def); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.indexes[def.Name]; ok {
		return ErrIndexExists
	}
	ix := newIndex(def)
	db.indexes[def.Name] = ix
	db.buildIndexLocked(ix)
	return nil
}

// DropIndex removes an index; the primary table is unaffected.
func (db *Engine) DropIndex(name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.indexes[name]; !ok {
		return ErrIndexNotFound
	}
	delete(db.indexes, name)
	return nil
}

// Index returns the named index.
func (db *Engine) Index(name string) (*Index, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	ix, ok := db.indexes[name]
	return ix, ok
}

// IndexDefs returns all index definitions sorted by name.
func (db *Engine) IndexDefs() []IndexDef {
	db.mu.RLock()
	defer db.mu.RUnlock()
	names := make([]string, 0, len(db.indexes))
	for n := range db.indexes {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]IndexDef, 0, len(names))
	for _, n := range names {
		out = append(out, db.indexes[n].Def())
	}
	return out
}

// Get returns the committed record under key.
func (db *Engine) Get(key []byte) (Record, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.tbl.get(key)
}

// Len returns the number of committed records.
func (db *Engine) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.tbl.len()
}

// Version returns a monotonically increasing counter bumped on every
// committed write (insert/update/delete/index rebuild). It lets callers tell
// whether the engine changed between two observations.
func (db *Engine) Version() uint64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.seq
}

// ScanTable iterates the primary table by ascending primary key over the
// half-open range [start, end). nil bounds mean unbounded. It observes one
// committed snapshot. fn returning false stops iteration early.
func (db *Engine) ScanTable(start, end []byte, fn func(Record) bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	db.tbl.scanAscending(start, end, fn)
}

// ReadTxn runs fn in a read-only transaction.
func (db *Engine) ReadTxn(fn func(tx *Txn) error) error {
	tx := db.BeginReadOnly()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateTxn runs fn in a read-write transaction and commits if fn succeeds;
// otherwise it rolls back and returns fn's error.
func (db *Engine) UpdateTxn(fn func(tx *Txn) error) error {
	tx := db.Begin()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func validateDef(def IndexDef) error {
	if def.Name == "" || len(def.Fields) == 0 {
		return ErrInvalidArgument
	}
	seen := make(map[string]bool, len(def.Fields))
	for _, f := range def.Fields {
		if f == "" || seen[f] {
			return ErrInvalidArgument
		}
		seen[f] = true
	}
	for _, f := range def.Covered {
		if f == "" {
			return ErrInvalidArgument
		}
	}
	return nil
}

func newIndex(def IndexDef) *Index {
	k := IndexNonUnique
	if def.Unique {
		k = IndexUnique
	}
	return &Index{def: def, kind: k, data: newOrderedIndex()}
}

// resolvedRow is the post-buffer outcome for one touched primary key.
type resolvedRow struct {
	op      bufferedOp
	existed bool
	old     Record // committed record, valid iff existed
	final   Record // post-txn record, valid iff hasNew
	hasNew  bool   // key exists after txn
}

// oneIndexChange is a prepared index mutation for a single (index, key).
type oneIndexChange struct {
	ix       *Index
	oldEntry Entry
	hadOld   bool
	newEntry Entry
	hasNew   bool
}

// commit validates and applies one buffered transaction atomically.
func (db *Engine) commit(tx *Txn) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	keys := make([]string, 0, len(tx.final))
	for k := range tx.final {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 1. Replay history to validate insert-if-absent, then derive the final
	//    row state relative to committed data.
	rows := make(map[string]resolvedRow, len(keys))
	for _, k := range keys {
		committed, existed := db.tbl.get([]byte(k))
		present := existed
		var cur Record
		for _, op := range tx.history[k] {
			switch op.kind {
			case opInsert:
				if present {
					return ErrExists
				}
				cur, present = op.rec, true
			case opPut:
				cur, present = op.rec, true
			case opDelete:
				cur, present = Record{}, false
			}
		}
		rows[k] = resolvedRow{
			op:      tx.final[k],
			existed: existed,
			old:     committed,
			final:   cur,
			hasNew:  present,
		}
	}

	// 2. Build the post-transaction view per index and enforce uniqueness on
	//    exactly that view, which makes value moves and swaps legal while
	//    still rejecting real duplicates.
	idxOrder := make([]*Index, 0, len(db.indexes))
	for _, ix := range db.indexes {
		idxOrder = append(idxOrder, ix)
	}
	sort.Slice(idxOrder, func(a, b int) bool { return idxOrder[a].def.Name < idxOrder[b].def.Name })

	allChanges := make([]oneIndexChange, 0)
	for _, ix := range idxOrder {
		// Post-txn entries contributed by touched rows.
		type postRow struct {
			entry Entry
			cols  string
		}
		bySortKey := make(map[string]postRow)
		byPK := make(map[string]bool)
		// removedPKs are committed rows whose committed entry is deleted or
		// moved away by this transaction; values they used to hold are freed.
		removedPKs := make(map[string]bool)
		for _, k := range keys {
			row := rows[k]
			ch := oneIndexChange{ix: ix}
			if row.existed {
				ch.oldEntry = ix.buildEntry(row.old)
				ch.hadOld = true
			}
			if row.hasNew {
				e := ix.buildEntry(row.final)
				ch.newEntry = e
				ch.hasNew = true
				cols := string(e.SortKey[:len(e.SortKey)-len(e.RecordKey)-1])
				bySortKey[string(e.SortKey)] = postRow{entry: e, cols: cols}
				byPK[string(e.RecordKey)] = true
			}
			// A committed row vacates its old value if it is deleted, or if
			// its new entry on this index differs from the old one.
			if row.existed {
				oldE := ix.buildEntry(row.old)
				if !row.hasNew {
					removedPKs[string(oldE.RecordKey)] = true
				} else {
					newE := ix.buildEntry(row.final)
					if compareBytes(oldE.SortKey, newE.SortKey) != 0 {
						removedPKs[string(oldE.RecordKey)] = true
					}
				}
			}
			if ch.hadOld && ch.hasNew && compareBytes(ch.oldEntry.SortKey, ch.newEntry.SortKey) == 0 {
				ch.hadOld, ch.hasNew = false, false
			}
			allChanges = append(allChanges, ch)
		}

		if ix.Unique() {
			// (a) touched rows must not collide among themselves in post-view.
			owner := make(map[string]string) // cols -> pk
			for sk, pr := range bySortKey {
				if pk, ok := owner[pr.cols]; ok && pk != string(pr.entry.RecordKey) {
					return &UniqueConflictError{
						Index:       ix.def.Name,
						Values:      append([]Value(nil), pr.entry.Values...),
						ExistingKey: []byte(pk),
					}
				}
				owner[pr.cols] = string(pr.entry.RecordKey)
				_ = sk
			}
			// (b) claimed values must not remain held by an untouched committed row.
			for cols, pk := range owner {
				other, ok := ix.data.existsExact([]byte(cols), []byte(pk))
				if ok && !byPK[string(other.RecordKey)] && !removedPKs[string(other.RecordKey)] {
					var vals []Value
					for _, pr := range bySortKey {
						if pr.cols == cols {
							vals = append([]Value(nil), pr.entry.Values...)
						}
					}
					return &UniqueConflictError{
						Index:       ix.def.Name,
						Values:      vals,
						ExistingKey: append([]byte(nil), other.RecordKey...),
					}
				}
			}
		}
	}

	// 3. All constraints satisfied: mutate the table then every index while
	//    still holding the write lock. No failure path remains, so table and
	//    indexes can never diverge.
	for _, k := range keys {
		row := rows[k]
		switch row.op.kind {
		case opInsert, opPut:
			if row.hasNew {
				db.tbl.put(row.final)
			}
		case opDelete:
			if row.existed {
				db.tbl.delete([]byte(k))
			}
		}
	}
	for i := range allChanges {
		ch := &allChanges[i]
		if ch.hadOld {
			ch.ix.data.remove(ch.oldEntry.SortKey)
		}
		if ch.hasNew {
			ch.ix.data.insert(ch.newEntry)
		}
	}
	db.seq++
	return nil
}

// buildIndexLocked populates ix from the full table (write lock held).
func (db *Engine) buildIndexLocked(ix *Index) {
	db.tbl.scanAscending(nil, nil, func(r Record) bool {
		ix.data.insert(ix.buildEntry(r))
		return true
	})
}
