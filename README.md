# secondary-index-engine-go

一个在保留“按主键读写 + 主键顺序遍历”通路的前提下，为记录提供**多字段二级索引、唯一约束、事务化原子提交、覆盖扫描、范围/前缀查询与索引重建校验**的内存型引擎。

- 模块路径：`github.com/Diudiuseven/secondary-index-engine-go`
- 包名：`sindex`
- 无第三方依赖，仅使用标准库。

---

## 1. 数据模型

- 主键：任意非空字节串 `[]byte`。主键在表内唯一，按字节序升序存储，`Engine.ScanTable(start, end, fn)` 提供半开区间 `[start, end)` 的顺序遍历（`nil` 表示无界）。
- 记录：`Record{key, fields}`，字段是具名的标量值 `Value`。
- 支持的标量类型（`Kind`）与**全序**：

  ```
  Nil < Bool(false) < Bool(true) < Int64 < Float64 < String < Bytes
  ```

  - 同类型按自然顺序比较：整数/浮点按数值，字符串与字节串按字典序；较短者若是前缀则更小。
  - 缺失字段按 `Nil` 参与索引。
  - `NaN` 不是合法的可索引浮点值。

### 构造索引

```go
db := sindex.New()
db.CreateIndex(sindex.IndexDef{
    Name:    "city_age",          // 索引名，唯一
    Fields:  []string{"city", "age"}, // 索引列（有序的复合键）
    Covered: []string{"name"},    // 附加的覆盖列（可被覆盖扫描直接取到）
    Unique:  false,               // 是否唯一索引
})
```

- `Fields` 构成有序复合索引键；索引按这些列的编码升序排列。
- `Covered` 是额外物化到每个索引项里的列（不必与 `Fields` 重复），用于覆盖扫描。
- 在已有数据的表上 `CreateIndex` 会立即从主表全量构建该索引。

---

## 2. 保序字节编码（字节序 == 字段比较序）

每个索引列被编码为：

```
tag || payload || 0x00(终止符)
```

类型标签按全序递增：

| 类型       | tag  | payload |
|------------|------|---------|
| Nil        | 0x00 | 无 |
| Bool false | 0x01 | 无 |
| Bool true  | 0x02 | 无 |
| Int64      | 0x10 | 8 字节大端，符号位翻转（有符号序 == 字节序） |
| Float64    | 0x20 | 8 字节大端 IEEE-754，负数全取反、正数翻符号位 |
| String     | 0x30 | 转义后的 UTF-8 字节 |
| Bytes      | 0x40 | 转义后的原始字节 |

转义是保序双射：

```
0x00 -> 0x00 0xFF
0xFF -> 0xFF 0x00
其它字节原样
```

因此：

1. 裸 `0x00` 只会作为列终止符出现（转义后的 `0x00` 一定是 `00 FF`，不会出现孤立的 `00`）；
2. 转义单调，负载的字节序与原始字节序完全一致；
3. 多列拼接后做一次 `bytes.Compare` 即等价于逐列值比较。

### 物理索引项键

```
encode(col1) || encode(col2) || ... || 0x00 || primaryKey
```

列编码之后用一个 `0x00` 分隔，再接原始主键。这样：

- 同一索引值在非唯一索引下可对应多条记录；
- 物理键全局唯一，主键作为最终 tie-break，使返回顺序**稳定且可复现**（先按索引值、再按主键字节序升序）。

---

## 3. 范围与前缀查询的边界语义

`Engine.Scan(RangeOptions)` 返回按物理键升序排列的结果。两种模式互斥：
设置了 `PrefixValues` 时不能再设置 `StartValues/EndValues`。

### 3.1 前缀模式 `PrefixValues`

`PrefixValues` 给出索引列的一个**前缀元组**（1～列数个值）。返回前导列恰好等于该元组的全部记录。

字节区间为半开 `[prefix, successor(prefix))`，其中：

- `prefix` 是这些列的编码，**最后一列不带终止符**（全宽前缀时仍能匹配后续的 `0x00 || pk` 区域）；
- `successor` 用经典的“从右往左找第一个非 0xFF 字节并加 1”构造，严格大于任何以 `prefix` 开头的键。

特例：

- **单列字符串/字节串的“值内前缀”**也由此支持。例如字符串列前缀 `"ab"` 会命中 `ab、abc、abd…`，不会命中 `"b"`，也不会命中 `"Az"`（大写 A 在小写 a 之前）。
- 当前缀全为 `0xFF` 时不存在有限后继键（此时上界无界）。

### 3.2 范围模式（SQL 风格行值比较）

`StartValues/EndValues` 各给出索引列的一个前缀元组，`StartBound/EndBound` 取
`Inclusive`/`Exclusive`。比较采用行值语义：边界元组与每条索引项的前若干列比较。

下界（全宽/部分元组一致）：

- `Inclusive`：无终止符的元组编码本身；
- `Exclusive`：该编码的后继键。

上界统一表达为半开区间的 `hi`：

- **部分元组**（少于索引列数）：边界代表“前导列等于该元组的所有行”，不论 Inclusive/Exclusive，`hi = successor(带终止符的部分编码)`。例如复合索引 `(city, age)` 以 `(city="BJ")` 为上界，会覆盖北京的全部 age。
- **全宽元组 Inclusive**：`hi = successor(带终止符编码)`。把末尾终止符 `0x00` 增至 `0x01`，落在主键分隔区之上、下一列值之下，因此包含同值的所有记录。
- **全宽元组 Exclusive**：`hi = 无终止符编码 + 0x00`。相等列值的物理键在该位置之后还有非空主键，故全部 `> hi` 而被排除；更大的列值也在其上。

