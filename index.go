package sindex

import (
	"sort"
)

// IndexKind selects unique vs non-unique semantics.
type IndexKind uint8

const (
	IndexNonUnique IndexKind = iota
	IndexUnique
)

// IndexDef declares a secondary index.
//
// Fields are the indexed columns, in order: the index is ordered by the
// encoded values of these fields, ascending, in the total value order
// documented in encoding.go.
//
// Covered lists ADDITIONAL columns materialised in every index entry (they
// need not repeat Fields). They let queries be answered from the index alone
// (covering scans) without consulting the primary table.
//
// Unique makes the combination of Fields values unique table-wide; an insert
// or update that would duplicate an existing combination is rejected at
// commit time.
type IndexDef struct {
	Name    string
	Fields  []string
	Covered []string
	Unique  bool
}

// Entry is one physical index item.
type Entry struct {
	// SortKey is encoded(Fields...) + 0x00 + primaryKey, the unique physical
	// ordering key of the entry.
	SortKey []byte
	// Values are the decoded indexed values, in def.Fields order.
	Values []Value
	// RecordKey is the owning record primary key.
	RecordKey []byte
	// Covered holds the materialised covering columns.
	Covered map[string]Value
}

type orderedIndex struct {
	entries []Entry // sorted by SortKey, duplicates impossible
}

func newOrderedIndex() *orderedIndex { return &orderedIndex{} }

// entrySortKey builds the physical key: fully encoded index columns, a single
// 0x00 separator, then the raw primary key bytes. Primary keys are assumed not
// to contain... they may contain any bytes: since every SortKey ends with the
// same encoded-prefix length for a fixed index, the trailing PK region is
// compared verbatim and gives a deterministic tie-break.
func entrySortKey(indexCols []byte, recordKey []byte) []byte {
	out := make([]byte, 0, len(indexCols)+1+len(recordKey))
	out = append(out, indexCols...)
	out = append(out, 0x00)
	out = append(out, recordKey...)
	return out
}

// lookupSortKey returns the (possibly empty) run of entries whose SortKey has
// indexPrefix as a strict byte prefix, i.e. entries for one index value in a
// non-unique index (for unique indexes the run has length <= 1).
func (ix *orderedIndex) lookupPrefix(indexPrefix []byte) []Entry {
	lo := sort.Search(len(ix.entries), func(i int) bool {
		return compareBytes(ix.entries[i].SortKey, indexPrefix) >= 0
	})
	var hi int
	succ, ok := prefixUpperBound(indexPrefix)
	if !ok {
		hi = len(ix.entries)
	} else {
		hi = sort.Search(len(ix.entries), func(i int) bool {
			return compareBytes(ix.entries[i].SortKey, succ) >= 0
		})
	}
	return ix.entries[lo:hi]
}

// existsExact reports whether any entry carries exactly the encoded index
// value prefix, excluding the record identified by exceptPK (used when moving
// a record from one index value to another).
func (ix *orderedIndex) existsExact(encodedCols []byte, exceptPK []byte) (Entry, bool) {
	run := ix.lookupPrefix(encodedCols)
	for _, e := range run {
		if compareBytes(e.RecordKey, exceptPK) != 0 {
			return e, true
		}
	}
	return Entry{}, false
}

func (ix *orderedIndex) insert(e Entry) {
	i := sort.Search(len(ix.entries), func(i int) bool {
		return compareBytes(ix.entries[i].SortKey, e.SortKey) >= 0
	})
	if i < len(ix.entries) && compareBytes(ix.entries[i].SortKey, e.SortKey) == 0 {
		// Same record, same key: replace (defensive; normally delete+insert).
		ix.entries[i] = e
		return
	}
	ix.entries = append(ix.entries, Entry{})
	copy(ix.entries[i+1:], ix.entries[i:])
	ix.entries[i] = e
}

func (ix *orderedIndex) remove(sortKey []byte) bool {
	i := sort.Search(len(ix.entries), func(i int) bool {
		return compareBytes(ix.entries[i].SortKey, sortKey) >= 0
	})
	if i < len(ix.entries) && compareBytes(ix.entries[i].SortKey, sortKey) == 0 {
		ix.entries = append(ix.entries[:i], ix.entries[i+1:]...)
		return true
	}
	return false
}

// walkRange visits every entry with SortKey in [lo, hi) ascending. A nil lo /
// hi means unbounded. fn returning false stops iteration. The walk operates on
// a snapshot of the slice header and hands out copies, so concurrent mutation
// is safe as long as callers serialise via the engine lock.
func (ix *orderedIndex) walkRange(lo, hi []byte, fn func(Entry) bool) {
	start := 0
	if lo != nil {
		start = sort.Search(len(ix.entries), func(i int) bool {
			return compareBytes(ix.entries[i].SortKey, lo) >= 0
		})
	}
	for i := start; i < len(ix.entries); i++ {
		if hi != nil && compareBytes(ix.entries[i].SortKey, hi) >= 0 {
			return
		}
		if !fn(cloneEntry(ix.entries[i])) {
			return
		}
	}
}

func (ix *orderedIndex) len() int { return len(ix.entries) }

func cloneEntry(e Entry) Entry {
	out := Entry{
		SortKey:   append([]byte(nil), e.SortKey...),
		RecordKey: append([]byte(nil), e.RecordKey...),
	}
	out.Values = make([]Value, len(e.Values))
	copy(out.Values, e.Values)
	if e.Covered != nil {
		out.Covered = make(map[string]Value, len(e.Covered))
		for k, v := range e.Covered {
			out.Covered[k] = cloneValue(v)
		}
	}
	return out
}

// Index --------------------------------------------------------------------

// Index is one secondary index: definition + physical storage.
type Index struct {
	def  IndexDef
	kind IndexKind
	data *orderedIndex
}

// Def returns the index definition (copy of the name/field slices).
func (ix *Index) Def() IndexDef {
	d := ix.def
	d.Fields = append([]string(nil), ix.def.Fields...)
	d.Covered = append([]string(nil), ix.def.Covered...)
	return d
}

// Len returns the number of physical entries.
func (ix *Index) Len() int { return ix.data.len() }

// Unique reports whether the index enforces uniqueness.
func (ix *Index) Unique() bool { return ix.kind == IndexUnique }

// buildEntry constructs the physical entry for r under this index.
func (ix *Index) buildEntry(r Record) Entry {
	vals := make([]Value, len(ix.def.Fields))
	cols := make([]byte, 0)
	for i, f := range ix.def.Fields {
		vals[i] = r.Field(f) // missing -> Nil
		cols = encodeValue(cols, vals[i])
	}
	var covered map[string]Value
	if len(ix.def.Covered) > 0 {
		covered = make(map[string]Value, len(ix.def.Covered))
		for _, f := range ix.def.Covered {
			covered[f] = cloneValue(r.Field(f))
		}
	}
	return Entry{
		SortKey:   entrySortKey(cols, r.key),
		Values:    vals,
		RecordKey: append([]byte(nil), r.key...),
		Covered:   covered,
	}
}
