# secondary-index-engine

一个嵌入式（库形态、无外部依赖、内存态）的 Go 记录存储。在既有「按主键读写 + 主键顺序遍历」通路之上，增加多字段二级索引、事务原子提交、并发快照读、覆盖扫描、索引重建与一致性校验。

## 1. 数据模型与配置（Schema）

一张表由 `Schema` 描述，建表（`Open`）时一次性声明、之后不可变：

```go
schema := &engine.Schema{
    PrimaryKey: "id",
    Fields: []engine.FieldSpec{
        {Name: "id",    Kind: engine.KindInt64},
        {Name: "email", Kind: engine.KindString},
        {Name: "city",  Kind: engine.KindString},
        {Name: "age",   Kind: engine.KindInt64},
        {Name: "phone", Kind: engine.KindString},
    },
    Indexes: []engine.IndexSpec{
        {Name: "uq_email",     Key: []string{"email"}, Unique: true},
        {Name: "idx_city_age", Key: []string{"city", "age"}, Covered: []string{"phone"}},
    },
}
db, err := engine.Open(schema)
```

配置项说明：

| 配置 | 含义 |
| --- | --- |
| `Schema.PrimaryKey` | 主键字段名，必须在 `Fields` 中声明；主键不允许为 NULL。 |
| `FieldSpec.Kind` | 字段逻辑类型：`KindNull / KindBool / KindInt64 / KindUint64 / KindFloat64 / KindString / KindBytes`。 |
| `IndexSpec.Name` | 索引名，表内唯一。 |
| `IndexSpec.Key` | 索引键字段（有序，支持复合索引）；字段必须已在 `Fields` 声明。 |
| `IndexSpec.Unique` | 是否唯一索引。 |
| `IndexSpec.Covered` | 覆盖附加列：除索引键列与主键外，额外冗余进索引项的字段，用于覆盖扫描。 |

记录就是 `map[string]Value`；类型化构造器有 `Int64Value / StringValue / BytesValue / Float64Value / Uint64Value / BoolValue / NullValue`。

## 2. 读写与事务（原子性、一致性）

```go
err := db.Update(func(tx *engine.Txn) error {
    if err := tx.Put(rec1); err != nil { return err }
    if err := tx.Put(rec2); err != nil { return err }
    return tx.Delete(oldPK)
}) // fn 返回 nil 提交；返回任何 error 自动回滚
```

- `Begin()/Get/Put/Delete/Commit/Rollback` 可手动使用；`Update`/`View` 是便捷封装。
- `Put` 为整体 upsert（按主键插入或替换整行）；`Delete` 幂等。
- **索引自动维护**：每次提交在内部草稿上先删除受影响记录的全部旧索引项与旧主表项，再统一做唯一校验、插入新索引项与新主表项，最后以**一次原子状态指针替换**发布。主表与任一索引之间不存在「只改了一边」的半更新窗口；提交失败时草稿直接丢弃，已提交状态完全不变。
- **回滚**只丢弃事务本地缓冲，从不触碰已提交状态，因此索引里不会残留该事务的任何索引项。
- 隔离级别：**快照隔离（SI）**。事务读取 `Begin` 时刻的已提交版本；提交时对读集合做写-写冲突检测（first-committer-wins），冲突返回 `ErrConflict` 且不产生写入。
- 唯一约束：
  - 冲突返回包装了 `ErrUniqueViolation` 的错误，整次提交（含同批次其他行）被拒绝，不留部分写入；
  - 同一行更新但其唯一键不变时允许提交；
  - 同批次内两行抢占同一唯一键同样整体拒绝；
  - 同批次中先删除占用者、再插入同键值允许成功。
  - **NULL 语义（确定、已文档化的选择）**：NULL 是普通可比较值，唯一索引中 NULL 至多出现一条（即「NULL 也相等」，不采用 SQL 的多 NULL 例外）。

## 3. 并发模型

- 已提交状态是**不可变快照**（持久化 treap，路径复制），查询通过 `atomic.Pointer` 无锁读取，永不阻塞写者、也不会读到中间态。
- 写者在一把提交互斥锁内完成冲突检测 + 新版本构建 + 原子发布；临界区内不执行用户代码。
- 一次提交里的所有主表/索引变更要么随新快照**一起可见**，要么（回滚/冲突/唯一冲突时）**全部不可见**。
- 所有导出路径通过 `go test -race`（见 `concurrency_test.go`）。

## 4. 扫描、范围边界与覆盖扫描

### 4.1 排序与编码规则

每个索引键列编码为 `tag | escaped(payload) | 0x00(列终止符)`：

- payload 保序：整数/浮点做符号翻转后大端编码，字符串/字节串原样；
- payload 内对 `0x00 → 0x01 0x01`、`0x01 → 0x01 0x02` 转义，因此裸 `0x00` 只作列终止符——**复合键的字节序与逐字段值序完全一致**，且各列自界定（`("a","")` 与 `("a")` 编码不同）；
- NULL 的 tag 为 `0x00`，排序在所有类型最前；布尔 `false < true`；三种数值类型按数值大小混排；字符串与字节串按字节序；其余跨类型按类型 tag 排序（同一索引列 Kind 由 Schema 固定，实际查询不跨类型）。
- **非唯一索引**的项键在键列后再拼主键列，因此同一索引键值下的多条记录**按主键字节序稳定、可复现**，且项键全局唯一。
- **唯一索引**项键不含主键后缀；主键存放在项值中（见下）。

索引项值冗余 `{主键} ∪ Covered` 列的快照，支持覆盖扫描与一致性校验。

### 4.2 边界语义（包含/排除规则）