无下界/无上界用空切片或 `nil` 表示。

### 3.3 排序与稳定性

结果严格按 `(索引列升序..., 主键字节序升序)` 返回，同一查询在同一数据状态下多次执行结果完全一致。

---

## 4. 覆盖扫描（covering scan）与可观测信息

`RangeOptions.Fields` 声明调用方需要的字段（主键总是可取）。当且仅当**每一个**请求字段都能由索引项本身提供（属于 `Fields` 或 `Covered`）时，本次扫描完全不回查主表：

```go
res, _ := db.Scan(sindex.RangeOptions{
    IndexName:    "city_age",
    PrefixValues: []sindex.Value{sindex.StringValue("BJ")},
    Fields:       []string{"city", "age", "name"}, // 全部可由索引满足
})
res.Covered       // true：全程未回表
res.TableLookups  // 0：回表次数
res.IndexItems    // 访问的索引项数
res.Limited       // 是否因 Limit 截断
```

一旦请求了非索引列，`Covered=false` 且 `TableLookups` 等于实际回表次数。返回记录只投影请求字段。索引项通过 `Covered` 携带支持覆盖所需的附加列。

---

## 5. 事务、原子性与一致性

```go
err := db.UpdateTxn(func(tx *sindex.Txn) error {
    tx.Insert(rec1)   // 主键必须不存在
    tx.Put(rec2)      // upsert
    tx.Delete(key)    // 存在才删
    return nil        // 返回 nil 才提交；返回错误则回滚
})
```

- `Insert` 是 insert-if-absent：针对**提交状态与本事务内已生效状态**判定，同事务重复插入同一主键返回 `ErrExists`。支持同键多次写（如 Insert→Delete→Insert）按序回放判定。
- `Commit` 在**一把引擎写锁**内完成全部校验与落盘：
  1. 回放每键操作历史，得到事务后状态；
  2. 折叠出每个索引的“事务后视图”，据此检查唯一约束——既能拒绝真实重复，也正确放行同事务内的唯一值**交换/移动**（A、B 两键互换唯一值合法）；
  3. 校验全部通过后，先改主表、再改相关索引；此后无失败路径。
- 因而一次提交的所有索引变更与主表变更**要么全部可见、要么全部不可见**，不存在只改一边的半更新；唯一冲突返回 `*UniqueConflictError`（含索引名、冲突值、已存在记录主键），且不留下任何部分写入。
- `Rollback` 仅丢弃私有缓冲，不触碰任何已提交状态，也不推进 `Version`。
- 读操作（`Get`、`Scan`、`ScanTable`、`Verify*`）持读锁，写操作持写锁；扫描在整段遍历期间持读锁，读到的是一个不可变的已提交快照：读不到未提交变更，也读不到某次提交的中间状态。代码通过 `go test -race`。

### 错误

| 错误 | 含义 |
|------|------|
| `ErrNotFound` | 主键不存在 |
| `ErrExists` | Insert 的主键已存在 |
| `ErrUniqueConflict` | 唯一索引冲突（具体类型 `*UniqueConflictError`） |
| `ErrIndexNotFound` / `ErrIndexExists` | 索引名不存在 / 已存在 |
| `ErrInvalidArgument` | 定义或查询参数非法（空名、重复列、越界元组、NaN 等） |
| `ErrTxnClosed` | 提交/回滚后继续使用事务 |

---

## 6. 索引重建与偏差校验

- `RebuildIndex(name)` / `RebuildAllIndexes()`：从主表全量重建，替换在读/写锁内原子完成，读者只会看到旧索引或新索引。重建结果与增量维护在物理键与覆盖列上逐字节一致（有测试保证）。
- `VerifyIndex(name)` / `VerifyAll()`：交叉核对主表与索引，返回偏差列表 `[]Drift`（空切片表示完全一致）。检测：
  - `missing_entry`：主表有记录但索引缺项；
  - `extra_or_corrupt_entry`：索引指向缺失记录、键值/覆盖列与主表不符，或存在多余项；
  - `duplicate_unique`：唯一索引中同一值出现多条。

---

## 7. 配置项与可观测性

`IndexDef` 字段即全部配置：

| 字段 | 说明 |
|------|------|
| `Name` | 必填，索引唯一名 |
| `Fields` | 必填、非空、不可重复、非空字符串：索引列，顺序即复合键顺序 |
| `Covered` | 可选，覆盖扫描附加列 |
| `Unique` | 是否唯一索引 |

`RangeOptions`：`IndexName`、`StartValues/StartBound`、`EndValues/EndBound`、
`PrefixValues`、`Fields`、`Limit`。

`ScanResult` 可观测字段：`Covered`、`TableLookups`（回表次数）、`IndexItems`、
`Limited`、`Records`。`Engine.Version()` 为每次成功提交/重建递增的单调版本号。

---

## 8. 测试

```bash
go test ./...            # 全量测试
go test -race ./...      # 含数据竞争检测
```

覆盖场景：保序编码与前缀后继键、CRUD 自动维护一致性、非唯一稳定可复现顺序、
唯一冲突（已提交/同事务/交换移动）、提交失败与回滚无残留、并发读写不读到
未提交或中间状态（`-race`）、范围/前缀边界包含与排除、覆盖扫描不回表及回表
计数、索引重建与增量一致、偏差注入检测与修复。

## 9. 当前边界

- 存储为进程内内存结构，未做持久化；并发模型为单把读写锁（简单且强一致，
  非高吞吐分片设计）。
- 复合索引支持最左前缀的范围/前缀查询。
