package sindex

import "errors"

// Bound selects inclusive/exclusive semantics for one range endpoint.
type Bound uint8

const (
	// Inclusive includes rows whose indexed columns equal the boundary value.
	Inclusive Bound = iota
	// Exclusive excludes rows whose indexed columns equal the boundary value.
	Exclusive
)

// RangeOptions describes a secondary-index scan.
//
// Two modes, selected by which fields are populated:
//
// Range mode (prefix of indexed columns):
//   - StartValues / EndValues each encode a PREFIX of the index columns
//     (1..len(Fields) values). Comparison is SQL-style row-value comparison:
//     the boundary tuple is compared against the leading columns of every
//     entry. StartBound/EndBound decide inclusion of exact tuple matches.
//   - A nil/empty Start means "from the beginning"; nil/empty End means
//     "to the end".
//
// Prefix mode:
//   - PrefixValues encodes a prefix of the index columns; the scan returns
//     exactly entries whose leading columns equal it. It is equivalent to an
//     inclusive range with the successor of the encoded prefix as the
//     exclusive end. Prefix mode must not be combined with Start/End.
//
// Fields lists the record fields the caller needs. When every requested field
// is available from the index (an indexed column or a Covered column), every
// result is served from the index alone (Covered=true, TableLookups=0).
// Fields may include the primary key, which is always available.
//
// Limit, when > 0, caps the number of returned records.
type RangeOptions struct {
	IndexName    string
	StartValues  []Value
	StartBound   Bound
	EndValues    []Value
	EndBound     Bound
	PrefixValues []Value
	Fields       []string
	Limit        int
}

// ScanResult reports scan output plus observability counters.
type ScanResult struct {
	Records []Record
	// Covered is true iff ALL returned records were assembled from index
	// entries without a primary-table lookup.
	Covered bool
	// IndexItems is the number of physical index entries visited.
	IndexItems int
	// TableLookups is the number of primary-table fetches performed (回表).
	TableLookups int
	// Limited is true when the scan stopped because Limit was reached.
	Limited bool
}

// Scan runs a secondary-index range or prefix scan.
func (db *Engine) Scan(opts RangeOptions) (ScanResult, error) {
	if opts.PrefixValues != nil && (opts.StartValues != nil || opts.EndValues != nil) {
		return ScanResult{}, errors.New("sindex: prefix and range bounds are mutually exclusive")
	}
	// Hold the read lock for the whole scan so it observes one immutable,
	// committed snapshot: no committed write can mutate the slice underneath
	// us. Multiple scans share the lock and proceed concurrently.
	db.mu.RLock()
	defer db.mu.RUnlock()
	ix, ok := db.indexes[opts.IndexName]
	if !ok {
		return ScanResult{}, ErrIndexNotFound
	}
	if err := validateScanValues(ix, opts); err != nil {
		return ScanResult{}, err
	}

	var lo, hi []byte
	hasLo, hasHi := false, false
	switch {
	case opts.PrefixValues != nil:
		lo, hi, hasLo, hasHi = buildPrefixBounds(ix, opts.PrefixValues)
	default:
		var err error
		lo, hi, hasLo, hasHi, err = buildRangeBounds(ix, opts)
		if err != nil {
			return ScanResult{}, err
		}
	}
	if hasHi && hasLo && compareBytes(lo, hi) >= 0 {
		return ScanResult{Covered: true}, nil // empty range
	}

	// Determine covering capability.
	need := requestedFields(opts.Fields)
	covered := true
	for f := range need {
		if f == "" {
			continue
		}
		if !ix.hasColumn(f) {
			covered = false
			break
		}
	}

	res := ScanResult{Covered: true}
	var walkLo, walkHi []byte
	if hasLo {
		walkLo = lo
	}
	if hasHi {
		walkHi = hi
	}

	ix.data.walkRange(walkLo, walkHi, func(e Entry) bool {
		res.IndexItems++
		var rec Record
		if covered {
			rec = assembleCovered(ix, e, need)
		} else {
			r, found := db.tbl.get(e.RecordKey)
			res.TableLookups++
			if !found {
				// Index points at a vanished record; should never happen given
				// atomic maintenance, but skip defensively.
				return true
			}
			rec = projectRecord(r, need)
		}
		res.Records = append(res.Records, rec)
		if opts.Limit > 0 && len(res.Records) >= opts.Limit {
			res.Limited = true
			return false
		}
		return true
	})
	if !covered {
		res.Covered = false
	}
	return res, nil
}

func validateScanValues(ix *Index, opts RangeOptions) error {
	check := func(vals []Value) error {
		if len(vals) > len(ix.def.Fields) {
			return ErrInvalidArgument
		}
		for _, v := range vals {
			if !v.IsValid() || v.IsNaN() {
				return ErrInvalidArgument
			}
		}
		return nil
	}
	if err := check(opts.StartValues); err != nil {
		return err
	}
	if err := check(opts.EndValues); err != nil {
		return err
	}
	if err := check(opts.PrefixValues); err != nil {
		return err
	}
	return nil
}

