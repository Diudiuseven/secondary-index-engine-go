package engine

// Bound 表示扫描边界（绑定到复合索引当前比较位置的那一列）。
type Bound struct {
	// Value 是边界列的值（Kind 必须与对应索引列一致）。
	Value Value
	// Strict=true 表示排他边界（> 或 <），false 表示包含边界（>= 或 <=）。
	Strict bool
	// Prefix=true 时 Value 必须是字符串/字节串，语义为该列上的字节前缀
	// （LIKE 'abc%'），而非等值。此时：
	//   Start: 下界为该前缀的起点（含），等价于 >= 裸前缀；
	//   End:   上界为该前缀之后（排他），即所有此前缀列值之后的位置。
	Prefix bool
}

// ScanOptions 描述一次二级索引扫描。
//
// 三种键范围指定方式可组合，最终范围取交集：
//
//   - Prefix：复合索引最左 N 列的等值前缀，N 可以小于索引键列数。
//     例如索引 (a,b) 上 Prefix=[1] 表示 a=1 的所有记录。
//   - Start/End：绑定到“Prefix 之后的下一列”的范围边界。
//     Prefix 长度为 N 时，Start/End 作用于第 N+1 列；Prefix 为空时作用于第 1 列。
//     未被边界绑定的后续列不参与比较（因此 Start 上的等值会同时匹配
//     该列相等、后续列任意的全部索引项；这是“前缀式”边界的固有语义）。
//   - Limit：最多返回行数。
type ScanOptions struct {
	Index  string
	Prefix []Value
	Start  *Bound
	End    *Bound
	Limit  int
	// Fields 声明查询实际需要的记录字段。
	// 当这些字段全部能由“索引键列 + 主键 + 覆盖列”提供时，扫描为覆盖扫描，
	// 不回查主表；否则逐行回表补全。
	Fields []string
}

// Row 是一行扫描结果。
type Row struct {
	Record  Record
	Primary Value
}

// ScanResult 是扫描结果及回表可观测信息。
type ScanResult struct {
	Rows         []Row
	Covered      bool // 本次扫描是否完全被索引覆盖（零回表）
	TableLookups int  // 实际回查主表的次数
}

// scanRange 计算索引 treap 上的字节范围。
type scanRange struct {
	start []byte // 含
	end   []byte // 排；nil 表示 +∞
}

func computeScanRange(spec *IndexSpec, opts ScanOptions) (scanRange, error) {
	n := len(opts.Prefix)
	if n > len(spec.Key) {
		return scanRange{}, errInvalidScan("Prefix 列数超过索引键列数")
	}
	base := encodeKey(opts.Prefix) // 完整列编码拼接
	r := scanRange{start: append([]byte(nil), base...), end: nil}

	boundCol := n // Start/End 绑定的列下标（0-based）
	haveColBound := opts.Start != nil || opts.End != nil
	if haveColBound {
		if boundCol >= len(spec.Key) {
			return scanRange{}, errInvalidScan("Start/End 没有可绑定的剩余索引列（Prefix 已覆盖全部键列）")
		}
		if err := checkBoundKind(spec.Key[boundCol], opts.Start); err != nil {
			return scanRange{}, err
		}
		if err := checkBoundKind(spec.Key[boundCol], opts.End); err != nil {
			return scanRange{}, err
		}
	}

	// 下界。
	if opts.Start != nil {
		b := opts.Start
		if b.Prefix {
			if err := checkStringPrefix(b.Value); err != nil {
				return scanRange{}, err
			}
			// 裸部分编码（无终止符）：匹配以该字节串开头的列值，含其自身。
			r.start = append(append([]byte(nil), base...), encodeKeyPart(b.Value)...)
		} else if b.Strict {
			// 排他下界：该列值 == b.Value 且后续列任意的项都应被排除，
			// 因此起点取“该列完整编码的严格后继”（终止符视为该列值的结束位置）。
			full := append(append([]byte(nil), base...), partFull(b.Value)...)
			r.start = nextByteKey(full)
		} else {
			// 包含下界：partFull 之后不追加任何字节，等值 + 更长复合键都会被匹配。
			r.start = append(append([]byte(nil), base...), partFull(b.Value)...)
		}
	}

	// 上界。
	if opts.End != nil {
		b := opts.End
		if b.Prefix {
			if err := checkStringPrefix(b.Value); err != nil {
				return scanRange{}, err
			}
			// 前缀之后（排他）：裸部分编码 + 字节上界。
			raw := append(append([]byte(nil), base...), encodeKeyPart(b.Value)...)
			r.end = byteUpper(raw)
		} else if b.Strict {
			// 排他上界：该列值 == b.Value 的“前缀段”整体排除，
			// 其上界就是裸部分编码（不含终止符），因为
			// partFull(v) = raw(v) || 0x00，而任何非空转义字节都 >= 0x01。
			r.end = append(append([]byte(nil), base...), encodeKeyPart(b.Value)...)
		} else {
			// 包含上界：覆盖该列 == v 且后续列任意的全部项，
			// 上界取该列完整编码的严格后继。
			full := append(append([]byte(nil), base...), partFull(b.Value)...)
			r.end = nextByteKey(full)
		}
	}
	// 等值 Prefix 始终把范围限制在该前缀段内：段上界为 base 的严格后继。
	// Start/End 给出的 r.end 是段内更紧的上界（其值必然 <= 段上界），
	// 因此只在调用方未提供 End 时补段上界，既不会漏数据，也不会串到后续前缀。
	if n > 0 && r.end == nil {
		r.end = nextByteKey(base)
	}
	return r, nil
}

