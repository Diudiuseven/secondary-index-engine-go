package sindex

import "sort"

// table is the primary-key store: a map plus a maintained sorted key list.
//
// The table performs NO locking of its own; all access is serialised by the
// engine lock (RLock for reads, Lock for writes). Callers must hold it.
type table struct {
	records  map[string]Record
	sortedKs [][]byte
}

func newTable() *table {
	return &table{records: make(map[string]Record)}
}

// get returns a defensive copy of the record stored under key.
func (t *table) get(key []byte) (Record, bool) {
	r, ok := t.records[string(key)]
	if !ok {
		return Record{}, false
	}
	return cloneRecord(r), true
}

// put inserts or replaces a record. The table stores its own copy.
func (t *table) put(r Record) {
	sk := string(r.key)
	if _, ok := t.records[sk]; !ok {
		t.insertSortedKey(r.key)
	}
	t.records[sk] = cloneRecord(r)
}

// delete removes key. It reports whether anything was removed.
func (t *table) delete(key []byte) bool {
	sk := string(key)
	if _, ok := t.records[sk]; !ok {
		return false
	}
	delete(t.records, sk)
	t.removeSortedKey(key)
	return true
}

func (t *table) len() int { return len(t.records) }

// scanAscending visits records with key in [start, end) in ascending key
// order. A nil start/end means unbounded. Returning false from fn stops early.
func (t *table) scanAscending(start, end []byte, fn func(Record) bool) {
	lo := 0
	if start != nil {
		lo = sort.Search(len(t.sortedKs), func(i int) bool { return compareBytes(t.sortedKs[i], start) >= 0 })
	}
	for i := lo; i < len(t.sortedKs); i++ {
		if end != nil && compareBytes(t.sortedKs[i], end) >= 0 {
			return
		}
		r, ok := t.records[string(t.sortedKs[i])]
		if !ok {
			continue
		}
		if !fn(cloneRecord(r)) {
			return
		}
	}
}

func (t *table) insertSortedKey(key []byte) {
	kc := make([]byte, len(key))
	copy(kc, key)
	i := sort.Search(len(t.sortedKs), func(i int) bool { return compareBytes(t.sortedKs[i], kc) >= 0 })
	t.sortedKs = append(t.sortedKs, nil)
	copy(t.sortedKs[i+1:], t.sortedKs[i:])
	t.sortedKs[i] = kc
}

func (t *table) removeSortedKey(key []byte) {
	i := sort.Search(len(t.sortedKs), func(i int) bool { return compareBytes(t.sortedKs[i], key) >= 0 })
	if i < len(t.sortedKs) && compareBytes(t.sortedKs[i], key) == 0 {
		t.sortedKs = append(t.sortedKs[:i], t.sortedKs[i+1:]...)
	}
}

func cloneRecord(r Record) Record {
	out := Record{key: make([]byte, len(r.key)), fields: make(map[string]Value, len(r.fields))}
	copy(out.key, r.key)
	for k, v := range r.fields {
		out.fields[k] = cloneValue(v)
	}
	return out
}
