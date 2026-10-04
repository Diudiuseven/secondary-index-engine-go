package engine

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	// ErrConflict 表示事务提交时发现写-写冲突（快照之后本事务读过的键被其他事务改动过）。
	ErrConflict = errors.New("engine: transaction conflict")
	// ErrNotFound 表示按主键读取的记录不存在。
	ErrNotFound = errors.New("engine: record not found")
	// ErrUniqueViolation 表示唯一索引上出现重复键，本次写入已被整体拒绝。
	ErrUniqueViolation = errors.New("engine: unique index violation")
	errTxClosed        = errors.New("engine: transaction already committed or rolled back")
)

// DB 是一张带二级索引的表。主表写入与全部相关索引变更在同一把提交锁下
// 基于同一基底状态构建，并以单次原子指针替换对外发布，因此不存在
// "只改了一边"的半更新窗口。
type DB struct {
	schema *Schema
	commit sync.Mutex // 串行化冲突检测 + 新版本构建
	state  atomicState
}

// Open 按 schema 打开（创建）表。
func Open(schema *Schema) (*DB, error) {
	if schema == nil {
		return nil, errors.New("engine: nil schema")
	}
	if err := schema.validate(); err != nil {
		return nil, err
	}
	db := &DB{schema: schema}
	db.state.Store(newState(schema))
	return db, nil
}

func (db *DB) Schema() *Schema { return db.schema }

func (db *DB) snapshot() *state { return db.state.Load() }

// Begin 开启一个事务（快照隔离）。事务内读写均基于 Begin 时刻的已提交数据。
func (db *DB) Begin() *Txn {
	st := db.snapshot()
	return &Txn{
		db:      db,
		read:    st,
		readGen: st.gen,
		writes:  make(map[string]txWrite),
		reads:   make(map[string]bool),
	}
}

