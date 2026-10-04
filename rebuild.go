package sindex

// DriftKind enumerates detected inconsistencies.
type DriftKind string

const (
	// DriftExtraEntry: index entry whose record is missing or whose payload
	// disagrees with the table.
	DriftExtraEntry DriftKind = "extra_or_corrupt_entry"
	// DriftMissingEntry: record not represented in the index.
	DriftMissingEntry DriftKind = "missing_entry"
	// DriftDuplicateUnique: two entries for one value in a unique index.
	DriftDuplicateUnique DriftKind = "duplicate_unique"
)

// Drift describes one index/table inconsistency.
type Drift struct {
	Index  string
	Kind   DriftKind
	Detail string
	Key    []byte // primary key when applicable
}

// RebuildIndex replaces the named index with one built from the full table.
// The replacement happens atomically under the write lock: concurrent readers
// see either the old or the new index, never a partially built one.
func (db *Engine) RebuildIndex(name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	ix, ok := db.indexes[name]
	if !ok {
		return ErrIndexNotFound
	}
	fresh := newIndex(ix.def)
	db.buildIndexLocked(fresh)
	ix.data = fresh.data
	db.seq++
	return nil
}

// RebuildAllIndexes rebuilds every index atomically as one state transition.
func (db *Engine) RebuildAllIndexes() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, ix := range db.indexes {
		fresh := newIndex(ix.def)
		db.buildIndexLocked(fresh)
		ix.data = fresh.data
	}
	db.seq++
	return nil
}

// VerifyIndex cross-checks the named index against the primary table and
// returns every detected drift. An empty slice means perfect consistency.
//
// Checks performed:
//  1. Every table record yields exactly one correctly encoded entry
//     (right indexed values, right covered columns).
//  2. Every index entry points at an existing record and agrees with it.
//  3. Unique indexes never contain two entries for the same indexed value.
func (db *Engine) VerifyIndex(name string) ([]Drift, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	ix, ok := db.indexes[name]
	if !ok {
		return nil, ErrIndexNotFound
	}
	return db.verifyLocked(ix), nil
}

// VerifyAll verifies every index and returns the combined drift list.
func (db *Engine) VerifyAll() ([]Drift, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []Drift
	for _, ix := range db.indexes {
		out = append(out, db.verifyLocked(ix)...)
	}
	return out, nil
}

func (db *Engine) verifyLocked(ix *Index) []Drift {
	var drifts []Drift
	add := func(k DriftKind, detail string, pk []byte) {
		drifts = append(drifts, Drift{
			Index: ix.def.Name, Kind: k, Detail: detail,
			Key: append([]byte(nil), pk...),
		})
	}

	// expected physical key for every record -> present?
	expected := make(map[string]bool)
	db.tbl.scanAscending(nil, nil, func(r Record) bool {
		want := ix.buildEntry(r)
		expected[string(want.SortKey)] = true
		// Find the entry by pk region through the value run is unnecessary:
		// check exact physical key presence.
		if !ix.data.hasSortKey(want.SortKey) {
			// Distinguish missing vs corrupt: look for any entry of this pk.
			if ix.data.hasRecordKey(r.key) {
				add(DriftExtraEntry, "index entry payload disagrees with table", r.key)
			} else {
				add(DriftMissingEntry, "record has no index entry", r.key)
			}
		}
		return true
	})

	// Every physical entry must map to an existing, matching record.
	seen := make(map[string]bool)
	for _, e := range ix.data.entries {
		if seen[string(e.SortKey)] {
			continue
		}
		seen[string(e.SortKey)] = true
		r, ok := db.tbl.get(e.RecordKey)
		if !ok {
			add(DriftExtraEntry, "entry points at missing record", e.RecordKey)
			continue
		}
		want := ix.buildEntry(r)
		if compareBytes(want.SortKey, e.SortKey) != 0 || !coveredEqual(want.Covered, e.Covered) {
			add(DriftExtraEntry, "entry does not match record", e.RecordKey)
		}
		if !expected[string(e.SortKey)] {
			add(DriftExtraEntry, "unexpected entry for record", e.RecordKey)
		}
	}

	// Unique integrity.
	if ix.Unique() {
		var prevCols string
		have := false
		for _, e := range ix.data.entries {
			cols := string(e.SortKey[:len(e.SortKey)-len(e.RecordKey)-1])
			if have && cols == prevCols {
				add(DriftDuplicateUnique, "duplicate value in unique index", e.RecordKey)
			}
			prevCols, have = cols, true
		}
	}
	return drifts
}

func (ix *orderedIndex) hasSortKey(sortKey []byte) bool {
	for _, e := range ix.entries {
		if compareBytes(e.SortKey, sortKey) == 0 {
			return true
		}
	}
	return false
}

func (ix *orderedIndex) hasRecordKey(pk []byte) bool {
	for _, e := range ix.entries {
		if compareBytes(e.RecordKey, pk) == 0 {
			return true
		}
	}
	return false
}

func coveredEqual(a, b map[string]Value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || Compare(va, vb) != 0 {
			return false
		}
	}
	return true
}