// nextByteKey 返回严格大于以 key 为“完整段前缀”的所有串的最短上界。
// 对 partFull 输出（以 0x00 结束）而言，这等价于 prefixEnd(key)。
func nextByteKey(key []byte) []byte {
	if len(key) > 0 && key[len(key)-1] == keyTerm {
		return byteUpper(key[:len(key)-1])
	}
	return byteUpper(key)
}

func checkBoundKind(colName string, b *Bound) error {
	if b == nil {
		return nil
	}
	if b.Prefix {
		return checkStringPrefix(b.Value)
	}
	if b.Value.IsNull() {
		// NULL 允许作为边界（排序中最小），无需 Kind 比对。
		return nil
	}
	return nil
}

func checkStringPrefix(v Value) error {
	if v.kind != KindString && v.kind != KindBytes {
		return errInvalidScan("Prefix 边界只支持 string/bytes 类型")
	}
	return nil
}

type scanError string

func (e scanError) Error() string { return "engine: " + string(e) }

func errInvalidScan(msg string) error { return scanError(msg) }

// ScanIndex 在指定索引上执行范围/前缀扫描。
func (tx *Txn) ScanIndex(opts ScanOptions) (*ScanResult, error) {
	if tx.closed {
		return nil, errTxClosed
	}
	spec, ok := tx.db.schema.index(opts.Index)
	if !ok {
		return nil, scanError("unknown index " + opts.Index)
	}
	for i, p := range opts.Prefix {
		col := spec.Key[i]
		kind, _ := tx.db.schema.fieldKind(col)
		if !p.IsNull() && p.kind != kind {
			return nil, errInvalidScan("Prefix 值类型与索引列 " + col + " 不一致")
		}
	}
	rng, err := computeScanRange(spec, opts)
	if err != nil {
		return nil, err
	}

	// 覆盖判定：所需字段 ∈ 索引键列 ∪ {主键} ∪ 覆盖列。
	available := make(map[string]bool, len(spec.Key)+len(spec.Covered)+1)
	available[tx.db.schema.PrimaryKey] = true
	for _, k := range spec.Key {
		available[k] = true
	}
	for _, c := range spec.Covered {
		available[c] = true
	}
	needAll := len(opts.Fields) == 0
	covered := true
	if !needAll {
		for _, f := range opts.Fields {
			if !available[f] {
				covered = false
				break
			}
		}
	} else {
		// 未声明 Fields 视为需要完整记录，必须回表。
		covered = false
	}

	idx := tx.read.indexes[opts.Index]
	valFields := entryValueFields(tx.db.schema.PrimaryKey, spec)

	res := &ScanResult{Covered: covered}
	limit := opts.Limit
	idx.ascIter(rng.start, func(k, v []byte) bool {
		if rng.end != nil && compareBytes(k, rng.end) >= 0 {
			return true
		}
		if limit > 0 && len(res.Rows) >= limit {
			return true
		}

		// 从索引键还原键列；唯一索引无主键后缀，主键从值中取。
		keyParts, err := decodeKeyParts(k)
		if err != nil {
			return true
		}
		entry := decodeCovered(valFields, v)

		var pk Value
		if spec.Unique {
			pk = entry[tx.db.schema.PrimaryKey]
		} else {
			pk = keyParts[len(spec.Key)]
		}

		if covered {
			rec := Record{}
			for i, col := range spec.Key {
				rec[col] = keyParts[i]
			}
			for col, val := range entry {
				rec[col] = val
			}
			if len(opts.Fields) > 0 {
				projected := Record{}
				for _, f := range opts.Fields {
					if val, ok := rec[f]; ok {
						projected[f] = val
					} else {
						projected[f] = NullValue()
					}
				}
				rec = projected
			}
			res.Rows = append(res.Rows, Row{Record: rec, Primary: pk})
			return false
		}

		// 回表：主表读取走事务快照（保证行内容与索引来自同一版本）。
		full, ok := tx.read.primary.get(pkKey(pk))
		if !ok {
			// 理论上不会发生（索引与主表在同一 state 中）；跳过该行。
			return false
		}
		res.TableLookups++
		rec := decodeRecord(full)
		if len(opts.Fields) > 0 {
			projected := Record{}
			for _, f := range opts.Fields {
				if val, ok := rec[f]; ok {
					projected[f] = val
				} else {
					projected[f] = NullValue()
				}
			}
			rec = projected
		}
		res.Rows = append(res.Rows, Row{Record: rec, Primary: pk})
		return false
	})
	return res, nil
}