// Update 在读写事务中执行 fn：fn 返回 nil 时提交，否则回滚并返回该错误。
func (db *DB) Update(fn func(tx *Txn) error) error {
	tx := db.Begin()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// View 在只读事务中执行 fn（保证读到同一快照）。
func (db *DB) View(fn func(tx *Txn) error) error {
	tx := db.Begin()
	tx.readOnly = true
	defer tx.Rollback()
	return fn(tx)
}

// Txn 是一次事务句柄；非并发安全，一个事务只应由一个 goroutine 使用。
type Txn struct {
	db       *DB
	read     *state
	readGen  uint64
	writes   map[string]txWrite // key: string(pkKey)
	reads    map[string]bool    // key: string(pkKey)，写-写冲突检测用
	readOnly bool
	closed   bool
}

type txWrite struct {
	pk      Value
	rec     Record
	deleted bool
}

// currentRecord 返回主键在事务视角下的记录（叠加本地缓冲），ok=false 即不存在。
func (tx *Txn) currentRecord(k []byte, pk Value) (Record, bool) {
	if w, ok := tx.writes[string(k)]; ok {
		if w.deleted {
			return nil, false
		}
		return w.rec, true
	}
	if v, ok := tx.read.primary.get(k); ok {
		return decodeRecord(v), true
	}
	return nil, false
}

// Get 按主键读取（事务一致读）。
func (tx *Txn) Get(pk Value) (Record, error) {
	if tx.closed {
		return nil, errTxClosed
	}
	k := pkKey(pk)
	tx.reads[string(k)] = true
	r, ok := tx.currentRecord(k, pk)
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

// Put 插入或整体替换一条记录（主键取记录中的 Schema.PrimaryKey 字段）。
func (tx *Txn) Put(r Record) error {
	if tx.closed {
		return errTxClosed
	}
	if tx.readOnly {
		return errors.New("engine: cannot write in a read-only transaction")
	}
	pk, ok := r[tx.db.schema.PrimaryKey]
	if !ok || pk.IsNull() {
		return fmt.Errorf("engine: record is missing primary key field %q", tx.db.schema.PrimaryKey)
	}
	cp := make(Record, len(r)) // 防御性拷贝，避免提交前被外部改动
	for k, v := range r {
		cp[k] = v
	}
	k := string(pkKey(pk))
	tx.reads[k] = true
	tx.writes[k] = txWrite{pk: pk, rec: cp}
	return nil
}

// Delete 按主键删除；记录不存在时为无操作（幂等）。
func (tx *Txn) Delete(pk Value) error {
	if tx.closed {
		return errTxClosed
	}
	if tx.readOnly {
		return errors.New("engine: cannot write in a read-only transaction")
	}
	k := string(pkKey(pk))
	tx.reads[k] = true
	tx.writes[k] = txWrite{pk: pk, deleted: true}
	return nil
}

// Rollback 放弃事务：只丢弃本地缓冲，不触碰任何已提交状态，
// 索引中不会残留本次事务的任何痕迹。
func (tx *Txn) Rollback() {
	tx.closed = true
	tx.writes = nil
	tx.reads = nil
}

type preparedChange struct {
	w          txWrite
	old        Record
	oldOK      bool
	skipDelete bool // 旧值与快照相同、且本事务此前已处理（同一主键只保留最后一条，恒为 false）
}

// Commit 原子提交事务。
//
// 持提交锁后：
//  1. 基于最新已提交版本做写-写冲突检测（first-committer-wins 的快照隔离）；
//  2. 阶段一：在私有草稿上移除所有受影响记录的旧主表项与旧索引项；
//  3. 阶段二：批内唯一去重 + 对照草稿检查唯一约束，通过后插入新主表项与新索引项；
//  4. 单次原子指针替换发布新版本。
//
// 任何一步失败都只丢弃草稿，已提交状态（主表与全部索引）完全不变。
func (tx *Txn) Commit() error {
	if tx.closed {
		return errTxClosed
	}
	tx.closed = true
	if tx.readOnly || len(tx.writes) == 0 {
		return nil
	}

	tx.db.commit.Lock()
	defer tx.db.commit.Unlock()

	latest := tx.db.snapshot()

	// 1) 冲突检测：本事务读过的主键若在快照之后被其他事务改动，则中止。
	if latest.gen != tx.readGen {
		for k := range tx.writes {
			tx.reads[k] = true
		}
		if changedKeys(tx.read.primary, latest.primary, tx.reads) {
			return ErrConflict
		}
	}

	// 按主键排序使构建过程确定（treap 形状本身只依赖键，不依赖插入顺序）。
	changes := make([]preparedChange, 0, len(tx.writes))
	for _, w := range tx.writes {
		old, oldOK := getCommitted(latest, w.pk)
		changes = append(changes, preparedChange{w: w, old: old, oldOK: oldOK})
	}
	sort.Slice(changes, func(i, j int) bool {
		return compareBytes(pkKey(changes[i].w.pk), pkKey(changes[j].w.pk)) < 0
	})

	next := latest.clone()
	next.gen = latest.gen + 1

	// 2) 阶段一：删除全部旧项（主表 + 索引）。
	for _, c := range changes {
		removeOld(tx.db.schema, next, c)
	}

	// 3) 阶段二：唯一检查（批内去重 + 对照草稿），通过后插入全部新项。
	for i := range tx.db.schema.Indexes {
		spec := &tx.db.schema.Indexes[i]
		if !spec.Unique {
			continue
		}
		seen := make(map[string]int)
		for ci, c := range changes {
			if c.w.deleted {
				continue
			}
			ek := string(uniqueEntryKey(spec, c.w.rec))
			if prev, dup := seen[ek]; dup {
				return fmt.Errorf("%w: index %q (records with pk %v / %v)",
					ErrUniqueViolation, spec.Name, changes[prev].w.pk, c.w.pk)
			}
			seen[ek] = ci
			if _, occupied := next.indexes[spec.Name].get([]byte(ek)); occupied {
				return fmt.Errorf("%w: index %q", ErrUniqueViolation, spec.Name)
			}
		}
	}
	for _, c := range changes {
		if c.w.deleted {
			continue
		}
		insertNew(tx.db.schema, next, c.w.pk, c.w.rec)
	}

	// 4) 原子发布：主表与所有索引在新版本中一起可见。
	tx.db.state.Store(next)
	tx.read = next
	return nil
}

func getCommitted(st *state, pk Value) (Record, bool) {
	if v, ok := st.primary.get(pkKey(pk)); ok {
		return decodeRecord(v), true
	}
	return nil, false
}

func removeOld(schema *Schema, st *state, c preparedChange) {
	if !c.oldOK {
		return
	}
	for i := range schema.Indexes {
		spec := &schema.Indexes[i]
		st.indexes[spec.Name] = st.indexes[spec.Name].delete(buildIndexKey(spec, c.old, c.w.pk))
	}
	st.primary = st.primary.delete(pkKey(c.w.pk))
}

func insertNew(schema *Schema, st *state, pk Value, rec Record) {
	for i := range schema.Indexes {
		spec := &schema.Indexes[i]
		st.indexes[spec.Name] = st.indexes[spec.Name].upsert(
			buildIndexKey(spec, rec, pk), buildIndexValue(schema.PrimaryKey, spec, rec, pk))
	}
	st.primary = st.primary.upsert(pkKey(pk), encodeRecord(rec))
}

// changedKeys 判断 keys 中是否存在在两个主表版本之间发生变化的键。
func changedKeys(old, neu *treap, keys map[string]bool) bool {
	for k := range keys {
		ov, ook := old.get([]byte(k))
		nv, nok := neu.get([]byte(k))
		if ook != nok || (ook && string(ov) != string(nv)) {
			return true
		}
	}
	return false
}
