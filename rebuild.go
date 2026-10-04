package engine

import "fmt"

// DriftKind 标识索引与主表之间的偏差类型。
type DriftKind int

const (
	// DriftMissingEntry：主表存在记录，但索引中缺少对应索引项（或键不对）。
	DriftMissingEntry DriftKind = iota + 1
	// DriftExtraEntry：索引中存在项，但主表无对应记录（或唯一索引项的主键无法对应）。
	DriftExtraEntry
	// DriftValueMismatch：索引项存在且能定位记录，但索引值（覆盖列）与主表不一致。
	DriftValueMismatch
)

func (d DriftKind) String() string {
	switch d {
	case DriftMissingEntry:
		return "missing-index-entry"
	case DriftExtraEntry:
		return "extra-index-entry"
	case DriftValueMismatch:
		return "index-value-mismatch"
	}
	return "unknown"
}

// IndexDrift 描述一条索引偏差。
type IndexDrift struct {
	Index   string
	Primary Value
	Kind    DriftKind
	Detail  string
}

// rebuildIndex 在给定快照上从主表全量构建一个索引 treap。
// 逐条插入与增量维护使用完全相同的键/值编码，因此结果逐字节一致。
func rebuildIndex(schema *Schema, st *state, spec *IndexSpec) *treap {
	t := newTreap()
	st.primary.ascIter(nil, func(k, v []byte) bool {
		rec := decodeRecord(v)
		pk := rec[schema.PrimaryKey]
		t = t.upsert(buildIndexKey(spec, rec, pk),
			buildIndexValue(schema.PrimaryKey, spec, rec, pk))
		return false
	})
	return t
}

// RebuildIndex 从主表全量重建指定索引（name 为空时重建全部索引）。
// 重建在提交锁内以最新快照完成并随新版本原子发布，重建期间的并发读写不受影响。
func (db *DB) RebuildIndex(name string) error {
	specs, err := db.resolveIndexes(name)
	if err != nil {
		return err
	}
	db.commit.Lock()
	defer db.commit.Unlock()

	latest := db.snapshot()
	next := latest.clone()
	next.gen = latest.gen + 1
	for _, spec := range specs {
		next.indexes[spec.Name] = rebuildIndex(db.schema, latest, spec)
	}
	db.state.Store(next)
	return nil
}

// VerifyIndex 比对索引与主表，返回检测到的全部偏差（不做任何修改）。
func (db *DB) VerifyIndex(name string) ([]IndexDrift, error) {
	specs, err := db.resolveIndexes(name)
	if err != nil {
		return nil, err
	}
	st := db.snapshot()
	var drifts []IndexDrift

	for _, spec := range specs {
		idx := st.indexes[spec.Name]

		// 主表 -> 索引方向：每条记录必须恰好有一个匹配项。
		st.primary.ascIter(nil, func(pkb, recb []byte) bool {
			rec := decodeRecord(recb)
			pk := rec[db.schema.PrimaryKey]
			wantKey := buildIndexKey(spec, rec, pk)
			gotVal, ok := idx.get(wantKey)
			if !ok {
				drifts = append(drifts, IndexDrift{
					Index: spec.Name, Primary: pk, Kind: DriftMissingEntry,
					Detail: "record has no matching index entry",
				})
				return false
			}
			wantVal := buildIndexValue(db.schema.PrimaryKey, spec, rec, pk)
			if string(gotVal) != string(wantVal) {
				drifts = append(drifts, IndexDrift{
					Index: spec.Name, Primary: pk, Kind: DriftValueMismatch,
					Detail: "index entry payload differs from primary table",
				})
			}
			return false
		})

		// 索引 -> 主表方向：每个索引项必须能定位一条现存记录且键与该记录一致。
		idx.ascIter(nil, func(ik, iv []byte) bool {
			pk, pkOK := indexEntryOwner(db.schema, spec, ik, iv)
			recb, exists := st.primary.get(pkKey(pk))
			if !exists {
				drifts = append(drifts, IndexDrift{
					Index: spec.Name, Primary: pk, Kind: DriftExtraEntry,
					Detail: "index entry points to a missing primary record",
				})
				return false
			}
			rec := decodeRecord(recb)
			if !pkOK || !valuesEqual(rec[db.schema.PrimaryKey], pk) {
				drifts = append(drifts, IndexDrift{
					Index: spec.Name, Primary: pk, Kind: DriftExtraEntry,
					Detail: "index entry owner does not match primary key",
				})
				return false
			}
			wantKey := buildIndexKey(spec, rec, pk)
			if string(wantKey) != string(ik) {
				drifts = append(drifts, IndexDrift{
					Index: spec.Name, Primary: pk, Kind: DriftExtraEntry,
					Detail: fmt.Sprintf("index key does not match record (entry belongs to a different key value)"),
				})
			}
			return false
		})
	}
	return drifts, nil
}

// indexEntryOwner 从索引项（键/值）解析其所属记录的主键。
func indexEntryOwner(schema *Schema, spec *IndexSpec, key, val []byte) (Value, bool) {
	parts, err := decodeKeyParts(key)
	if err != nil {
		return NullValue(), false
	}
	if !spec.Unique {
		if len(parts) != len(spec.Key)+1 {
			return NullValue(), false
		}
		return parts[len(spec.Key)], true
	}
	entry := decodeCovered(entryValueFields(schema.PrimaryKey, spec), val)
	pk, ok := entry[schema.PrimaryKey]
	return pk, ok && !pk.IsNull()
}

func (db *DB) resolveIndexes(name string) ([]*IndexSpec, error) {
	if name == "" {
		out := make([]*IndexSpec, 0, len(db.schema.Indexes))
		for i := range db.schema.Indexes {
			out = append(out, &db.schema.Indexes[i])
		}
		return out, nil
	}
	spec, ok := db.schema.index(name)
	if !ok {
		return nil, fmt.Errorf("engine: unknown index %q", name)
	}
	return []*IndexSpec{spec}, nil
}