// decodeKeyParts 把复合键解码为各列值（含非唯一索引末尾的主键列）。
func decodeKeyParts(key []byte) ([]Value, error) {
	var out []Value
	pos := 0
	for pos < len(key) {
		if pos >= len(key) {
			return nil, errInvalidScan("corrupt index key")
		}
		tag := key[pos]
		pos++
		var payload []byte
		for pos < len(key) {
			c := key[pos]
			pos++
			if c == keyTerm {
				break
			}
			if c == 0x01 {
				if pos >= len(key) {
					return nil, errInvalidScan("corrupt escape in index key")
				}
				esc := key[pos]
				pos++
				switch esc {
				case 0x01:
					payload = append(payload, 0x00)
				case 0x02:
					payload = append(payload, 0x01)
				default:
					return nil, errInvalidScan("corrupt escape marker in index key")
				}
				continue
			}
			payload = append(payload, c)
		}
		v, err := decodeOrderedPayload(tag, payload)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeOrderedPayload(tag byte, p []byte) (Value, error) {
	switch tag {
	case tagNull:
		return NullValue(), nil
	case tagBool:
		if len(p) != 1 {
			return NullValue(), errInvalidScan("bad bool key part")
		}
		return BoolValue(p[0] != 0), nil
	case tagInt64:
		if len(p) != 8 {
			return NullValue(), errInvalidScan("bad int64 key part")
		}
		u := beUint64(p) ^ (1 << 63)
		return Int64Value(int64(u)), nil
	case tagUint64:
		if len(p) != 8 {
			return NullValue(), errInvalidScan("bad uint64 key part")
		}
		return Uint64Value(beUint64(p)), nil
	case tagFloat64:
		if len(p) != 8 {
			return NullValue(), errInvalidScan("bad float64 key part")
		}
		u := beUint64(p)
		if u&(1<<63) != 0 {
			u ^= 1 << 63
		} else {
			u = ^u
		}
		return Float64Value(float64FromOrderedBits(u)), nil
	case tagString:
		return StringValue(string(p)), nil
	case tagBytes:
		return BytesValue(append([]byte(nil), p...)), nil
	}
	return NullValue(), errInvalidScan("unknown key part tag")
}

// PrimaryScanOptions 主键顺序遍历。
type PrimaryScanOptions struct {
	Start *Bound
	End   *Bound
	Limit int
}

// ScanPrimary 按主键字节序顺序遍历主表。
func (tx *Txn) ScanPrimary(opts PrimaryScanOptions) ([]Record, error) {
	if tx.closed {
		return nil, errTxClosed
	}
	tmp := ScanOptions{Start: opts.Start, End: opts.End, Limit: opts.Limit}
	// 直接复用单列边界计算（Prefix 为空，Start/End 绑定主键列）。
	rng, err := func() (scanRange, error) {
		fake := &IndexSpec{Key: []string{tx.db.schema.PrimaryKey}}
		return computeScanRange(fake, tmp)
	}()
	if err != nil {
		return nil, err
	}
	var out []Record
	tx.read.primary.ascIter(rng.start, func(k, v []byte) bool {
		if rng.end != nil && compareBytes(k, rng.end) >= 0 {
			return true
		}
		if opts.Limit > 0 && len(out) >= opts.Limit {
			return true
		}
		out = append(out, decodeRecord(v))
		return false
	})
	return out, nil
}