// buildPrefixBounds returns [prefix, successor(prefix)) over physical keys.
func buildPrefixBounds(ix *Index, vals []Value) (lo, hi []byte, hasLo, hasHi bool) {
	// Always non-terminated: for a full-width prefix this still matches the
	// physical key region (0x00 + pk) that follows the final column.
	prefix := encodePartial(nil, vals, false)
	lo = prefix
	if up, ok := prefixUpperBound(prefix); ok {
		return lo, up, true, true
	}
	return lo, nil, true, false
}

// buildRangeBounds converts SQL-style tuple bounds into physical byte bounds
// against entries of the form encode(allFields)+0x00+pk.
//
// Lower bound (both full-width and partial tuples):
//
//	Inclusive: raw non-terminated encoding of the tuple.
//	Exclusive: successor of that encoding.
//
// Upper bound, expressed in the half-open interval [lo, hi), using SQL-style
// row-value comparison against the leading index columns:
//
//	Partial tuple (fewer values than index columns): the boundary stands for
//	ALL rows whose leading columns equal it, i.e. a closed sub-range. The
//	corresponding byte upper bound is the successor of the TERMINATED partial
//	prefix (last column emitted with terminator); Inclusive/Exclusive is
//	irrelevant for a partial tuple in row-value semantics.
//
//	Full-width tuple:
//	  Inclusive: successor of the terminated encoding; 0x00 -> 0x01 lands just
//	             above the PK separator region and below the next column value.
//	  Exclusive: non-terminated prefix followed by 0x00; every matching key has
//	             a non-empty PK after that separator, so all of them sort above
//	             this bound while equal-column keys are excluded.
func buildRangeBounds(ix *Index, opts RangeOptions) (lo, hi []byte, hasLo, hasHi bool, err error) {
	full := func(vals []Value) bool { return len(vals) == len(ix.def.Fields) }
	if len(opts.StartValues) > 0 {
		startPrefix := encodePartial(nil, opts.StartValues, false)
		if opts.StartBound == Inclusive {
			lo, hasLo = startPrefix, true
		} else if up, ok := prefixUpperBound(startPrefix); ok {
			lo, hasLo = up, true
		}
	}
	if len(opts.EndValues) > 0 {
		switch {
		case !full(opts.EndValues):
			base := encodePartial(nil, opts.EndValues, true)
			if up, ok := prefixUpperBound(base); ok {
				hi, hasHi = up, true
			}
		case opts.EndBound == Inclusive:
			base := encodePartial(nil, opts.EndValues, true)
			if up, ok := prefixUpperBound(base); ok {
				hi, hasHi = up, true
			}
		default: // full-width exclusive
			base := encodePartial(nil, opts.EndValues, false)
			hi = append(base, 0x00)
			hasHi = true
		}
	}
	return lo, hi, hasLo, hasHi, nil
}

func encodePartial(dst []byte, vals []Value, terminated bool) []byte {
	for i, v := range vals {
		last := i == len(vals)-1
		switch {
		case last && !terminated:
			dst = encodeColumnPrefix(dst, v)
		default:
			dst = encodeValue(dst, v)
		}
	}
	return dst
}

// requestedFields normalises the field projection. Empty means "all fields"
// which can never be covered (index carries only declared columns).
func requestedFields(fields []string) map[string]bool {
	if len(fields) == 0 {
		return nil
	}
	m := make(map[string]bool, len(fields))
	for _, f := range fields {
		m[f] = true
	}
	return m
}

// hasColumn reports whether f can be read from the index entry itself.
func (ix *Index) hasColumn(f string) bool {
	for _, c := range ix.def.Fields {
		if c == f {
			return true
		}
	}
	for _, c := range ix.def.Covered {
		if c == f {
			return true
		}
	}
	return false
}

// assembleCovered builds a projected record purely from an index entry.
func assembleCovered(ix *Index, e Entry, need map[string]bool) Record {
	fields := make(map[string]Value)
	if need == nil {
		// "all fields" requested cannot be covered; this path is unused.
		need = map[string]bool{}
	}
	for f := range need {
		switch {
		case f == "":
			continue
		case ix.hasColumn(f):
			fields[f] = valueFromEntry(ix, e, f)
		}
	}
	return Record{key: append([]byte(nil), e.RecordKey...), fields: fields}
}

func valueFromEntry(ix *Index, e Entry, f string) Value {
	for i, c := range ix.def.Fields {
		if c == f && i < len(e.Values) {
			return e.Values[i]
		}
	}
	if v, ok := e.Covered[f]; ok {
		return v
	}
	return NilValue()
}

func projectRecord(r Record, need map[string]bool) Record {
	if need == nil {
		return cloneRecord(r)
	}
	out := Record{key: append([]byte(nil), r.key...), fields: make(map[string]Value, len(need))}
	for f := range need {
		if f == "" {
			continue
		}
		if v, ok := r.Get(f); ok {
			out.fields[f] = v
		} else {
			out.fields[f] = NilValue()
		}
	}
	return out
}