`ScanOptions{Index, Prefix, Start, End, Limit, Fields}`：

- **`Prefix []Value`**：复合索引最左 N 列的**等值前缀**，N 可小于总键列数。如索引 `(city, age)` 上 `Prefix=[BJ]` 匹配 `city=BJ` 的所有行。
- **`Start/End *Bound`** 绑定到 `Prefix` 之后的下一列（`Prefix` 为空时绑定第 1 列）：
  - `Strict=false`（默认）：**包含**边界（`>=` / `<=`）。边界按「列前缀」解释，即 `Start >= v` 同时覆盖该列等于 v、后续列任意的全部项；
  - `Strict=true`：**排除**边界（`>` / `<`），等于边界值的整段（含其任意后续列）都被排除；
  - `Prefix=true`（仅 `string/bytes`）：该列的**字节前缀**匹配（类似 `LIKE 'abc%'`），`Start` 从裸前缀开始（含），`End` 到该前缀之后（不含）。
- 范围是上述条件的交集；`End=nil` 表示正无穷；`Limit<=0` 表示不限。
- 只要给出了 `Prefix`（N>0），扫描范围始终被钳制在该等值前缀段内：Start/End 只在段内进一步收窄，绝不会串到后续前缀值（例如 `(city)=(BJ)` 上只给 `age>=30` 不会读到 `city=SH` 的行）。
- 未被边界绑定的更后列不参与比较。

字节层面精确定义（`base` = Prefix 各列完整编码的拼接；`part(v)` = 单列 tag+payload，`partFull(v)` = `part(v) || 0x00`）：

| 边界 | 下界（含） | 上界（排） |
| --- | --- | --- |
| 包含（非 Strict） | `base ‖ partFull(v)` | `successor(base ‖ partFull(v))` |
| 排除（Strict） | `successor(base ‖ partFull(v))` | `base ‖ part(v)` |
| 字节前缀 | `base ‖ part(v)` | `byteUpper(base ‖ part(v))`（全 0xFF 时为 +∞） |

其中 `successor(x)` = 去掉末尾终止符后的「字节上界」（`prefixEnd` 语义）。只要给出 `Prefix`，最终上界还会与 Prefix 段后继 `successor(base)` 取更紧者，保证扫描不越过该前缀段。

### 4.3 覆盖扫描与回表观测

```go
res, _ := tx.ScanIndex(engine.ScanOptions{
    Index:  "idx_city_age",
    Prefix: []engine.Value{engine.StringValue("BJ")},
    Start:  &engine.Bound{Value: engine.Int64Value(18)}, // age >= 18
    Fields: []string{"id", "city", "age", "phone"},      // 实际需要的列
})
res.Covered      // true ⇒ 零回表
res.TableLookups // 实际回查主表的行数
```

- 当 `Fields` 中每个字段都属于 `索引键列 ∪ {主键} ∪ Covered` 时，结果直接由索引项组装，`Covered=true`、`TableLookups=0`，**不回查主表**。
- 否则逐行用同一事务快照回表补全，`Covered=false`，`TableLookups` 为回表次数。
- 不声明 `Fields` 视为需要完整记录，总是回表。
- 唯一索引的覆盖扫描同样成立（主键取自索引项值）。

### 4.4 主键顺序遍历（既有通路）

`tx.ScanPrimary(PrimaryScanOptions{Start, End, Limit})` 按主键字节序遍历，边界语义与索引扫描单列时相同。

## 5. 索引重建与一致性校验

```go
_ = db.RebuildIndex("idx_city_age") // 重建单个；传 "" 重建全部
drifts, _ := db.VerifyIndex("")     // 校验全部；返回每条偏差
```

- 重建在提交锁内以最新已提交快照为基础，逐条主表记录用**与增量维护完全相同**的键/值编码构建新 treap，再原子发布；重建期间并发读写不受影响，重建结果与增量维护结果逐字节一致（测试 `TestRebuildEqualsIncremental`）。
- `VerifyIndex` 做双向比对：
  - 主表 → 索引：缺项报 `DriftMissingEntry`，键在但覆盖列/主键快照不符报 `DriftValueMismatch`；
  - 索引 → 主表：悬空项、所有者主键不存在、索引键与记录不符报 `DriftExtraEntry`。
- 校验只读，不修改数据；检测到偏差后可调用 `RebuildIndex` 修复，再校验应得到 0 条偏差。

## 6. 错误与可观测信息

| 错误 | 触发场景 |
| --- | --- |
| `ErrNotFound` | `Get` 的主键不存在。 |
| `ErrConflict` | 快照过期导致的写-写冲突，事务被中止。 |
| `ErrUniqueViolation` | 唯一索引键冲突（含批内冲突），整次提交被拒绝。 |

`ScanResult{Rows, Covered, TableLookups}` 提供「是否覆盖、回表次数」观测；`IndexDrift{Index, Primary, Kind, Detail}` 提供偏差定位信息。

## 7. 测试

```sh
go test ./...            # 全部用例
go test -race ./...      # 含数据竞争检测
go test -run . -count=10 # 重复执行
```

覆盖场景：编码保序与自界定、记录序列化往返、主键 CRUD、唯一冲突（跨事务/批内/删后插/同键更新）、非唯一索引稳定顺序、提交回滚无残留、快照隔离与冲突、并发混合读写、覆盖扫描回表计数、范围/前缀各边界、从主表重建与增量结果逐字节一致、偏差注入后检测与修复。

## 8. 当前范围与非目标

- 存储为进程内数据结构，无磁盘持久化 / WAL（现有主表通路即内存态）。
- 索引集合建表后固定；新增索引可借助 `RebuildIndex` 的同类路径扩展。
