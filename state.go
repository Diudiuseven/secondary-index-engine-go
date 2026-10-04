package engine

// state 是主表与全部索引在某一时刻的不可变快照。
// DB 持有的 *state 以 atomic.Pointer 整体替换：替换前的所有修改对外不可见，
// 替换后一次全部可见，天然满足"同一次提交的索引变更一起可见"。
type state struct {
	primary *treap            // key: partFull(主键值); val: encodeRecord
	indexes map[string]*treap // key: 索引键编码(见 buildIndexKey); val: buildIndexValue
	gen     uint64            // 单调递增版本号
}

func newState(schema *Schema) *state {
	st := &state{
		primary: newTreap(),
		indexes: make(map[string]*treap, len(schema.Indexes)),
	}
	for _, spec := range schema.Indexes {
		st.indexes[spec.Name] = newTreap()
	}
	return st
}

// pkKey 编码主表键（含终止符的完整列编码）。
func pkKey(pk Value) []byte {
	return partFull(pk)
}

// buildIndexKey 编码索引项键。
//
//	唯一索引：  partFull(key1) ... partFull(keyN)
//	非唯一索引：以上内容后再拼 partFull(pk)，使同一索引键值下的多条记录
//	           按主键字节序稳定排列，并保证索引项全局唯一、无重复。
func buildIndexKey(spec *IndexSpec, r Record, pk Value) []byte {
	parts := make([]Value, 0, len(spec.Key)+1)
	for _, k := range spec.Key {
		parts = append(parts, r[k])
	}
	key := encodeKey(parts)
	if !spec.Unique {
		key = append(key, partFull(pk)...)
	}
	return key
}

// uniqueEntryKey 计算唯一索引项的键（不带主键后缀）。
func uniqueEntryKey(spec *IndexSpec, r Record) []byte {
	parts := make([]Value, 0, len(spec.Key))
	for _, k := range spec.Key {
		parts = append(parts, r[k])
	}
	return encodeKey(parts)
}

// indexPrefix 编码索引等值前缀（最左 N 列的完整列编码）。
func indexPrefix(spec *IndexSpec, vals []Value) []byte {
	if len(vals) > len(spec.Key) {
		vals = vals[:len(spec.Key)]
	}
	return encodeKey(vals)
}

// entryValueFields 返回索引项值中冗余的字段集合：主键 + 覆盖列。
func entryValueFields(pkName string, spec *IndexSpec) []string {
	fields := []string{pkName}
	for _, c := range spec.Covered {
		if c != pkName {
			fields = append(fields, c)
		}
	}
	return fields
}

// buildIndexValue 编码索引项值（主键 + 覆盖列快照）。
// 索引键之外、覆盖扫描需要的附加列都冗余在这里；唯一索引的键不含主键后缀，
// 主键放在值里后扫描既能回表也能直接组装覆盖行。
func buildIndexValue(pkName string, spec *IndexSpec, r Record, pk Value) []byte {
	return encodeCovered(entryValueFields(pkName, spec), r.withPrimary(pkName, pk))
}

// clone 复制 state 外壳以便在其上构建新版本（treap 本身不可变）。
func (s *state) clone() *state {
	idx := make(map[string]*treap, len(s.indexes))
	for k, v := range s.indexes {
		idx[k] = v
	}
	return &state{primary: s.primary, indexes: idx, gen: s.gen}
}
