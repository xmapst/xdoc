# xdoc

单文件、无服务进程的嵌入式文档数据库。事务、索引、查询表达式，全部在进程内。

磁盘格式是逐字节定死的：8192 字节的页、定长页头与槽位表、带编号的值类型、跨类型的
固定排序。页 0 偏移 32 起那 27 字节是格式标识，打开时**逐字节比对**，差一个字节就当
不是本格式的文件。细节与改法见[格式标识](#格式标识)。

```go
db, err := xdoc.Open("game.db")
if err != nil {
    return err
}
defer db.Close()

users := db.Typed[User]("users")
id, err := users.InsertOne(ctx, User{Name: "阿伊", Level: 12})

top, err := users.Query().
    Where("$.level > @min").Param("min", 10).
    OrderByDesc("$.level").
    Limit(20).
    Slice(ctx)
```

---

## 目录

- [什么时候该用它](#什么时候该用它)
- [安装与最小示例](#安装与最小示例)
- [包结构](#包结构)
- [数据模型](#数据模型)
- [集合与文档](#集合与文档)
- [结构体映射](#结构体映射)
- [索引](#索引)
  - [向量索引](#向量索引)
- [查询](#查询)
- [文件存储](#文件存储)
- [事务](#事务)
- [并发模型](#并发模型)
  - [共享模式（跨进程）](#共享模式跨进程)
- [持久性与崩溃语义](#持久性与崩溃语义)
- [文件布局与运维](#文件布局与运维)
  - [库级设置（pragma）](#库级设置pragma)
  - [运行时统计与日志](#运行时统计与日志)
  - [系统虚拟集合](#系统虚拟集合)
  - [把查询结果写成文件](#把查询结果写成文件)
- [错误处理](#错误处理)
- [性能](#性能)
- [SQL](#sql)
  - [投影列表](#投影列表)
  - [多个排序键](#多个排序键)
  - [不带 FROM 的 SELECT](#不带-from-的-select)
  - [`SELECT ... INTO`](#select--into)
  - [哪些地方收 JSON，哪些地方收表达式](#哪些地方收-json哪些地方收表达式)
  - [`UPDATE ... SET` 的键](#update--set-的键)
  - [名字里的非 ASCII 字符](#名字里的非-ascii-字符)
  - [事务里能做什么](#事务里能做什么)
  - [`REBUILD`](#rebuild)
- [限制](#限制)
- [内部结构](#内部结构)

---

## 什么时候该用它

**合适**：单进程持有全部数据、需要事务与索引、不想引入一个数据库进程。
配置、存档、离线索引、边缘节点的本地状态、需要随程序一起分发的数据集。

**不合适**：需要网络访问；数据量远超内存且以随机范围扫描为主；需要主从复制。

多进程同时读写同一个文件是**可以**的，但要显式换成[共享模式](#共享模式跨进程)：
默认的直连模式不取任何文件锁，两个进程同时打开会互相写坏。共享模式每个操作都要
重开一次库（实测约 13 毫秒），而且一个做过写事务的进程会把库攥到自己退出——
先读完那一节再决定值不值。

---

## 安装与最小示例

```go
import "github.com/xmapst/xdoc"
```

```go
ctx := context.Background()

db, err := xdoc.Open("game.db")
if err != nil {
    return err
}
defer db.Close()

users := db.Collection("users")

// 写
id, err := users.InsertOne(ctx, xdoc.Doc(
    "name", "阿伊",
    "level", 12,
    "tags", []string{"pvp", "guild"},
))

// 读
doc, err := users.FindByID(ctx, id)
if errors.Is(err, xdoc.ErrNotFound) {
    // 没有这条记录
}

// 遍历
for d, err := range users.All(ctx, xdoc.Asc) {
    if err != nil {
        return err
    }
    fmt.Println(d.Get("name"))
}
```

集合不需要预先创建，第一次写入时自动建出来，连同它的主键索引。

---

## 包结构

**只有一个公开包**：`github.com/xmapst/xdoc`。文档模型、结构体映射器、JSON 收发、
排序规则、`Decimal` 与 `Guid`，入口全在它上面，一句 import 就够。

内部按职责切成了十几个包：页、事务日志、B 树、外部排序、查询执行、表达式的解析与
求值、文档模型、结构体映射。它们都在 `internal/` 里，不对外暴露。公开出去等于把磁盘
布局、语法树、映射计划的形状一并变成版本承诺，此后每一次页结构调整、每一种新增的
表达式节点都成了破坏性改动。

这不意味着够不着它们提供的能力。表达式写在 `Where("…")` 这样的字符串里，出错用
[`IsExprError`](#错误处理) 判别；数据文件层的错误码用一个结构性接口取，见
[数据文件层的错误码](#数据文件层的错误码)；文档模型的类型全部用**别名**提到了根包，
所以 `xdoc.Document` 就是那个类型本身，不是包了一层的壳——它的方法、包括
`Mapper` 上那几个泛型方法，都照常可用：

```go
var d *xdoc.Document = xdoc.Doc("a", 1)
d.Set("b", xdoc.Int32(2))              // 方法直接调

m := xdoc.NewMapper()
m.RegisterSubtype[Cat]("cat")          // 方法级泛型也照常
```

---

## 数据模型

一篇**文档**是一组有序的字段。字段名**大小写不敏感**（`name` 与 `Name` 是同一个
字段），但保留第一次写入时的拼法。顺序按插入序。

主键 `_id` 是例外，但只在**写出去的时候**：序列化把它排到最前、键名规范成小写，
所以从库里读回来的文档 `_id` 总在第一个。刚在内存里 `Set` 上去的那一篇仍是插入序
——`Keys()` 给的是插入序，`Elements()` 给的是写出去的那个顺序。

支持的值类型，按排序顺序：

| 类型 | 说明 |
|---|---|
| `MinValue` / `MaxValue` | 两端哨兵，不能做主键或索引键 |
| `Null` | 空值 |
| `Int32` / `Int64` | 整数 |
| `Double` | 64 位浮点 |
| `Decimal` | 十进制定点数，不丢精度 |
| `String` | UTF-8 字符串（严格校验，非法字节会被拒绝） |
| `Document` / `Array` | 嵌套 |
| `Binary` | 字节串 |
| `ObjectID` | 12 字节、按时间递增的标识 |
| `GUID` | 16 字节随机标识 |
| `Boolean` | 布尔 |
| `DateTime` | 时刻，精度到毫秒，只有一种身份（世界时） |

**跨类型可比**：不同类型的值按上表顺序排，数字之间按数值排（跨宽度也一样）。
这一点决定索引里键的物理顺序。

构造值：

```go
d := xdoc.Doc("name", "阿伊", "level", 12, "score", 98.5)
d.Set("tags", xdoc.Arr(xdoc.String("a"), xdoc.String("b")))

v := d.Get("level")            // 取不到返回 Null，不会是 nil
if v.IsNull() { … }            // 判空值——包括"字段不存在"和"字段就是空值"
n, ok := v.AsInt64()           // 类型不符时 ok 为假
```

`Doc` 的值会走一次自动转换，Go 的基本类型、切片、映射、结构体都能直接写进去：

| Go 类型 | 存成 |
|---|---|
| `int` / `int64` / `uint` … | `Int64` |
| `int32` / `int16` / `int8` | `Int32` |
| `float64` / `float32` | `Double` |
| `string` | `String` |
| `bool` | `Boolean` |
| `[]byte` | `Binary` |
| `time.Time` | `DateTime`（截到毫秒） |
| 切片 / 数组 | `Array` |
| 映射 / 结构体 | `Document` |

注意 **Go 的 `int` 存成 `Int64`**，不是 `Int32`——`int` 在这个平台上就是 64 位。
要显式指定宽度就写 `xdoc.Int32(12)`。这个区别不影响查询与主键匹配：
数值之间按数值比较，跨宽度也一样，`Int64(1)` 与 `Int32(1)` 相等。

### 打印与 JSON

`Document`、`Array`、`Value` 都有 `String()`，印出来便于读，但**它不是任何一种落盘
编码**——键名不加引号正是为了让它一眼看上去不能当数据用。自引用和过深的嵌套会被
截断成 `{…}`，所以拿它打印任何文档都不会把进程带崩：

```go
fmt.Println(d)     // {name: 阿伊, level: 12, tags: [a, b], addr: {city: 北京}}
```

要真正的 JSON 用这一族：

```go
b, err := xdoc.MarshalJSON(xdoc.DocValue(d))          // {"name":"阿伊","level":{"$numberLong":"12"},…}
bi, err := xdoc.MarshalJSONIndent(xdoc.DocValue(d))   // 带缩进
v, err := xdoc.UnmarshalJSON([]byte(`{"a":1}`))       // JSON 文本 -> *xdoc.Value
```

名字带 `JSON` 后缀，是为了和 [`DB.Marshal`](#结构体映射) / `DB.Unmarshal` 分开——那
两个走的是 Go 结构体与文档之间的映射，和文本没有关系，而两件事经常接在一起做。
要从一个流里连续读多个值（导出文件、日志式的追加写就是这个形状），用
`xdoc.NewJSONReader`，它不必把整份文本先读进内存；反方向用 `xdoc.NewJSONWriter`，
它边拼边刷，不必先在内存里把整篇文本拼出来（`Pretty` 与 `Indent` 两个字段控制排版）。

**标准库的 `encoding/json` 对文档不起作用。** `Document` 的字段一个都不导出，所以
`json.Marshal(d)` 返回的是 `{}`，而且 `err == nil`——一个看起来成功的空结果，比报错
难查得多。这不是可以顺手修掉的疏漏：让 `Document` 实现 `json.Marshaler` 得在定义它
的那个包里加方法，而 JSON 编码在另一个包、后者又依赖前者，反过来 import 会成环。
收发 JSON 一律走 `MarshalJSON` / `UnmarshalJSON`。

---

## 集合与文档

```go
c := db.Collection("users")

n, err := c.Insert(ctx, doc1, doc2, doc3)   // 整批一个事务
id, err := c.InsertOne(ctx, doc)            // 返回主键

n, err := c.Update(ctx, doc1, doc2)         // 按主键改写
r, err := c.Upsert(ctx, doc)                // r.Inserted / r.Updated
n, err := c.Delete(ctx, id1, id2)

doc, err := c.FindByID(ctx, id)             // 找不到返回 ErrNotFound
ok, err := c.Exists(ctx, id)
n, err := c.Count(ctx)

for d, err := range c.All(ctx, xdoc.Asc) { … }
```

几处需要留意的语义：

- **整批一个事务**。`Insert` 里任何一篇失败（比如撞唯一索引），**整批都不生效**。
  这不是保守：一篇文档要写进数据页、还要在每条索引上各建一个节点，冲突是建到
  某条索引时才发现的，那时数据块和前面几条索引的节点都已经落下去了。
- **`Update` 返回的是真的改掉了几条**，找不到的既不算失败也不算更新。
  这个数和你提交的条数不一致时，往往正是要查的地方。
- **`Update` / `Upsert` 里出错的那篇原样不动**。撞唯一索引、键超长、索引表达式
  求值失败，都在写数据块之前查出来，所以即便在事务里忽略这个错误照样提交，
  那篇文档也还是旧值、旧索引，不会出现"数据是新的、索引里却查不到"。
- **`Upsert` 分两个数**，因为调用方通常要区别对待"新来的"和"改过的"。
- **`Delete` 静默跳过不存在的主键**，返回真的删掉的条数。
- **`Count` 要走一遍索引**，不是现成的计数。需要频繁读就自己维护一个。

主键没带时自动生成，方式由 `WithAutoID` 决定，默认是按时间递增的 `ObjectID`：

```go
db, _ := xdoc.Open(path, xdoc.WithAutoID(xdoc.AutoIDInt64))
c := db.Collection("users").WithAutoID(xdoc.AutoIDObjectID)  // 单个集合覆盖
```

`AutoIDInt32` / `AutoIDInt64` 的序列**只在内存里**，不落盘也不随事务回滚。
重启后从主键索引末尾重新推。代价是回滚过的号不退回，主键会有空洞——空洞不影响
任何东西。手写数字主键只会把序列往上抬，不会压低。序列到了类型上限（`int32` /
`int64` 的最大值）再要新号时报错，不会回绕成负数。

---

### 按条件批改与批删

不必自己读出来再写回去：

```go
// 只改写到的字段，没写到的原样留着
n, err := users.UpdateMany(ctx, "{ age: $.age + 1 }", "$.city = 'BJ'")

n, err = users.DeleteMany(ctx, "$.stale = true")
n, err = users.DeleteAll(ctx)          // 清空，索引定义留着
```

两个都是整批一个事务：任何一篇失败，一篇也不生效。主键改不得——转换表达式给出
一个不同的 `_id` 会让整批失败，而不是把文档悄悄搬到另一个主键下面。

内存与匹配到的篇数无关：按 `_id` 递增分批推进，一次改一千万篇也不会把这一千万篇
同时摆在内存里。

极值走索引的一端，不扫全表：

```go
oldest, err := users.Max(ctx, "$.age")
first, err := users.Min(ctx, "")        // 空串取主键
```

### 过期自动删除

会话、缓存、令牌这类到点就该消失的文档，交给后台清理：

```go
// 每次启动调一次，和 EnsureIndex 一样
err := sessions.EnsureTTL(ctx, "$.expireAt", time.Minute)
```

它先在表达式上建一条普通索引，再每隔一个周期沿索引删掉 `expireAt` 已经不晚于现在的
文档。值不是日期的（字符串、数字、缺失）永远不删。

- **分批删**：每批最多一千篇、一批一个事务，批与批之间放开集合锁。一次过期几十万篇
  也不会像 `DeleteMany` 那样整段占着集合，别的写入最多等一批。
- **注册只在内存里**，文件格式不变。同一集合同一表达式重复调什么也不做；换表达式报错。
- `Close` 停下并等后台清理退出；只读库上调用报错。
- 后台出错（比如等锁超时）没有地方报，本轮放弃、下个周期再试。
- 共享模式下清理也要取跨进程锁；而事务会把锁一直攥着（见[共享模式](#共享模式跨进程)），
  所以一旦真删过东西，这个进程就会占住库直到关闭。

## 结构体映射

```go
type User struct {
    ID    int64     `bson:"_id"`
    Name  string    `bson:"name"`
    Level int32     `bson:"level,omitempty"`   // 结构体字段的宽度照写
    Tags  []string  `bson:"tags,omitempty"`
    Meta  Meta      `bson:"meta"`
    Born  time.Time `bson:"born"`
    cache int       // 非导出字段不参与映射
}

users := db.Typed[User]("users")

id, err := users.InsertOne(ctx, User{Name: "阿伊", Level: 12})
u, err := users.FindByID(ctx, id)
all, err := users.Slice(ctx, xdoc.Asc)

for u, err := range users.All(ctx, xdoc.Asc) { … }
```

### 手动在结构体与文档之间转

`TypedCollection` 覆盖不到的地方——事务、分组投影、`Explain`——交出来的都是
`*Document`。这四个方法把它转回结构体：

```go
u, err := db.Decode[User](doc)          // 文档 -> 结构体，类型从实参来
err     = db.Unmarshal(doc, &u)         // 同上，类型从 out 来
d, err  := db.MarshalDocument(u)        // 结构体 -> 文档
v, err  := db.Marshal(42)               // Go 值 -> 字段值（Val 的可报错版本）
```

**它们挂在 `db` 上而不是包级函数，这就是它们存在的全部理由。** 它们用的是这个库
实际在用的那个转换器（`WithMapper` 换过的话就是换过的那个），而包级的 `xdoc.Doc` /
`xdoc.Val` 拿不到库，只能用默认那套。同一个值在这两条路上编出来的东西可以完全不同：

```go
// 前提：这个库的映射器上注册过 time.Duration -> 毫秒，见下一节
xdoc.Doc("timeout", 2500*time.Millisecond)   // 默认那套：Int64(2500000000)，纳秒
db.Marshal(2500 * time.Millisecond)          // 本库那套：Int64(2500)，毫秒
```

拿默认那套编出来的值，去查一个按本库规则写进去的字段，查询照常返回，只是零行
——不报错，看起来和"本来就没有匹配的记录"一模一样。

`db.Marshal` 与 `xdoc.Val` 还差一件事：`Val` 转不动时**静默返回 `Null`**，
`Marshal` 把错误交出来。`Val` 是包级构造器，没有地方放这个错误。

```go
xdoc.Val(make(chan int))     // null
db.Marshal(make(chan int))   // err: xmap: unsupported type: chan has no document representation
```

还有一类会被拒绝的：**字段全是非导出的结构体**，比如 `netip.Addr`、`big.Int`。
按结构体展开它们只会得到一篇空文档，而两头都不报错——写进去是 `{}`，读回来是个
零值（`invalid IP`、`0`），全程一处信号也没有，等发现时库里已经攒了一批取不回来
的记录。这一类现在直接报错：

```go
type Host struct{ Addr netip.Addr `bson:"addr"` }
db.MarshalDocument(Host{...})
// err: xmap: unsupported type: every field of netip.Addr is unexported, so
//      expanding it as a struct yields an empty document; register a converter
//      pair for it with Mapper.RegisterType (at addr, type netip.Addr)
```

映射的**键**也有一档规矩：文档里字段名只能是字符串，所以键要能在字符串与原类型
之间来回换算。放开的是三类——字符串族、整数族（十进制）、以及实现了
`encoding.TextMarshaler` 与 `TextUnmarshaler` 的类型（`time.Time` 就在这一档）。
浮点数与接口不放开：前者的文本形式不唯一（`1` 与 `1.0`），后者根本不知道该解回
什么，放开它们的代价是两个不同的键在文档里撞成一个而没有任何提示。
字段顺序按**换算出来的名字**排，所以整数键在文件里是 `"-1","10","9"` 这个顺序。

逃生口只有一个，错误文案里也写着：给这个类型注册一对转换函数，注册要赶在第一次
转换之前。这套映射规则里没有「让类型自己声明怎么序列化」的接口，要接管一个类型
只有这一条路：

```go
m := xdoc.NewMapper()
m.RegisterType[netip.Addr](
    func(a netip.Addr) (*xdoc.Value, error) { return xdoc.String(a.String()), nil },
    func(v *xdoc.Value) (netip.Addr, error) { s, _ := v.AsString(); return netip.ParseAddr(s) },
)
db, _ := xdoc.Open(path, xdoc.WithMapper(m))
```

字段被 `bson:"-"` 全标掉而产出空文档不在此列——那是明说了要这样。

这一组最主要的去处是事务——那里没有类型化的集合句柄，按结构体读写只剩这条路，
连同它自带的一个坑，见[事务里按结构体读写](#事务里按结构体读写)。

### 换一个映射器

`WithMapper` 换掉的是上面那一整条路：`Typed[T]` 的读写、刚才那四个方法、以及查询里
的 [`Param`](#参数)，全都改走新的那个。

```go
m := xdoc.NewMapper()
m.Naming = snake      // func(string) string；不设即照 Go 字段名原样写
m.MaxDepth = 20       // 嵌套深度上限，0 用默认 20
m.EmitNull = false    // 空值字段是否写进文档
m.TrimStrings = false // 关掉「写出前把字符串首尾的空白去掉」
db, err := xdoc.Open(path, xdoc.WithMapper(m))
```

`TrimStrings` 与 `EmptyStringToNull` 在 `NewMapper()` 建出来的映射器上**默认开着**，
这是这套映射规则约定的默认档。它们会让"存进去再读出来"得到不同的值——`" a "` 存成
`"a"`、`""` 存成空值——不想要就像上面那样显式关掉。

**`&xdoc.Mapper{}` 与 `xdoc.NewMapper()` 不等价**：零值那份两个开关都是关的，也没有
预置的 `url.URL` / `time.Duration` / `regexp.Regexp` 三条转换规则。要那套约定的默认
就走 `NewMapper()`。

还有四个钩子，都要在映射器投入使用之前设好（字段计划按类型缓存，事后改不生效）：

```go
// 按规律批量调整字段：加前缀、按后缀不持久化、按外部配置定名
m.ResolveField = func(owner reflect.Type, sf reflect.StructField, f *xdoc.FieldSpec) {
    if strings.HasSuffix(sf.Name, "Cache") { f.Name = "" }   // 置空 = 移出文档
}
// ref 没写集合名时怎么从类型推
m.CollectionNaming = func(t reflect.Type) string { return strings.ToLower(t.Name()) + "s" }
// 把老形态的数据在解之前就地改成新形态（每一层每一个值都经过它，要快）
m.BeforeDecode = func(target reflect.Type, v *xdoc.Value) *xdoc.Value { … }
// 同一层里两个字段抢同一个文档名时报错，而不是默认的「先声明的赢、后来的丢掉」
m.StrictDuplicateFields = true
```

### 多态字段（`_type`）

字段声明成接口时，写进文档的是**某一个**实现的形状，而文档本身记不住那是哪一个。
登记一下，读写就都成立：

```go
m.RegisterSubtype[Circle]("Circle, MyApp")
m.RegisterSubtype[Square]("Square, MyApp")

type Holder struct {
    ID    int64   `bson:"_id"`
    Shape Shape   `bson:"shape"`     // 接口
    Many  []Shape `bson:"many"`
    Any   any     `bson:"any"`
}
// 写出：{"_id":1,"shape":{"_type":"Circle, MyApp","R":2.0}, …}
```

- 判别符字段叫 `_type`，**排在文档最前面**——名字与位置都是磁盘格式的一部分，
  所以与同格式的库互认；判别符的值自己定，互读时写成对方认得的形式。
- **只出现在声明为接口的位置上**。声明成具体类型的字段不带；`any` 里装的是字典、
  数组或标量时也不带——它们的形状本来就自带类型信息，结构体不带。
- **读不出来就报错**，不退回接口的形状：退回去得到的是一个"看着对、内容缺一半"
  的值，而那半截缺失要到很久以后才被人看见。
- 方法挂在指针上（`func (p *Circle) Area()`）时，读回来的是 `*Circle`。

自己不拥有的类型（标准库、第三方包）用 `RegisterType[T]` 补一对转换函数。它也能
盖掉 `NewMapper()` 预置的那三条——下面这个 `time.Duration` 就是一例，预置的那条按
这个格式的时间粒度存 100 纳秒计数：

```go
m.RegisterType[time.Duration](
    func(d time.Duration) (*xdoc.Value, error) {
        return xdoc.Int64(d.Milliseconds()), nil
    },
    func(v *xdoc.Value) (time.Duration, error) {
        n, ok := v.AsInt64()
        if !ok {
            return 0, fmt.Errorf("timeout 不是整数")
        }
        return time.Duration(n) * time.Millisecond, nil
    },
)
```

**注册必须在这个映射器投入使用之前全部做完**，之后就当它只读。注册表是一张普通的
映射，每一次转换都要读它，而注册是在写它——一边转一边注册就是一场数据竞争，
它不一定当场崩，更可能是某次转换读到半张表，安静地按默认规则编了出去。

刻意不预置任何标准库类型的规则（`time.Time` 是文档模型自带的类型，不在此列）。
每一条内置注册都是一条使用者没有写、却会改变数据形态的规则；显式注册让文档里
出现的每一种形态，都能在调用方自己的代码里找到出处。

### 标签语法

`bson:"名字,选项1,选项2=值"`

| 名字部分 | 含义 |
|---|---|
| `-` | 忽略该字段 |
| `-,` | 字段名就是一个减号 |
| 空 / 无标签 | 由命名策略从 Go 字段名推出 |
| `age` | 固定为 `age` |

| 选项 | 含义 |
|---|---|
| `omitempty` | 零长度、nil、0、false、空串时不写出 |
| `omitzero` | 值等于类型零值时不写出（也认结构体，如零值 `time.Time`） |
| `id` | 主键，名字强制成 `_id` |
| `noauto` | 与 `id` 同用时关掉自增 |
| `ref=集合名` / `ref` | 引用，只写 `{$id, $ref}` 壳 |
| `inline` | 把该具名字段的结构体成员摊平到当前层 |
| `vector` | `[]float32` 按向量类型写出 |

选项顺序无关，重复按最后一次算，**写错的选项名会报错**——静默忽略会让"标签写了
但没生效"这种问题拖到数据已经写坏才被发现。

### 主键

按两级规则找，命中即止：带 `id` 选项或名字写成 `_id` 的字段，其次是 Go 字段名
（忽略大小写）为 `ID` 的字段。

**自增主键取零值时整个不写出**，交给库生成——零值的意思正是"我没指定"。
要自己管主键就加 `noauto`，那时 0 是一个正当取值。

自动生成的主键**按指针传时会写回**传进来的对象（`db.Typed[*User]`），按值传则不会
——Go 里改不到调用方手上那份副本。两条路都不因为"这个类型没有主键字段"而失败：
那时插入已经提交了，报错会让调用方以为没写进去而重试一遍，多出一条记录。
不想改成指针又要拿主键，用 `InsertOne`，它返回主键。

### 匿名嵌入

按 Go 自己的提升规则展开，浅的一层遮住深的一层。展开后仍有两个字段抢同一个
文档名字时，**先声明的赢、后来的被静默丢掉**——那是数据丢失，而且丢的是哪个还
取决于字段顺序，但这条规则的既定行为就是如此。要它改报错，把
`Mapper.StrictDuplicateFields` 打开。

---

## 索引

```go
_, err := c.EnsureIndex(ctx, "level", "$.level", false)
_, err = c.EnsureIndex(ctx, "name", "$.name", true)    // 唯一
_, err = c.EnsureIndex(ctx, "tags", "$.tags[*]", false) // 多键：一篇文档多个键

_, err = c.EnsureIndexOn(ctx, "$.name", false)          // 名字从表达式推：name

_, err = c.DropIndex(ctx, "level")
```

- **可以重复调**。已存在且表达式相同就什么也不做，所以"每次启动都保证索引在"
  是正常写法。
- **已存在但表达式不同会报错**。那是两条不同的索引共用一个名字；静默改掉它会让
  此前按旧表达式写进去的键全部失效，而查询照常返回，只是结果不全。
- **建在已有数据上会自动回填**。
- **主键索引恒存在**，建集合时就有，不能删。
- **取键表达式必须能索引**：至少引用一处文档字段、用到的方法都不易变
  （`NOW()` / `NOW_UTC()` / `TODAY()` / `GUID()` / `OBJECTID()` / `RANDOM()` 不行）、
  不带参数。判据落在整棵树上，所以 `CONCAT($.a, GUID())` 也不行。不满足会报错，
  因为这三类表达式建出来的索引查不到自己写进去的记录，而且删不掉——删除要先按
  键查到它。想先问一句再决定，先 `e, err := xdoc.Compile(expr)` 再看 `e.Indexable()`
  ——`Compile` 是两个返回值，接不成一串。
- **会摊开成多个键的表达式上不能加唯一约束**（`$.tags[*]` 加 `unique` 会报错）。
  一篇文档在这条索引里留下的是一串键，"唯一"是指这串键整体唯一还是串里每个键
  唯一，说不清楚；含糊地选一种等于让约束在某些数据上悄悄不成立。
- **多键索引里，一篇文档的重复键只留一个**，判重按「**类型相同且值相同**」。
  判重按 `(类型, 原始值)` 分桶，几条后果值得先知道：

  | 一篇文档的 `$.arr[*]` | 挂几个索引节点 |
  | --- | --- |
  | `[1, 1, 2]` | 2 |
  | `[5, 5.0]` | **2**——Int32 与 Double 是两个类型 |
  | `[5, {$numberLong:'5'}]` | **2**——Int32 与 Int64 |
  | `[0.0, -0.0]` | 1——同为 Double，值相等 |
  | `['a', 'A']` | 2，**即使库的比较规则忽略大小写**（判重按序数比） |
  | `[[1,2], [1,2]]` / `[{x:1},{x:1}]` / 两段相同的二进制 | **2**——这三种按引用判重，内容一样也各占一个键 |
  | 两个相同的 GUID / ObjectId / 日期 | 1 |

  节点数是落盘的一部分：同一篇文档建出的节点数不一样，同一个库就不是同一个库了。
- **`EnsureIndexOn` 的名字推法**：归一之后删掉所有非 ASCII 字母数字。
  `$.name` → `name`，`UPPER($.n)` → `UPPERn`，`$.a[*].b` → `MAPab`
  （它归一成 `MAP($.a[*]=>@.b)`）。推法是格式的一部分，所以同一个库上推出来
  的是同一个名字。推不出名字（比如 `$.名字`，一个 ASCII 字母数字都不剩）时报错。

索引是跳表。节点层数随机决定，键按[数据模型](#数据模型)里那个跨类型全序排列。

掷硬币用的是一条**进程级共享、定种子**的随机流，所以「从空
库开始的同一串操作」在一个干净的进程里逐字节可复现，但换一个先做过别的写入的进程
就不是了——层数序列被前面那些操作推着走了。

这件事会**露到查询结果里**一处：非唯一索引上的等值查找（`INDEX SEEK`），命中多行
时这几行的**先后不定**。定位停在等值段的哪一个节点取决于各节点掷到了几层，而执行
是「先交出停下的那个，再沿链往后展开，再往前展开」。要一个确定的顺序就写
`ORDER BY`——但注意排序键若正好就是这条索引的取键表达式，`ORDER BY` 会被
[优化掉](#看它为什么慢)，于是又回到不定。

### 向量索引

按向量相似度检索，用来做嵌入向量的近邻查找。

```go
// 768 维，按余弦距离比
_, err := c.EnsureVectorIndex(ctx, "emb", "$.embedding", 768, xdoc.Cosine)

// 最近的 10 篇
docs, err := c.TopKNear(ctx, "emb", query, 10)

// 距离不超过 0.3 的全部
docs, err = c.FindNear(ctx, "emb", query, 0.3)
```

三种度量：

| 度量 | 含义 | 方向 |
|---|---|---|
| `xdoc.Euclidean` | 欧氏距离 | 越小越近 |
| `xdoc.Cosine` | 余弦距离 `1 − cos(θ)`，只看方向不看长度 | 越小越近，同向 0、正交 1、反向 2 |
| `xdoc.DotProduct` | 点积 | **越大越近**，而且偏好模长大的向量 |

`FindNear` 的阈值随度量变含义：前两种是距离上限（含等于），点积是相似度下限。
点积不是距离——查一个向量自己，回来的很可能是别人；要「最像的」用余弦。

几条要知道的：

- **维度建索引时定死**。取出来的向量长度对不上，那篇文档就不进索引，既不报错
  也不参与检索。字段缺失、元素不是数，同样只是不进索引。
- **检索是近似的，而且在高维大集合上会明显不准**。底下是一张分层邻近图（HNSW），
  每个节点每层最多记八个邻居，探索宽度是 `max(k*4, 32)`——两个数都写死在格式里，
  调不了。实测 128 维、两千条数据时 `recall@10` 掉到 0.0–0.6，最差的一次真正的
  前十条一条都没返回。驱动因素是**维度**不是条数：八个邻居的度数在高维撑不住连通性。
  低维（几维到几十维）或小集合上是准的。
- **同一批数据两次建索引，结果可能不同**。层数是随机采样的，图的形状因此不同，
  并列距离的先后也就不同。距离值本身是稳定的——要对结果做断言，比距离的集合，
  别比返回顺序。
- **维度超过 1996 时向量另存**到数据页，节点里只留一个地址。这个界是「向量还
  装不装得进一个 8 KiB 的页」算出来的。
- **两处容易踩空的地方在这里是补上的**（都不改落盘字节）：一是给节点找页时不能只看
  「剩余空间够不够 1400 字节」而不看这个节点实际要多少——那样 512、768、1024、1536
  这些常用维度插到第几条就会抛「页放不下」，这里装不下就另起一页；二是删除与清空
  不能从根广搜——模长为零的向量常常一条边都连不上、从根走不到，那样的节点既删不掉
  也回收不了，这里按页扫描，孤儿照样清得干净。
- **同一集合可以建多条向量索引**。节点上没记它属于哪条索引，按页扫到的可能是别的索引的，
  所以一个集合有多条向量索引时，先顺着各条索引的空闲页链与从根走得到的图认出节点和页的归属，
  只摘本索引的节点；清孤儿也只清落在本索引的页或无主页上的，别的索引的页不碰。删掉其中一条
  只拆它自己的图、回收它自己的页，其余索引原样留着。页的划分与挂链规则与只有一条时相同。
- 表达式里还有 `VECTOR_SIM(a, b)`，求两个向量的**余弦距离**，与索引上定的度量无关。

#### 查询里怎么用上这条索引

查询与 SQL 也能走向量索引，但**只有中缀写法**认得出来：

```sql
SELECT $ FROM c WHERE $.v VECTOR_SIM [1.0, 0.0] <= 0.5     -- 走向量索引
SELECT $ FROM c ORDER BY $.v VECTOR_SIM [1.0, 0.0] LIMIT 5 -- 走向量索引
SELECT $ FROM c WHERE VECTOR_SIM($.v, [1.0, 0.0]) <= 0.5   -- 不走，全表扫
```

函数写法 `VECTOR_SIM(a, b)` 语义完全一样，却永远选不中索引：函数调用形式的表达式
不填 `Left`/`Right`，而选索引第一步就是取 `Left`。这是格式层面的既定行为，不作修正。

它带来的**不是快慢的差别，是行数的差别**：走索引时行数被搜索宽度
`max(LIMIT*4, 32)` 截断。200 篇的库上，134 篇满足 `<= 0.5`：

| 写法 | 行数 |
|---|---|
| `WHERE $.v VECTOR_SIM [1,0] <= 0.5` | **32** |
| `WHERE VECTOR_SIM($.v,[1,0]) <= 0.5` | 134 |
| `ORDER BY $.v VECTOR_SIM [1,0]`（无 LIMIT） | **32** |
| `... LIMIT 5` / `LIMIT 20` / `LIMIT 100` | 5 / 20 / 100 |

还有几条同源的既定行为：

- **`ORDER BY` 的方向被丢掉**：`ORDER BY $.v VECTOR_SIM [...] DESC` 与不写 DESC
  出的是同一串，都按距离升序。相似度表达式一旦用来选中索引，整段排序就被吃掉。
- **`OFFSET` 在截断之后才生效**：`LIMIT 5 OFFSET 3` 只出 **2** 行——向量搜索按
  limit=5 返回 5 条，OFFSET 再跳掉 3 条。
- **被选中的那条谓词不再复算**：`$.V`（大小写写错）照样选中建在 `$.v` 上的索引
  （索引表达式按不分大小写比），谓词被吃掉，于是返回的正是索引搜出来的那一批。
- **选不中就退回全表扫，谓词留着逐条判**：维度对不上、目标不是数组/向量、
  目标数组里有 `null`、阈值不是数字——这几种都不选索引。而 `VECTOR_SIM` 在这些
  情形下常常给 `Null`，`null <= 0.5` 是**真**，于是查询返回**全部**文档。

`EXPLAIN` 里这条计划印成 `VECTOR INDEX SEARCH`，代价恒为 1。

---

## 查询

```go
docs, err := c.Query().
    Where("$.level > @min").Param("min", 10).
    Where("$.active = true").          // 多个 Where 之间是「并且」
    OrderByDesc("$.level").ThenBy("$.name").
    Skip(20).Limit(10).
    Slice(ctx)

one, err := c.Query().Where("$.name = 'aiy'").First(ctx)   // 没有则 ErrNotFound
n, err := c.Query().Where("$.level > 10").Count(ctx)
ok, err := c.Query().Where("$.banned = true").Exists(ctx)

for d, err := range c.Query().Where(…).All(ctx) { … }
```

### 构建器是不可变的

每一步返回一份**新的**，从同一个起点分出的两支互不影响：

```go
base := c.Query().Where("$.active = true")
a, _ := base.Limit(10).Slice(ctx)     // 不会把 b 的上限也改掉
b, _ := base.Limit(20).Slice(ctx)
```

就地改再返回自己会让复用变成陷阱，而且两边都不报错。

### 参数

用 `Param` 绑定值，**不要把值拼进表达式串**——拼串要自己处理引号与转义，
漏一处就是一条能被输入内容改变含义的查询。参数走的是值这条路，不经过解析。

参数值用这个库自己的映射器编码（[`WithMapper`](#换一个映射器) 换过的那个），
不是默认那套。两边不一致的后果是最难查的那种：文档按你的规则写进去、参数按默认
规则编出来，命名策略、空串的处理、注册过的自定义类型只要有一处对不上，这条查询就
什么都查不到——不报错，只是没有结果，和"本来就没有匹配的记录"分不开。

**转不出来的值会报错，不再静默变成 `Null`。** 一个静默变成 `Null` 的参数照样参与
比较，给出的是一份不对的结果。错误记在构建器上，留到执行时交出来——`Slice`、
`First`、`Count`、`Exists`、`All`、`Explain` 每一条都报，`Count` 不会返回一个数，
`Exists` 也不会返回 `(false, nil)`。文案里带着参数名：一条查询上绑五六个参数是常事，
只说"有个值转不了"等于让人挨个注释掉再试一遍。

```go
_, err := c.Query().Where("$.a = @x").Param("x", make(chan int)).Count(ctx)
// xdoc: query parameter "x": xmap: unsupported type: chan has no document representation (type chan int)
```

已经是 `*xdoc.Value` 的参数直接用，不再走一遍编码——代价是它绕过了上面这条规则。
`xdoc.Val(2500*time.Millisecond)` 构造出来的值是按**默认**那套编的，拿它去查一个按
本库规则写进去的字段，同样是安静的零行。要让参数跟着库走，就把 Go 值原样交给
`Param`，别自己先转一道。

### 表达式

路径从 `$` 开始：

| 写法 | 产出 |
|---|---|
| `$.name`、`$.meta.k` | 一个值 |
| `$.tags[0]`、`$.tags[-1]` | 一个值（越界给 `Null`） |
| `$.tags[*]` | **序列**（零到多个值） |
| `$.items[@.n > 3]` | **序列**（按条件筛出的元素） |

产出序列的路径不能直接做标量投影，要配量词或聚合方法使用：

```go
c.Query().Where("$.tags[*] ANY = 'a'")        // 有任意一个标签是 a
c.Query().Where("$.tags[*] ALL != 'ban'")     // 所有标签都不是 ban
c.EnsureIndex(ctx, "tags", "$.tags[*]", false) // 多键索引：一篇文档多个键
```

运算符：`= != > >= < <= BETWEEN IN LIKE AND OR NOT`，以及量词 `ANY` / `ALL`。
`LIKE` 用 `%` 与 `_` 通配。`Where` 收的必须是**谓词**（求值为布尔），
一个裸路径不是谓词，会被拒绝。

内置方法 111 个重载，覆盖字符串、数值、日期、类型判断与转换、数组与文档操作、聚合。

#### 单独求一条表达式

不挂在查询上也能求值。用来校验用户填进配置里的取键表达式，或者只是想知道
某条表达式在一篇文档上算出什么：

```go
v, err := xdoc.Eval("UPPER($.name)", doc)          // "ANN"
v, err = xdoc.Eval("1 + 2", nil)                   // 3，与文档无关就不用给文档
v, err = xdoc.Eval("$.n > @min", doc, map[string]any{"min": 3})

e, err := xdoc.Compile("$.tags[*]")   // 编译一次、对每篇文档求多次
for v, err := range e.Values(doc) { … }            // 摊开成多个值
e.Source()                                          // 归一后的源串
e.Indexable()                                       // 能不能拿来建索引
```

`Value` 只收一个值，遇到会摊开的表达式（`$.tags[*]`）报错；要那些值用 `Values`。
`db.Compile` / `db.Eval` 与包级那两个的分别只在**用库的字符串比较规则**——
`"ABC" = "abc"` 在忽略大小写的库上为真，按码元的库上为假。

#### 数字与日期的字符串解析看区域

`DOUBLE`、`DECIMAL`、`DATETIME` 这几个从字符串转过来的方法，解析时按**区域**的
写法来：小数点是点还是逗号、千位分隔符是什么、货币符号长什么样，都随区域变。

```go
xdoc.Eval(`DOUBLE("1.5", "de-DE")`, nil)   // 15  —— 德语里点号是千位分隔符
xdoc.Eval(`DOUBLE("1,5", "de-DE")`, nil)   // 1.5
xdoc.Eval(`DOUBLE("1 234,5", "fr-FR")`, nil) // 1234.5 —— 法语的千位分隔符是窄空格
xdoc.Eval(`DOUBLE("$1.5", "en-US")`, nil)  // 1.5
```

不带区域名的那一档（`DOUBLE("1,5")`）用的是**这个库的排序规则所在的区域**：
排序规则是「区域号 + 选项」，区域号顺带决定了字符串怎么解析成数字。新建库的默认
规则是按码元比较（区域号 127，不依赖区域），所以默认行为是不依赖区域的那一套；
用 `WithCollation("de-DE/IgnoreCase")` 建的库上，`DOUBLE("1,5")` 是 1.5。
这两件事捆在一起并不讲道理，但格式就是这么定的：数字的解析规则跟着排序规则的
区域号走。

区域数据是一份**内嵌的快照**（870 个区域，取自 ICU 76），不去问运行的机器。
去问机器就等于让同一条语句在两台机器上解析出不同的值，再写进索引键——那是
[限制](#限制)里说的毁库那一类。

几处容易撞上的边角：

| 写法 | 结果 | 为什么 |
|---|---|---|
| `DOUBLE("Infinity")`、`DOUBLE("1e999")` | `+Inf` | 越界给无穷而不是失败；JSON 里印成 `null` |
| `DOUBLE("NaN")`、`DOUBLE("-NaN")` | `NaN` | 特殊值符号前允许带一个正负号 |
| `DOUBLE("∞", "abc")` | `+Inf` | 认不出的区域名拿到的是国际化库的**根**数据，无穷符号是 `∞` |
| `DOUBLE("∞")` | `null` | 不依赖区域的那一档，无穷符号是 `Infinity` |
| `DOUBLE("( 1.5)")` | `null` | 符号（含左括号）之后不许再有空白 |
| `DOUBLE("($ 1.5)", "en-US")` | `-1.5` | 除非空白之前已经吃到过货币符号。这一行**必须带区域**：不依赖区域的那一档货币符号是 `¤` 不是 `$`，`DOUBLE("($ 1.5)")` 给 `null` |
| `DOUBLE(".5", "de-DE")` | `null` | 千位分隔符前面必须已经有数字 |
| `DOUBLE("1.5", "en US")` | **报错** | 区域名写法不过关时整条表达式失败，不是给 `null` |

`DATETIME` 底下没有「格式列表」这种东西：它是一台手写的词法分析加状态机，认的
写法比任何一张格式表都宽，而且认哪些跟区域走。

```go
xdoc.Eval(`DATETIME("15.01.2020", "de-DE")`, nil)  // 2020-01-15
xdoc.Eval(`DATETIME("1/15/2020", "de-DE")`, nil)   // null —— 德语的短日期是 dd.MM.yyyy
xdoc.Eval(`DATETIME("2020年1月15日", "ja-JP")`, nil) // 2020-01-15
```

| 写法 | 结果 | 为什么 |
|---|---|---|
| `DATETIME("Thu, 15 Jan 2020")` | `null` | 写了星期就得对上，那天是星期三 |
| `DATETIME("1 2020 15")` | 2020-01-15 | 三位以上的数字不管排在哪都是年份，余下两个按「月 日」读 |
| `DATETIME("15 1 2020")` | `null` | 同上，15 不是月份 |
| `DATETIME("2020-01-15T1:30")` | `null` | 带 `T` 的走另一个认 ISO 8601 的子解析器，时分秒**正好两位** |
| `DATETIME("2020-01-15T10:30 PM")` | `null` | `T` 之后不许再跟上下午标记或时区词 |
| `DATETIME("... GMT")` | 世界时 | 认 `GMT`，从不认 `UTC` |
| `DATETIME("15.01.2020 10.30", "fi-FI")` | 2020-01-15 10:30 | 芬兰语的日期与时间分隔符都是 `.`，角色**按空格分段**各自定 |
| `YEAR(DATETIME("2020-01-15", "th-TH"))` | 1477 | 泰语区用佛历 |
| `YEAR(DATETIME("1399-01-15", "fa-IR"))` | 2020 | 波斯语区用波斯历，元旦落在公历三月二十前后，折出来是 2020-04-03 |
| `YEAR(DATETIME("2020-01-15T10:30:00", "th-TH"))` | 2020 | 带 `T` 的一律按公历读，绕过日历折算 |
| `SECOND(DATETIME("0001-01-01T00:00:00Z"))` | 0 | 时区偏移永远是整分钟；上海 1900 年前的地方平时是 +8:05:43，那几秒被抹掉 |

月份名跟一个数字怎么搭，也全看区域，而且**两张短模式都要看**：数在月份名**后**时，
`MonthDayPattern` 是「日 月」**且** `YearMonthPattern` 是「月 年」才当年读；数在
**前**时，`MonthDayPattern` 是「月 日」**且** `YearMonthPattern` 是「年 月」才当年读。
两个条件缺一不可，凑不齐就当日读：日是比两位年更好的默认。
所以同一串 `Jan 5` 在英语区是一月五日、在德语区是二〇〇五年一月，而在波斯语区
（月日是「日 月」、年月是「年 月」，第二个条件不成立）又回到当日读。

| 写法 | 结果 | 为什么 |
|---|---|---|
| `DATETIME("Jan 5")` | 今年 1 月 5 日 | 英语区的 `MonthDayPattern` 是 `MMMM d`，数在后按日读 |
| `DATETIME("Jan 5", "de-DE")` | 2005 年 1 月 1 日 | 德语区是 `d. MMMM`，数在后改按年读 |
| `DATETIME("5 Jan", "de-DE")` | 今年 1 月 5 日 | 数在前要月日模式是「月 日」才当年读，德语是「日 月」，于是回到当日读 |
| `DATETIME("Jan 5 15")` | 2015-01-05 | 两个数按短日期模式去掉月份剩下两位的次序填 |
| `DATETIME("Jan 99 15")` | 1999-01-15 | 99 当不了日，换个个儿读成「年 日」 |
| `DATETIME("1月 2020", "ja-JP")` | `null` | 日语的 `1月` 不是月份名，是「数字 + 记号」 |
| `DATETIME("一月 2020", "zh-CN")` | 2020-01-01 | 同一区域，全称不以数字打头就认——差别只在名字本身 |
| `DATETIME("1-р сар 2020", "mn")` | 2020-01-01 | 蒙古语有 `UseDigitPrefixInTokens`，数字打头是正经写法 |
| `DATETIME("1月 5 15", "ja-JP")` | 今年 1 月 1 日 05:15 | 记号已自成日期，剩下两个裸数字成了时与分 |
| `HOUR(DATETIME("10:30 PM", "th-TH"))` | 22 | 不变区域的 `AM`/`PM` 在每个区域下都认（`P.M.`、`P` 不认） |
| `DATETIME("Jan 15 2020", "de-DE")` | 2020-01-15 | 英文的月份名与星期名也在**每个**区域下都认 |
| `DATETIME("15 de enero de 2020", "es-ES")` | 2020-01-15 | 模式里带引号的「日期词」（这里的 `de`）读到就丢掉 |
| `DATETIME("15. 1. 2020.", "bs")` | 2020-01-15 | 波斯尼亚语的短日期末尾还有个点，整个日期分隔符因此降格成可忽略记号 |
| `DATETIME("10 h 30 min 45 s", "fr-CA")` | 今天 10:30:45 | 这六个时分秒后缀只有 `fr-CA` 一个区域有 |
| `DATETIME("january 15 2020", "en-US-POSIX")` | `null` | 八百七十个区域里就这一个比对记号**分大小写** |

非公历的那十四个区域（十二个波斯历、两个佛历）的年份要按各自的日历折算，两套都
实现了：佛历是一个固定偏移，波斯历的元旦逐年不同、闰年也没有闭式公式，那 2270 个
闰年是逐年算出来存成的一张位图。两位年补世纪的界线也跟着
日历走——波斯历是 1410、佛历是 2572，跟公历那套（2049）没关系。

再往下还有一层：整个解析是一台写死的状态机，记号连着它后面的分隔符归成一类、
按一张表推进，走到终结状态才把攒下的数字兑现，而数字缓冲区**只有三格**。这台机器的
形状直接决定了一批讲不出道理的结果，它们跟「日期格式」没有关系：

| 写法 | 结果 | 为什么 |
|---|---|---|
| `DATETIME("Jan 15 10:30")` | 今天 15:10 | 月份名加一个散数撞上时间分隔符，日期整个作废，15 成了点钟、10 成了分钟，30 没人要 |
| `DATETIME("1/15 10:30")` | `null` | 1、15、10、30 四个数字，缓冲区装不下 |
| `DATETIME("1/15 PM")` | `null` | 这个状态上遇到上下午标记是错 |
| `DATETIME("1/15/2020 PM")` | 2020-01-15 12:00 | 换个状态就好了 |
| `DATETIME("1/15 10 PM")` | 今年 1 月 15 日 22:00 | 这个位置上有一条专门的分支，先把日期兑现再收 10 当点钟 |
| `DATETIME("15 AM")` | `null` | 上午只认到 12 点；下午认到 23 点，`13 PM` 就是十三点 |
| `DATETIME("15 Am Faoilleach", "gd")` | `null` | 数字后面那个位置找的是分隔符，`Am` 被当成上午记号，剩下的不认得 |
| `DATETIME("2020年1月15日水曜日", "ja-JP")` | `null` | 记号也要整词，末尾那个 `日` 后面顶着星期名 |

### 分组

```go
docs, err := c.Query().
    GroupBy("$.level").
    Select("{ level: @key, n: COUNT(*) }").
    Having("COUNT(*) > 5").
    Slice(ctx)
```

### 引用展开

```go
type Order struct {
    ID   int64 `bson:"_id"`
    User *User `bson:"user,ref=users"`
}
docs, err := c.Query().Include("$.user").Slice(ctx)
```

挂在句柄上就管这个句柄之后的所有读，`FindByID` 与 `All` 也算在内：

```go
c := db.Collection("orders").Include("$.user")
o, err := c.FindByID(ctx, xdoc.Val(10))   // 读出来的 user 已经是被引的那篇文档
for d, err := range c.All(ctx, xdoc.Asc) { … }
```

原句柄不受影响——`Include` 返回的是新句柄。事务内的句柄（`tx.Collection`）与
类型化句柄（`db.Typed[T]`）也有同名方法。

### 看它为什么慢

```go
plan, err := c.Query().Where("$.level = @v").Param("v", 5).Explain(ctx)
```

执行计划会说清走了哪条索引、扫描区间多大、要不要额外排序。计划里显示全表扫描而
你以为它该走索引，多半是索引的取键表达式与查询里的写法对不上——**匹配是按表达式
源文本比的**，`$.age` 与 `$["age"]` 不互认。

计划文档的形状与键名都是格式的一部分，包括那个拼错的 `snaphost`：

```json
{
  "collection": "c",
  "snaphost": "read",
  "pipe": "queryPipe",
  "index": {"name": "idx_a", "expr": "$.a", "order": 1, "mode": "INDEX SEEK(idx_a = 1)", "cost": 10},
  "lookup": {"loader": "document", "fields": "$"},
  "filters": ["$.b=\"x\""],
  "select": {"expr": "$", "all": false}
}
```

| 键 | 意思 |
| --- | --- |
| `snaphost` | 快照模式，`read` 或 `write`（`ForUpdate` 的查询是后者）。拼法如此，不作订正 |
| `pipe` | `queryPipe` 或 `groupByPipe` |
| `index.mode` | 这条索引怎么扫：`INDEX SEEK(...)` / `INDEX SCAN(...)` / `INDEX RANGE SCAN(...)` / `FULL INDEX SCAN(...)` / `FULL COLLECTION SCAN` |
| `index.order` | 扫描方向，`1` 或 `-1`；外部源上是 `0`（那里没有方向可言） |
| `index.cost` | 选中算子的代价，越小越好 |
| `lookup.loader` | `document` 回表读数据页、`index` 只用索引键、`virtual` 外部源 |
| `lookup.fields` | 要反序列化的根字段；`"$"` 表示整篇 |
| `filters` | 索引吃不掉、要逐条判定的谓词 |
| `orderBy` | 索引没省掉的排序；被省掉时这个键不出现 |

`includeBefore` / `includeAfter` / `limit` / `offset` / `groupBy` 只在用到时出现。

**集合不存在时没有计划**：`EXPLAIN` 一行都不出，与那条查询本身一样——优化之前
就按「没有集合就没有文档」短路了。投影里用到 `*` 时出的那一行是**投影在空源上
的结果**（`{expr: []}`、`{expr: 0}`），不是计划。

`filters` 里的源串是**归一之后**的写法，不是你写的原文。量词的左操作数是路径时接
`[*]`，不是路径时包 `ITEMS(...)`——`$.arr ANY = 1` 印成 `$.arr[*] ANY=1`，
`UPPER($.a) ANY = 1` 印成 `ITEMS(UPPER($.a)) ANY=1`。归一规则是格式的一部分：同一个
串会落进数据文件（索引的取键表达式存的就是它），写出的字节必须一致。

### 惰性

`Limit` 是真惰性的：取够就停，底下不会扫完整个集合。实测 `LIMIT 1` 读 3 次底层，
同一个查询全扫要读 115 次。

遍历期间占着一个快照——中途 `break` 之后要让 `for` 循环正常退出，资源才会释放。
用 Go 的 range-over-func 时这是自动的。

### 类型化查询

```go
users := db.Typed[User]("users")
top, err := users.Query().Where("$.level > @v").Param("v", 10).Limit(5).Slice(ctx)
```

结果形状与 `T` 不同的查询（比如分组投影）用 `.Raw()` 拿回产出文档的构建器，
再用 `db.Decode[Other](doc)` 把产出的文档解成另一个结构体：

```go
type Stat struct {
    Level int32 `bson:"level"`
    N     int64 `bson:"n"`
}

rows, err := users.Query().Raw().
    GroupBy("$.level").
    Select("{ level: @key, n: COUNT(*) }").
    Slice(ctx)

for _, d := range rows {
    s, err := db.Decode[Stat](d)   // {Level:5 N:2}
    …
}
```

---

## SQL

`db.Execute` 收一条 SQL 语句，逐条产出结果。

```go
for v, err := range db.Execute(ctx, `SELECT $ FROM users WHERE $.age > 18 ORDER BY $.age DESC LIMIT 10`) {
    if err != nil {
        return err
    }
    doc, _ := v.AsDocument()
    fmt.Println(doc)
}
```

查询语句产出文档，改动语句产出一个数（改了几篇、删了几篇），建索引与删集合产出布尔。
产出的一律是一个值，而不是「查询专用的行」。

```go
db.Execute(ctx, `INSERT INTO users VALUES {_id: 1, name: "ann"}, {_id: 2, name: "bob"}`)
db.Execute(ctx, `UPDATE users SET age = $.age + 1 WHERE $.city = "BJ"`)
db.Execute(ctx, `DELETE users WHERE $.stale = true`)
db.Execute(ctx, `CREATE UNIQUE INDEX idx_name ON users($.name)`)
db.Execute(ctx, `DROP INDEX users.idx_name`)
db.Execute(ctx, `RENAME COLLECTION users TO people`)
db.Execute(ctx, `PRAGMA USER_VERSION = 3`)
```

`PRAGMA <名字>` 读一项库级设置，`PRAGMA <名字> = <值>` 写它并产出**改没改**
（相同就没改，产出 `false`）。六项设置见[库级设置（pragma）](#库级设置pragma)。

### 投影列表

`SELECT` 后面可以是一串用逗号隔开的表达式，每一个是结果文档的一列。列名用 `AS`
指定，`AS` 可以省：

```sql
SELECT $.name AS n, $.age AS a FROM users
SELECT $.name n, $.age a FROM users
```

不写列名时，列名取这个表达式引用到的**根字段名**——不是最后一段。多个根字段用
`_` 连起来，重复的只算一次，一个字段都没引用到时叫 `expr`：

| 表达式 | 列名 |
| --- | --- |
| `$.z.k` | `z` |
| `$.x + $.z.k` | `x_z` |
| `$.x + $.x` | `x` |
| `1`、`COUNT(*)` | `expr` |

列名撞了就从第二个起追加一个计数，`a`、`a1`、`a2`。计数是整条投影共用的一个，
自己指定的别名与推出来的默认名一视同仁。

产出一串值的表达式（`*`、`$.a[*]`）在一列里会被收成一个数组，所以 `SELECT * FROM c`
只出**一行**，值是整个结果集；一个都没查到时这一行照出，值是空数组：

```
SELECT * FROM c            => {"expr":[{...},{...}]}
SELECT * FROM c WHERE 假   => {"expr":[]}
SELECT $.nope[*] FROM c    => {"nope":[]}
```

只有一列、且没写别名时有三条捷径，结果不再包一层：`SELECT $`（整篇文档）、
`SELECT {a:1}`（文档字面量）、`SELECT EXTEND($, {...})`。

聚合投影（含 `*`、`COUNT(*)` 之类，或写了 `GROUP BY`）里没有「当前文档」这个概念，
在那里再取字段是错的，报的是这一句：

```
SELECT *, $.x FROM c
=> xbexpr: Field 'x' is invalid in the select list because it is not contained
   in either an aggregate function or the GROUP BY clause.
```

### 多个排序键

`ORDER BY` 收一串键，每个键各带自己的方向，没写就是升序——方向**不从前一个键继承**。
键可以是任意表达式：

```sql
SELECT $ FROM c ORDER BY $.a, $.b DESC
SELECT $ FROM c ORDER BY UPPER($.name), $.age ASC
```

### 不带 FROM 的 SELECT

`SELECT` 后面直接结束时不查任何集合，就地求一次表达式并产出一行。根文档是一篇空文档，
所以 `$` 出的是 `{}`。这条路上不产生执行计划，`EXPLAIN` 与不加一样；`WHERE`、
`ORDER BY`、`LIMIT` 这些子句一个都走不到，写了就报错。

```
SELECT 1            => {"expr":1}
SELECT 1 AS a, 2    => {"a":1,"expr":2}
SELECT $            => {}
```

### `SELECT ... INTO`

`INTO` 把结果写进另一个集合，产出写了几篇。目标集合名后面可以跟一个冒号加主键类型，
指定源文档**没带主键**时现发什么：

```sql
SELECT {q:$.x} INTO t:INT FROM c
```

类型名一共四个——`INT`、`LONG`、`GUID`、`OBJECTID`，大小写不敏感；写不认识的名字
（比如 `INT32`）或冒号后面空着都报错。冒号后面那截**只作用于这条语句**，不落到集合上：
下一次不带冒号写进去的仍然是 ObjectId。但序号是按集合记的，再写一次 `:INT` 会接着
上次的号往下发。源文档自己带主键时原样保留，不现发。

`INTO` 后面也可以是 `$file(...)`，见[把查询结果写成文件](#把查询结果写成文件)。

### 哪些地方收 JSON，哪些地方收表达式

SQL 里有四处收的是 **JSON**，不是表达式：

| 位置 | 例子 |
| --- | --- |
| `INSERT ... VALUES` | `INSERT INTO c VALUES {a:1}, {a:2}` |
| `PRAGMA <名> = <值>` | `PRAGMA USER_VERSION = 3` |
| `REBUILD <选项>` | `REBUILD {collation:'en-US/None'}` |
| 系统集合名后面的参数 | `SELECT $ FROM $page_list(2)` |

其余地方（`WHERE`、`SELECT` 的投影、`ORDER BY`、`UPDATE ... SET`、`INCLUDE`、
索引的取键表达式）收的都是表达式。两套语法收的东西不一样：

- **JSON 不认表达式**。`{v:1+1}`、`{v:UPPER('a')}`、`{v:$.x}`、`{v:@p}` 在那四处都是
  语法错误。**参数占位符也不认**——参数只在表达式里有意义。
- **JSON 认扩展记法**，而且载荷不必是字符串：`{$numberLong:5}` 是 Int64，
  `{$numberDecimal:5}` 是 Decimal，`{$minValue:<任意值>}` 是 MinValue。同样的写法
  在表达式里只有载荷是字符串时才转类型，否则就是一篇普通文档。
- 扩展记法必须是这篇文档的**第一个**键，而且后面要立刻收尾：
  `{a:1,$oid:'…'}` 是普通文档，`{$oid:'…',a:1}` 是语法错误。认不出的 `$` 键
  （`{$nope:'x'}`）就是个普通键。载荷不合法（`{$oid:1}`）报错，不会退化成普通文档。
- **JSON 容得下多余的逗号**：`{v:1,}` 合法。

`INSERT ... VALUES` 后面的值必须是文档，写数组或标量报错。多篇文档之间要有逗号。

`db.Execute` 的参数只作用在表达式上：`SELECT $ FROM t WHERE $._id = @id` 认，
而 `INSERT INTO t VALUES {_id: @id}`、`PRAGMA USER_VERSION = @v`、
`REBUILD {collation: @c}` 都是语法错误。

### `UPDATE ... SET` 的键

两种写法都收，含义相同——`SET a = 1, b = 2` 与 `SET { a: 1, b: 2 }` 都是**合并**：
写到的键覆盖，没写的原样留着。

等号左边可以是裸标识符、加引号的字符串、或一个整数：

```sql
UPDATE c SET a = 1          -- 字段 a
UPDATE c SET 'b' = 1        -- 字段 b
UPDATE c SET "p.q" = 1      -- 一个叫 "p.q" 的平键，不是嵌套的 p.q
UPDATE c SET 1 = 2          -- 字段名就是 "1"
```

引号里的点号建的是**平键**：`SET "p.q" = 1` 之后 `$.p` 是 `null`，`$.p.q` 查不到。
裸标识符里不能有点号，`SET n.o = 1` 与 `SET $.n = 1` 都报错。

### 名字里的非 ASCII 字符

集合名、字段名、索引名、列别名都按 Unicode 认字母，中文、日文、西里尔字母直接写就行：

```sql
INSERT INTO 用户 VALUES {_id:1, 名字:'张三'}
SELECT $.名字 AS 乙 FROM 用户
CREATE INDEX 索引 ON 用户($.名字)
```

首字符必须是字母或下划线（`1c` 不行，`_c` 行），中间不能有空格或连字符。没有加引号的
写法——`[名字]`、`"名字"` 都不认，所以名字里带空格的集合在 SQL 里写不出来。

### 事务里能做什么

`BEGIN` / `COMMIT` / `ROLLBACK` 是三次独立的 `Execute`，事务因此挂在库上，
同一个库上同时跑两个 SQL 事务会互相抢。要并发就用 `db.Transaction`，它每次开自己的。

```go
db.Execute(ctx, `BEGIN`)
db.Execute(ctx, `UPDATE users SET vip = true WHERE $.score > 100`)
db.Execute(ctx, `COMMIT`)
```

嵌套的 `BEGIN`、以及没有事务时的 `COMMIT` / `ROLLBACK` 都不报错，产出 `false`。

| 语句 | `BEGIN` 里 |
| --- | --- |
| `INSERT` / `UPDATE` / `DELETE` / `SELECT` | 进事务，回滚跟着回 |
| `SELECT ... INTO` | 进事务 |
| `CREATE INDEX` / `DROP INDEX` | 进事务，本事务里立刻可见，回滚后消失 |
| `DROP COLLECTION` / `RENAME COLLECTION` / `CHECKPOINT` | **拒**，报 `The current thread already contains an open transaction. Use the Commit/Rollback method to release the previous transaction.` |
| `PRAGMA` 写 | **拒**，同上；写一个同值的除外（本来就没改，产出 `false`） |
| `REBUILD` | 底下的核心被换掉，这个事务跟着没了，之后 `ROLLBACK` 产出 `false` |

后面那几条改的是整个库的形态，事务给不了它们隔离，所以一律拒。

### `REBUILD`

`REBUILD` 就地重建数据文件，产出回收了多少字节。它会把库关掉、搬运、再打开，
所以库句柄本身还能接着用，但底下的核心换了一个：

```sql
REBUILD                          -- 等同 REBUILD {}
REBUILD {}
REBUILD {collation:'en-US/None'} -- 顺便换比较规则
REBUILD {password:'新口令'}      -- 顺便换口令（用旧口令读、新口令写）
```

不带参数当作 `REBUILD {}`：一句合法的 SQL 不该把进程带走。
认不出的键静默忽略；参数不是文档（`REBUILD 1`、`REBUILD 'x'`）报错。
选项写错（比如 `{collation:'zzz'}`）在**关库之前**就报出来，库不受影响。
重建本身失败（比如磁盘满）时，按原来的选项把原文件重开回来，句柄照样能用，错误照常返回。
直连模式下它先等同一句柄上在途的操作做完，重建期间新来的操作排队等它，等不到就在库的超时
到点时报错、库原样不动。遍历只在取下一行时算在途，行交到循环体里就不算——所以在循环体里
执行它不会等自己，但这次遍历的下一步会报错。

内存库没有文件可重建，直接返回零值。库级的同一件事是 `db.Rebuild(...)`，关着的文件
用包级的 `xdoc.Rebuild(path, ...)`。

子句顺序是固定的，`LIMIT` 在 `OFFSET` 之前，反过来写不认。

`FROM` 与 `INTO` 后面可以是 `$` 打头的系统虚拟集合，名字后面还能带一个 JSON 参数：

```go
db.Execute(ctx, `SELECT $ FROM $cols WHERE type = 'user'`)
db.Execute(ctx, `SELECT $ FROM $file({filename:'in.csv', delimiter:';'})`)
db.Execute(ctx, `SELECT $ INTO $file('out.json') FROM users WHERE $.age > 20`)
```

只有 `SELECT` 的这两处认括号：`INSERT INTO $file(...)`、`DELETE $file(...)` 在语法层
就废掉，因为那两条语句的集合名不走带参数的解析。不带括号的 `DELETE $file` /
`UPDATE $file` 静默返回 0——它们被当成一个不存在的普通集合。
清单与各自的字段见[系统虚拟集合](#系统虚拟集合)。

这两处不是任意的文件与语句入口：`$file` 默认只能读写**打开库时的工作目录之下**的文件，
绝对路径、`..` 与符号链接逃出去都报错（三个相关选项见[把查询结果写成文件](#把查询结果写成文件)）；
`$query('...')` 的内层**只收不带 `INTO` 的 `SELECT`**，`DROP`、`REBUILD`、`DELETE`
这类放进去报错——外面是一句读语句，不能借它改库。

---

## 文件存储

`db.Storage()` 把文件存进库里：元信息一篇文档放 `_files`，内容切成块放 `_chunks`。
两个集合的布局是格式的一部分，同格式的库互读得出来。

```go
st := db.Storage()

// 上传：收 io.Reader，不要求先把文件读进内存
f, _ := os.Open("report.pdf")
defer f.Close()
info, err := st.Upload(ctx, xdoc.String("doc-1"), "report.pdf", f, xdoc.Doc("owner", "ann"))

// 下载：写进 io.Writer
out, _ := os.Create("copy.pdf")
_, err = st.Download(ctx, xdoc.String("doc-1"), out)

// 只取元信息，不碰内容
info, err = st.FindByID(ctx, xdoc.String("doc-1"))
fmt.Println(info.Filename, info.MimeType, info.Length, info.Chunks)

// 列出与删除
for info, err := range st.FindAll(ctx) { ... }
for info, err := range st.Find(ctx, `$.metadata.owner = "ann"`) { ... }
ok, err := st.Delete(ctx, xdoc.String("doc-1"))   // 连同它的所有块
```

要边生成边写、或者边读边处理，用流式的那一对。`FileWriter` 是 `io.WriteCloser`，
**必须 Close**——元信息是在 Close 时才落盘的；`FileReader` 是 `io.ReadCloser`，
另外还能 Seek，所以可以直接喂给 `http.ServeContent`。

```go
w, err := st.OpenWrite(ctx, xdoc.String("log-1"), "app.log", nil)
io.Copy(w, src)
err = w.Flush()   // 想让写到一半的内容先能被读到时才需要，见下
err = w.Close()

r, err := st.OpenRead(ctx, xdoc.String("log-1"))
defer r.Close()
io.Copy(dst, r)
```

`Flush` 把缓冲区里剩下的字节提交成一块——**哪怕不满**，于是文件中间留下一个半块，
元信息也跟着落一个中间态（`length` 记的是到此刻为止的字节数，别的读者在这个窗口里
读到的是一个被截短的文件）。之后还能接着写，`Close` 时长度是总数。不调它不影响
正确性，`Close` 会做同样的事。

`Find` 的表达式可以带位置参数，编号从 `@0` 起：

```go
for info, err := range st.Find(ctx, `$.mimeType = @0 AND $.metadata.tag > @1`, "image/png", 0) { ... }
```

空的或全是空白的表达式**不当「不过滤」**，是一条错误——判空排在解析之前；
要遍历全部用 `FindAll`。

还有两个走本地路径的便捷方法：`UploadFile(ctx, id, path)` 与
`DownloadFile(ctx, id, path, overwrite)`。`SetMetadata` 换掉自定义字段（是整篇替换，
不是合并）。

### 两个集合的布局

`_files` 一篇文档七个字段，顺序固定：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `_id` | 任意 | 文件主键 |
| `filename` | String | 只留最后一段，不含目录；空的存成 `null`（见下） |
| `mimeType` | String | 由扩展名查表得出，认不出来是 `application/octet-stream` |
| `length` | Int64 | 内容总字节数 |
| `chunks` | Int32 | 内容占了几块 |
| `uploadDate` | DateTime | 最后一次写完的时刻，落盘为 UTC |
| `metadata` | Document | 自定义字段 |

`_chunks` 一篇文档两个字段：`_id` 是**一篇文档** `{f: 文件主键, n: 块序号}`（不是把两段
拼成的字符串），`data` 是那一块的字节。主键做成复合文档是有讲究的——文档之间按字段
逐位置比大小，于是同一个文件的块在主键索引里天然连成一段且按 `n` 递增，顺序读一个
文件就是顺着索引走。

### 分块与内存

写出去的块是 255KB（`xdoc.ChunkSize`，这个常数是格式的一部分），最后一块装多少算多少。

**读的时候不认这个数**：一律按每块 `data` 的实际长度走。这个格式里的块大小并不规整——
写入方的做法是「攒够 255KB 就把缓冲区排空」，而缓冲区里攒了多少取决于上游一次递多少
字节，一个 512KB 的文件可以存成 `261120 + 1024 + 260096` 三块，中间那块 1KB 既不是
末块也不满。所以「除最后一块外都是满的」不是这个格式的规则，照着它算偏移会在别处
产生的库上读出错位的数据。

**上传下载都是流式的，内存不随文件大小增长。** 上传只攒一块（255KB 的缓冲区还原地
复用），下载同一时刻也只持有一块——所以接口收 `io.Reader`、写 `io.Writer`，而不是
收发 `[]byte`。实测一个 512MB 的文件走完整个来回，进程峰值常驻内存写 33MB、读 89MB，
与文件大小无关。每一块自成一个事务，不把整次上传包成一个大事务：
那样几十万个脏页要压在内存里等提交。代价是上传中途断掉会留下一批孤块（`_files` 里没有
对应描述）；下一次写同一个 ID 时，写第 0 块（空文件是写描述）的那个事务会先把 `_chunks`
里这个 ID 名下的块按主键区间整段删掉，块号不连续也不漏。探测是同一事务里的只读查找，
没有孤块时写出的字节与不探测时完全一样。

`FileReader` 的 `Seek` 代价也跟着块大小不规整这件事走：**"第 N 个字节落在第几块"算不出来**，
只能顺着把块的长度加起来。已经走过的部分会记下起止偏移不必重读（每块只多记 8 个字节，
一个 4GB 的文件也就 128KB），往回退或跳到没读过的地方则要把中间的块依次取一遍。
跳到文件末尾是个例外——长度是现成的，不用读任何块，所以 `http.ServeContent`
那种"先探末尾再回到开头"的用法是廉价的。

### 与同格式的库互读

`db.Storage()` 取默认的 `_files`/`_chunks`，`db.StorageOn(files, chunks)` 指定集合名——
一个库里可以有多个互不相干的文件存储。主键跟集合的其它接口一致，用 `*Value`，不另设
一个类型参数。

已经验证过的双向互操作：0 字节、1 字节、不足一块、正好一块、正好两块、跨多块且末块
不满，两个方向逐字节相同；覆盖上传、互删、互改元信息、整数主键、512MB 大文件也都
对得上。

有一处是有意为之的：中间缺了一块时这里**报错**，而不是当成"到头了"返回 0 字节——
后者会把一个掉了块的文件静默读成一个短文件，调用方拿到的字节数不对却什么都不知道。
正常文件不受这条影响。

文件名这一段有三处边角：**空文件名不收**（推 MIME 类型这一步排在建写入流之前，
对空串就报错——所以空文件名既不产生新块，也不动已有文件的旧块）；判的是「空」不是
「全空白」，`" "` 是个**合法**的文件名；只留最后一段，于是 `"a/b/"` 这种以分隔符结尾的
串会得到一个空文件名，而空文件名**落盘成 `null` 而不是空串**。走本地路径的
`UploadFile` 比这严一档，全是空白的路径在开文件之前就被挡下来。

另外，`OpenWrite` 一开就把旧的内容块删了（不是等写完再换），代价是：开了流却没写完，
旧内容已经没了而元信息还停在旧的长度上。要保住旧内容，换个主键写完再删旧的。

同一个 ID 的写入是**排队**的：`OpenWrite` 拿到的写入流从打开起一直占着这个 ID，直到
`Close`（集合名不分大小写、主键按排序规则相等来认），其间别的 `OpenWrite`、`Delete`
与 `SetMetadata` 都等着，`ctx` 取消或超时就返回它的错误、文件原样不动——所以忘了
`Close` 会让同 ID 的下一次写入、删除、改元信息一直等下去。排进这个队，删除才不会被
还没写完的写入流在 `Close` 时写回成一个残缺文件，改元信息也不会被写入流的描述盖掉。
**同一个 goroutine 拿着写入流时别对同一个 ID 调 `Delete` 或 `SetMetadata`**：它们要等
的正是自己手里这把锁，只会一直等到 `ctx` 结束。
这把锁只在本进程的同一个 `*DB` 句柄内有效，挡不住别的进程或另开的句柄，也管不到直接
对 `Files()`/`Chunks()` 集合的改写；读（`OpenRead`、`Download`、`FindByID` 等）不排这个队，
读到一半遇上删除或覆盖可能报缺块。

---

## 事务

```go
err := db.Transaction(ctx, func(tx *xdoc.Tx) error {
    from := tx.Collection("accounts")
    if _, err := from.Update(ctx, debited); err != nil {
        return err            // 返回错误即整体回滚
    }
    _, err := from.Update(ctx, credited)
    return err
})
```

回调返回 nil 就提交，返回错误就回滚，**panic 也先回滚再原样抛出**。
事务里看得见自己还没提交的改动。

回调形态而不是 `Begin`/`Commit`：让"一定会收尾"由库保证。漏一次回滚不会报错，
而是那个事务永远占着集合锁——这个集合从此谁也写不进去，每次尝试各自等到超时，
现场看不出是谁没放手。

### fn 可能被调用多次

两个事务以相反顺序访问同一批集合会构成互等。库会检测出来，回滚其中一方并
**重新跑一遍它的 fn**（最多十次，退避翻倍加随机抖动）。

所以 fn 要能被重复执行：它对数据库的改动会随回滚一起消失，但它自己在数据库之外
做的事（发消息、改内存里的计数）不会。**把那些副作用挪到 `Transaction` 返回之后。**

重试用尽会返回一个包着 `ErrDeadlock` 的错误。

### 查出来再改回去

**要把查与写包进同一个事务**，否则挡不住丢更新：

```go
err := db.Transaction(ctx, func(tx *xdoc.Tx) error {
    c := tx.Collection("users")
    for doc, err := range c.Query().Where("$.stale = true").All(ctx) {
        if err != nil {
            return err
        }
        doc.Set("stale", xdoc.Bool(false))
        if _, err := c.Update(ctx, doc); err != nil {
            return err
        }
    }
    return nil
})
```

只改某几个字段时不必自己读改写，`UpdateMany` 一句就够，它也守着同一个事务：

```go
n, err := db.Collection("users").UpdateMany(ctx,
    "{ stale: false, checked: $.checked + 1 }",   // 写到的键覆盖，没写的原样留着
    "$.stale = true")
```

`Query().ForUpdate()` 的写锁**只覆盖这次查询本身**：库级查询各自开一次性事务，
`Slice` 一返回锁就还了。拿它去做「查出来再改回去」挡不住别人在你读完与写回之间
改同一批文档——写回时会把别人的改动盖掉，两边都不报错。它的用处是让一次查询与
并发的写串行开，不是跨越查询与写回的隔离。

`TxCollection.Query()` 与库级的 `Collection.Query()` 只差在事务：它查出来的东西
与事务里的改动互相看得见，也不会在读完之前被外面改掉。

### 事务只认句柄，不认调用它的那段代码

**回调里要用 `tx.Collection(…)`，不能用 `db.Collection(…)`。**

```go
db.Transaction(ctx, func(tx *xdoc.Tx) error {
    tx.Collection("orders").Insert(ctx, a)   // 在事务里
    db.Collection("audit").Insert(ctx, b)    // 不在——它自己开一个事务，当场提交
    return err                               // 回滚带不走上面那一条
})
```

两种表现：

- **同一个集合**：库级那次写要等事务手里的集合锁，等到超时。报错里带着一句指向
  正确写法的话——这是这个误用唯一会自己叫出声的场合。
- **别的集合**：库级那次写自己开一个事务并当场提交，外层回滚也带不走它，而且
  不报错。

**这一条是 Go 上的必然选择。** 把事务绑在执行流上——「`BeginTrans()` 之后同一线程
的任何写入都自动进这个事务」——在 Go 里没有对应物：goroutine 没有稳定身份，而且
一段工作在多个 goroutine 之间挪动是常态，真按 goroutine 绑反而会绑错。
从库这一层看，「回调里调了库级方法」与「另一个 goroutine 正好在写别的集合」
一模一样，而后者必须放行，所以第二种表现连报警都做不到。

SQL 那一侧不受影响：语句没有地方接句柄，所以 `BEGIN` 之后的语句自动进这个事务。

### 事务里按结构体读写

`Typed[T]` 是 `db` 上的方法，不是 `tx` 上的——事务里目前**没有类型化的集合句柄**，
`tx.Collection(...)` 收发的都是 `*Document`。中间那一道自己转，用的还是本库的
映射器（见[手动在结构体与文档之间转](#手动在结构体与文档之间转)）：

```go
d, err := db.MarshalDocument(u)          // 结构体 -> 文档
if err != nil {
    return err
}
err = db.Transaction(ctx, func(tx *xdoc.Tx) error {
    _, e := tx.Collection("users").WithAutoID(xdoc.AutoIDInt64).Insert(ctx, d)
    return e
})

doc, err := db.Collection("users").FindByID(ctx, id)
u, err := db.Decode[User](doc)           // 文档 -> 结构体
```

**那句 `WithAutoID` 省不得**，主键留着零值时尤其。`db.Typed[T]` 会按 `T` 的主键字段
类型挑生成方式（`ID int64` 就发整数），而事务里这个句柄不知道 `T`，用的是库级默认，
也就是 12 字节的 `AutoIDObjectID`。于是一个 `ID int64` 的结构体被塞进一个 ObjectID，
**插入照常成功**，要等到某次 `db.Decode` 读回来才报出来：

```
xmap: document value does not match target type: ObjectId is not an integer (at _id, type int64)
```

那时这类记录已经攒了一批。自己把主键填好再写的不受这条影响。

转换本身放在 `Transaction` 外面：回调可能被重跑（见[fn 可能被调用多次](#fn-可能被调用多次)），
而 `MarshalDocument` 的错误是"这个结构体存不进去"唯一会说出来的地方，别把它吞在回调里。

### 提交后通知

失效缓存、同步搜索、推送 UI 这类事，挂在 `OnCommit` 上，不必去包每一条写路径：

```go
cancel := db.OnCommit(func(ctx context.Context, cs xdoc.ChangeSet) {
    for _, c := range cs.Changes {
        switch c.Op {
        case xdoc.ChangeInsert, xdoc.ChangeUpdate, xdoc.ChangeDelete:
            cache.Forget(c.Collection, c.ID)
        default: // ChangeDeleteAll、ChangeDropCollection、ChangeRenameCollection：ID 为 nil
            cache.ForgetCollection(c.Collection)
        }
    }
})
defer cancel()
```

- **一次提交一次回调**，`ChangeSet` 按发生先后列出这次提交**真正落下去**的变更。
  事务里写多少次都只来一次；找不到主键的 `Update`/`Delete` 不计，`Upsert` 按实际是插入
  还是改写记。
- **所有写路径都算**：集合与类型化集合的增删改、`UpdateMany`/`DeleteMany`、`Transaction`
  与 `BeginTrans`、SQL（`INSERT`/`UPDATE`/`DELETE`、`BEGIN`…`COMMIT`、`SELECT ... INTO`）、
  文件存储（记在 `_files`/`_chunks` 这两个集合上）。
- **集合级变更不列主键**：`DeleteAll`（以及空谓词的 `DeleteMany`、不带 `WHERE` 的 SQL
  `DELETE`）记一条 `ChangeDeleteAll`，删集合记 `ChangeDropCollection`，改名记
  `ChangeRenameCollection`（`Collection` 是旧名，`NewName` 是新名）——不为清空一张大表
  在内存里攒下全部主键。
- **只在提交落盘成功之后**：回滚、提交失败、ctx 取消、整批撞唯一键，一次也不来。
- **同步**：在提交者的 goroutine 里、锁都放掉之后调用，写入调用要等所有回调返回才返回。
  回调里可以读写这个库，不会死锁；它自己的写入提交后会再通知一次，别写成无限循环。
  慢活挪到自己的 goroutine 里。回调拿到的 ctx 是提交那次调用的：`Tx.Commit` 用开事务时
  的，SQL 的 `COMMIT` 用那一句的。
- **进程内、只针对本句柄**：变更只在内存里攒，不写进文件。别的进程、同一文件上另开的
  `*DB`、共享模式下的其它进程，它们的写入这里都收不到；进程崩溃时还没回调的通知也就没了。
- **回调 panic 会原样传给写入调用方，但写入已经提交**，不会因此回滚；排在它后面的回调
  这一次收不到。
- 注册之前已经开始的写入不保证通知。`cancel` 可以重复调，返回之后开始的提交不再回调。
  `ChangeSet` 被同一次提交的所有回调共用，只读。

---

## 并发模型

| 类型 | 能否跨 goroutine 共用 |
|---|---|
| `DB` | 能 |
| `Collection` / `TypedCollection` | 能 |
| `QueryBuilder` | 能（不可变） |
| `Tx` / `TxCollection` | **不能** |

- **读之间不互斥。**
- **读与写靠版本号隔离**：读事务看到的是它开始那一刻的整个数据库，其间别人提交的
  改动不会半路混进来。
- **写同一个集合的事务之间互斥**，写不同集合的可以并行。
- `Tx` 的每一步都依赖前一步的状态，跨 goroutine 共享它没有意义。

**不要在事务里调库级接口。** `db.Collection(...)` 上的写入、改名、删除、建索引、
检查点，每一个都自开一个事务。在 `db.Transaction` 的回调里调它们，等于拿一个新事务
去等外层事务手里的集合锁，而外层正等着这个回调返回——谁也不会先放手，一直等到锁超时：

```go
db.Transaction(ctx, func(tx *xdoc.Tx) error {
    db.Collection("users").Insert(ctx, doc)   // 等自己，直到超时
    tx.Collection("users").Insert(ctx, doc)   // 这样写
    return nil
})
```

互等检测查的是锁的等待图，而外层事务此刻并没有在等任何锁，图上没有环，所以它只能
表现为超时。错误文本里带着一句提示。写别的集合不受影响（锁按集合分），纯读也不受
影响（读不拿集合锁）。

**默认跨进程不安全**：直连模式不取文件锁，两个进程同时打开同一个库会互相写坏。
要让多个进程共用一个库，用下面的共享模式。

### 共享模式（跨进程）

`WithConnection(xdoc.ConnectionShared)` 把这个句柄换成共享模式：**每个操作先取一把
按数据文件路径认的跨进程锁，把库开出来，做完关掉再放锁**。同一时刻只有一个进程在
碰这个库，两个进程于是可以共用同一个文件。

```go
db, err := xdoc.Open("app.db", xdoc.WithConnection(xdoc.ConnectionShared))
```

它与直连模式的差别不止"多了一把锁"，下面这几条都是共享模式的既定行为，
包括其中不合直觉的几条：

- **打开时一个字节都不碰。** 文件不存在、口令不对、带着"上次没有干净关闭"的标记
  ——这些全都推迟到**第一个操作**才报出来。`Open` 只算了一个锁名字。
- **每个操作重开一次库。** 页缓存、序列号缓存、查询执行器全都跟着操作走，不跨操作
  留存。这就是共享模式慢的地方——它换来的是跨进程能用。
- **内存库在共享模式下写不进任何东西。** `OpenMemory(WithConnection(ConnectionShared))`
  的每个操作都从一个**全新的空库**开始：写一篇再数，得到 0 篇。重开就是新建，
  这条不作修正。
- **事务期间独占，而且做完不还锁。** 事务里的每个操作都会把锁多攥一次却不还，
  提交只还一次——于是**一个做过带写入事务的进程，把这个库攥到自己退出为止**，
  别的进程从此全被挡住。这是共享模式的既定行为，实测可复现（事务内写 3 次，另一个
  进程等满持锁进程的剩余寿命；一次都不写则不等）。要改这条行为，改动点集中在
  `leakTransactionLockDepth` 一个常量上。
- **这把锁只在 xdoc 的进程之间通用。** 用的是 `flock(2)`／Windows 的 `LockFileEx`；
  别的运行时若用另一种原语（比如进程共享的 pthread 互斥体）实现同名的锁，两种原语
  互相看不见，**不能同时以共享模式打开同一个库**。xdoc 进程之间是可靠的。
- **重入按句柄，不按 goroutine。** Go 里没有稳定的线程／goroutine 身份，所以按库句柄
  算。与[事务只认句柄](#事务只认句柄不认调用它的那段代码)出自同一个原因。同一句柄上
  并发的多个操作共用同一个打开的库，最后一个做完才关。

锁文件落在 `<临时目录>/.xdoc/shm/global/<名字>.Mutex`，进程被 `kill -9` 时由内核
自动释放，不留陈旧状态。名字是数据文件路径转绝对路径、解开软链接、转小写后的 URI
转义；转义结果太长（各平台都一样）时换成 `sha1-<路径摘要>`。**锁名规则改过之后，
新旧两个版本的进程算出的名字可能不同、彼此不再互斥**——升级时先停掉所有旧版本进程，
再启动新版本。

### 取消与超时

每个收 `ctx` 的方法都认它的取消信号，读和写都认：

```go
ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
defer cancel()

_, err := c.InsertOne(ctx, doc)
if errors.Is(err, context.DeadlineExceeded) { … }
```

三处会检查：

- **操作开始时**。已经取消的 `ctx` 直接返回它的错误，事务根本不开。
- **长遍历的每一条结果**。整表遍历与查询可能扫过整个集合，取消之后立刻停下，
  快照与集合锁跟着还回去。
- **写操作提交之前**。这一处最要紧：批量写入、建一条索引都要跑上一阵，期间被
  取消的话事务**回滚**，一个字都不会落进文件。少了这道检查，调用方已经按
  「取消了」往下走，库里却悄悄多出一批它并不知道的记录——而它不会去回滚，
  也不知道有东西需要回滚。

所以取消一个写操作意味着它**整个不生效**，不是「做了一半」。

等锁也认取消：一个事务等另一个事务放开集合锁时，`ctx` 取消会让它立刻放弃，
不必等满锁超时。

---

## 持久性与崩溃语义

提交**默认等待日志落盘**。

```go
db, _ := xdoc.Open(path, xdoc.WithoutSyncOnCommit())
```

关掉之后提交只把字节交给操作系统页缓存就返回：进程崩溃（`kill -9`、panic）
不丢数据，但**掉电或内核崩溃会丢掉已经返回"提交成功"的事务**。只在数据可重建时
才关它。开关不改变文件内容，两种设置写出的库互通。

### 这条承诺的价钱，以及怎么不白付

等一页落到持久介质上，在普通固态盘上要 1.7 毫秒左右——这是设备本身的开销，换成
`fdatasync`、或者预先把文件空间要够再写，实测都不会更快（1.90 与 1.88 毫秒，都比
直接 `fsync` 的 1.75 慢）。省不掉，只能少做。

**所以一次事务写一篇，和一次事务写一万篇，差的不是一点点：**

```go
// 一万次提交，一万次等待落盘：18.2 秒
for _, d := range docs {
    c.Insert(ctx, d)
}

// 一次提交，一次等待落盘：37 毫秒
c.Insert(ctx, docs...)
```

同样一万篇文档，差了将近五百倍。

批量接口都收变长参数，事务里做的多次写入也只在提交时落一次盘。逐篇写入本身不慢
——慢的是逐篇等待。

写入量大到一个事务装不下时，按批切开（几千篇一批）比逐篇快两个数量级，也比关掉
落盘等待安全得多。

### 崩溃后会发生什么

重开时自动重放日志。**没有完成提交的事务整体消失**——不存在"写了一半"的中间态。

提交点就是确认页的字节落到持久介质那一刻：在它之前崩溃，日志里那批页没有确认
标记，重启扫描时不会被登记，检查点也会跳过它们；在它之后崩溃，整批页一起生效。
中间没有第三种状态，这就是原子性的全部来源，不需要任何回滚日志。

尾部残缺的一页（一次没写完的写入留下的）会在打开时被裁掉——**只读打开时不裁**，
只在内存里当它不存在。

---

## 文件布局与运维

一个库对应两个文件：

```
game.db        数据文件，第 N 页在偏移 N × 8192
game-log.db    日志，追加写
```

日志攒到一定量会自动搬回数据文件并清空（检查点）。关库时若日志是空的，那个文件
会被删掉——不删的话，一个只读过的库关掉之后目录里会凭空多一个 0 字节的 `-log`，
而下次打开又会照样把它建出来。只读打开不删，那种句柄承诺过一个字节都不碰。

自动检查点在提交之后顺手做，前提是那一刻没有事务开着，否则等下次提交再试。事务
前后交叠地一直开着——哪怕只是没走完的游标——这个前提就一直不成立，所以日志涨到
阈值的 4 倍时，提交会排队等在途事务走完，最多等 1 秒：排队期间新开的事务先等着，
排到了就做检查点。等不到（多半是本 goroutine 自己还开着别的事务）就算了，提交照样
成功，下一次排队推到日志再翻一倍时。手工的 `db.Checkpoint(ctx)` 同样排队，新开的
事务要等它做完，或者等它超时放弃；在事务里调它照旧是等锁超时。

### 预分配文件空间

库要装多少数据心里有数时，建库可以一次把空间要够：

```go
db, err := xdoc.Open(path, xdoc.WithInitialSize(64<<20))  // 64 MB
```

撑出来的是全零空白，库的内容一个字节都不变：页照旧从头页后面一页一页地分配，
只是文件不必每长一页就再向文件系统要一次空间。省掉的是一路上所有的扩展，
以及扩展带来的碎片。

三条边界：

- **只在新建库时起作用。** 打开一个已有的库时这个选项整个不看，连下面两条检查都
  不做——已经建好的文件不会因为这里给了个怪数字而突然打不开。
- **加密库不支持**，给了就报错（错误码 210）。加密流的第一页是盐，后面每一页都要
  按页加解密，凭空多出来的全零区在它眼里不是空白而是一片解不开的密文。
- **必须是 8192（一页）的整数倍**，否则报错（错误码 211）。非正数当没给。

两条检查里加密那条在前：一个加密库同时给了个不对齐的数，报的是加密那条。报错时
盘上留下的是一个只有头页、没有日志的文件——那是一个合法的空库，下次不带这个选项
打开就能用。

### 复制一个库

```go
err := db.BackupFile(ctx, "backup.db")   // 目标已存在时报错，不覆盖
```

写出来的就是一份刚做完检查点的数据文件：旁边不需要日志，直接 `Open` 就能用。加密库的备份
仍用原来的口令打开；内存库也能这样落成文件；只读句柄、共享连接照样能备份。要写到别处
（压缩、上传）用 `db.Backup(ctx, w)`，它返回写了多少字节。`BackupFile` 先写临时文件、
刷盘再换名，中途失败或者取消都不会留下半截文件。

备份定住开始那一刻已提交的内容，**写入不必停**：期间的提交照常进行，只是不进这份备份。
代价与边界：

- 检查点要等备份结束才能做，备份期间日志只涨不缩。
- 在途事务的改动不在备份里；它们刚占下的页在备份里可能是没挂进空页链的空页，只占空间，
  重建时回收。
- 共享连接下，备份期间一直攥着跨进程锁，别的进程要等它做完。

**不要先 `Checkpoint` 再拷数据文件**：`Checkpoint` 一返回，别的提交就可能触发自动检查点
改写数据文件，拷到一半的副本前后对不上。

### 只读检查一个可疑文件

```go
db, err := xdoc.Open(path, xdoc.ReadOnly())
```

只读模式**不会碰那个文件哪怕一个字节**：不认领路径、不裁尾部残页、关库时不做检查点。
所以它对任何可疑文件都是安全的，包括带「上次没有干净关闭」标记的那种——
那种文件**可写打开会被拒**（`ErrNeedsRebuild`），只读打开照常放行。处置顺序是
先只读看一眼、把要的内容导出来，再决定就地重建还是从备份恢复：

```go
db, err := xdoc.Open(path, xdoc.ReadOnly())
if errors.Is(err, xdoc.ErrNeedsRebuild) { /* 只读打开不会走到这里 */ }
// …导出内容…
_, err = xdoc.Rebuild(path)   // 重建会替换原文件，所以放在导出之后
```

要不要重建，先让 `Verify` 查一遍再定。它同样以只读方式打开、查完就关，一个字节不写：

```go
rep, err := xdoc.Verify(path)   // 选项与 Open 相同，加密库照样带 WithPassword
if err != nil { /* 查不了：路径不对、口令不对、不是库文件 */ }
if !rep.OK() {
    for _, is := range rep.Issues {
        log.Println(is)             // page 12 users.name [index-order] node 12:3 has key …
    }
    // 先导出要的内容，再 xdoc.Rebuild(path)
}
```

打开着的库用 `db.Verify(ctx)`：看的是调用那一刻已提交的状态，并发写入不会被误报；
它全程占着一个读事务，检查点要等它做完；`ctx` 取消时中途停下，返回取消错误。

**判读**：`err` 与文件好坏无关——文件坏了照样返回 `nil` error，问题都在 `rep.Issues` 里，
每条带页号、所属集合与索引、类别 `Kind` 和一句说明。`rep.OK()` 为真才是没查出问题；
`Kind` 取下面这些值（常量 `xdoc.VerifyKind…`），**出现任何一类都该重建**：

| `Kind` | 查到的是什么 | 意味着 |
|---|---|---|
| `page` | 页读不出来、页号与位置不符、页类型不认识 | 那一页上的内容读不到 |
| `free-list` | 空闲链成环、指出分配范围、链上挂着类型或档次不对的页 | 再分配页时可能把在用的页当空页发出去 |
| `page-owner` | 同一页被两个属主占着，比如既在空页链上又装着数据 | 改一处会写坏另一处 |
| `collection` | 集合页读不出来 | 整个集合读不到 |
| `document` | 文档块链断了、串到别处，或者解不开 | 这几篇读不出来 |
| `index-link` | 跳表或向量图断链、成环、前后指针对不上 | 查找与遍历中途报错或走偏 |
| `index-order` | 相邻键不按库的排序规则递增，唯一索引里有相等的键 | **按索引查找静默漏记录** |
| `index-count` | 索引节点数与文档应有的键数不等；多键索引还逐条指出缺了哪篇的哪个键、多了哪个节点 | 走索引与全表扫描给出不同的答案 |
| `dangling` | 索引节点（含向量索引）指向的位置上没有文档 | 查到已删的文档，或者读的时候报错 |
| `orphan-page` | 数据页属于集合表里没登记的集合 | 集合表丢了登记，见下文的 `col_<集合号>` |

后文列的几种「打开照常、查找静默漏记录」的库——早先版本建的库、复合键、汉字排序规则、
排序规则与索引顺序不符——报出来的都是 `index-order`：重建按库的规则把索引重排一遍，之后就没了。
同一集合、同一索引、同一类问题最多列 100 条，多出来的只计进 `rep.Omitted`。
主键索引与单键索引每篇文档正好一个节点；多键索引（如 `$.tags[*]`）的应有条数用写入时的同一段代码
逐篇求键、判重再累加（按库的排序规则与 `UTC_DATE`，判重规则见「索引」一节），先比总数，
对不上再逐篇定位缺了哪个键、多了哪个节点。有文档读不出来时说不准应有几个键，这条多键索引不核对条数。
向量图里从根走不到的节点不算问题——删掉根之后图本来就可能裂开。

读的过程中若遇到 `IsCorrupt(err)` 为真的错误，说明那一处的字节已经自相矛盾，
那一处重建也搬不过去。重建对两类损坏的反应不一样：

- **文档级的**（某一页读不出来、某一篇解不开）不会让重建停下。常规搬运顺着主键
  索引走，中途断了就改走按页救援——绕开索引把够得着的文档一页一页捡回来。
  少搬的东西记在 `RebuildResult.Skipped` 里，也写进新库的 `_rebuild_errors` 集合。
- **结构级的**（索引建不起来）会中止整次重建，原文件一个字节都不动。

两种都可能少东西，所以核对过新库确实完整之前，别删 `-backup`。

换文件的顺序保证原路径上任何时刻都有一份完整的库：原文件先硬链接成 `-backup`
（文件系统不支持硬链接就整份复制），新文件再一次原子换名顶替原路径。早先的顺序是先把
原文件改名成 `-backup`、再把临时文件 `<库名>.db-rebuild.tmp` 改名过来，两步之间崩溃
就只剩这两个文件。所以**数据文件不存在、而 `-backup` 或重建临时文件还在时，打开会报错，
不会新建空库**：先把那份文件挪回原路径（确认不要了就删掉），再打开。

集合表整块没了是个例外，它能全捡回来。那张表是页 0 上的一篇文档，一次写到一半的
页 0 写入能把它整块抹掉，而文档一篇没少——它们躺在各自的数据页上，每一页的页头
都记着自己属于哪个集合。重建搬完表里那批之后会再扫一遍数据页，把「有数据页、
表里却没登记」的集合捡回来，叫作 `col_<集合号>`：

```go
_, err := xdoc.Rebuild("game.db")
// 集合表没了的话，原来的 users / orders 会变成 col_1 / col_4，文档一篇不少
```

名字改不回来——原名只记在被抹掉的那张表里，没有第二处存着。索引也捡不回来，
只剩主键：索引的名字与取键表达式记在集合页上，而集合页读不出来正是走到这一步的
前提。`col_<集合号>` 这个命名规则是格式的一部分：同一个坏文件重建出来，给出的是
同一批名字。

连一页数据页都找不到、集合表又是空的时候，重建**拒绝**动手：那时分不清「集合真被
删光了」和「损坏还伤到了别处」，而拿一个空库去顶替原文件是不可逆的。

### 自动重建

```go
db, err := xdoc.Open(path, xdoc.WithAutoRebuild())
```

带「上次没有干净关闭」标记的库，打开时自动重建再打开，不报 `ErrNeedsRebuild`。
原文件留作 `<库名>-backup.db`；搬不干净的话，跳过了哪些页写在新库的
`_rebuild_errors` 集合里——这条路没有调用方接得住 `RebuildResult`，那个集合是
唯一的报告。

只有这一个标记走自动重建。口令不对、比较规则对不上照常报出来：拿重建去回应
它们，等于用一次替换原文件的操作回应一个打错的参数。

**拿不准就别设它。** 默认的拒绝给你留下一个原封不动的文件，先只读看一眼、把要的
内容导出来，再决定是重建还是从备份恢复。

#### `_rebuild_errors`

搬不干净时，新库里会多一个这个集合，一条跳过一篇文档，字段如下：

| 字段 | 内容 |
|---|---|
| `buildId` | 这一次重建的标识，同一次的所有行共用 |
| `created` | 记下这一条的时刻 |
| `pageID` / `positionID` | 出问题的页号，以及它在文件里的字节偏移 |
| `origin` | 恒为 `"Data"`，见下 |
| `pageType` | 页类型；整页读不出来时记 `"Empty"` |
| `message` | 读到的错误 |
| `exception` | `{ code, hresult, type, inner, stacktrace }` |

有三个字段在这里没有对应物，一律写 Null 或固定值，而不是编一个：

- `exception.hresult` 是宿主运行时的 32 位错误号，Go 的 `error` 没有这个东西 → Null。
- `exception.stacktrace` 要求错误里带着调用栈，本实现的错误不带 → Null。
- `origin` 这一位区分 `Data` / `Log`，那是分别扫两个文件时才有的信息；这里搬运读的是
  数据文件与日志合并之后的视图，一页来自哪个文件在那一层已经看不见 → 恒为 `"Data"`。

还有一处：报告文档里**没有出问题的集合名**这一项。这个字段在格式里就是缺的，
这里也不补。

### 内存库

```go
db, err := xdoc.OpenMemory()
```

格式与落在文件上的完全一样，进程退出即消失。适合验证一段写入逻辑而不碰磁盘。

### 数据迁移

```go
if db.UserVersion() < 3 {
    // …迁移…
    err = db.SetUserVersion(ctx, 3)
}
```

`SetUserVersion` 会开一个自己的事务把改动落下去，因此可能失败。

### 库级设置（pragma）

六项写在文件头里、库开着的时候还能改的设置。改了立刻生效，也随文件走——
下次打开、别的程序打开，读到的都是改过的值。

| 名字 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `USER_VERSION` | int32 | 0 | 自定义版本号，做迁移判据用 |
| `TIMEOUT` | int32（秒） | 60 | 等锁超时 |
| `CHECKPOINT` | int32（页） | 1000 | 日志涨到这么多页就搬回数据文件，0 关掉 |
| `LIMIT_SIZE` | int64（字节） | 无上限 | 数据文件大小上限 |
| `UTC_DATE` | bool | false | 日期读出来落在世界时还是本地时 |
| `COLLATION` | string | `/Ordinal` | 字符串比较规则，**只读** |

```go
d := db.Timeout()                       // 60s
err := db.SetTimeout(ctx, 90*time.Second)
err = db.SetCheckpointSize(ctx, 0)      // 关掉自动检查点
err = db.SetUTCDate(ctx, true)          // 之后读出来的日期都是 UTC 的
n := db.LimitSize()
c := db.Collation()                     // 只读

// 名字来自配置文件时用按名字的那一对
v, err := db.Pragma("CHECKPOINT")
changed, err := db.SetPragma(ctx, "CHECKPOINT", xdoc.Val(500))
```

SQL 面是同一套：

```go
db.Execute(ctx, `PRAGMA TIMEOUT`)        // 产出 60
db.Execute(ctx, `PRAGMA TIMEOUT = 90`)   // 产出 true（改了）；再来一次产出 false
```

几处容易绊到的地方：

- **写一个与当前相同的值什么都不做**，返回 `false`，不开事务、不写日志。设置常写在
  启动代码里每次开库跑一遍，不这样的话每次开库都要提交一个空事务，只读打开的库
  还会因此失败。这一条排在最前面，于是 `PRAGMA COLLATION = <当前值>` 不报「只读」
  而是安静地返回 `false`。
- **事务里不能写**（SQL 的 `BEGIN` 开着时）。这道检查排在校验之前，所以事务里写一个
  非法值报的是「事务里不能写」，不是「值不合法」。
- **校验判的是值本身，不是转换后的数**。`CHECKPOINT = -0.4` 被拒（它小于 0），
  尽管取整之后是 0。
- **转不动与超范围是两类错，先后是定死的**：先按值本身判范围，再做类型转换。
  所以 `TIMEOUT = 0` 报「必须大于零」，而 `TIMEOUT = 'x'` 报「不是整数」——
  后者的值在范围判据里比不出小于零（字符串排在数字之后），落到转换那一步才报。
- **转不动的值只是这一句报错，库照常能用**。类型转换在开事务之前就问完了。
  留到写盘那一步问的后果不是「这一句报错」，而是一次失败的提交把整个库句柄废掉。
- **`UTC_DATE` 只认布尔**，`= 1` 与 `= "true"` 都是错的；而 `CHECKPOINT`、`TIMEOUT`、
  `LIMIT_SIZE`、`USER_VERSION` 认字符串与浮点（浮点按四舍六入五成双取整）。这个不
  对称是格式定死的：布尔那项走的是一次强制转换，整数那几项走的是带转换的读法。
- **`UTC_DATE` 改完立刻影响读**，连正在用的查询执行器与取索引键的那一段也跟着变。
  但已经建好的索引不会重算——建在 `HOUR($.d)` 这类看时区的表达式上的索引，改完之后
  要重建才对得上。
- **`LIMIT_SIZE` 至少四页（32768 字节），且不得小于文件现在的大小。**

### 运行时统计与日志

`$database` 回答的是「文件现在什么样」；「库跑得怎么样」看 `db.Stats()`：

```go
s := db.Stats()
hitRate := float64(s.CacheHits) / float64(s.CacheHits+s.CacheMisses)
fmt.Println(hitRate, s.LogPages, s.Checkpoints, s.CheckpointDuration, s.LockTimeouts)
```

| 字段 | 含义 |
|---|---|
| `CacheHits` / `CacheMisses` | 读页时页缓存命中 / 未命中的次数 |
| `LogPages` | 日志此刻有几页，即还没搬回数据文件的量 |
| `Checkpoints` / `CheckpointDuration` | 做成的检查点次数与累计耗时，只算日志非空的那些 |
| `CheckpointFailures` | 检查点失败的次数 |
| `LockWaits` | 拿闸门或集合锁时真的挂起等过的次数，一次获取只算一次 |
| `LockTimeouts` | 等锁等到超时（或 ctx 先结束）的次数 |
| `Deadlocks` | 检测到事务互等的次数，`Transaction` 自动重试掉的也算 |
| `OpenTransactions` / `OpenCursors` | 此刻开着的事务数、没走完的查询数 |

- **计数跟着句柄走**，从打开起一直累加：`db.Rebuild` 换掉底层实例、共享模式每个操作
  开关一次文件，都不清零。重建时搬运本身读写的页也记在里面。
- 计数全是原子变量，缓存命中按分片各记一份，**读页路径不加锁、不分配**。`Stats()` 不开
  事务、不碰文件，可以随时并发调用；共享模式下文件恰好没开着时 `LogPages` 与
  `OpenTransactions` 为 0。
- `LockTimeouts` 只算库内的闸门与集合锁；共享模式跨进程锁的等待、`Rebuild` 等在途操作
  退出的那段不在里面。提交路径上为防日志无界增长而排队等检查点、等不到就算了的那一下
  也不算超时——那不是调用方看得到的失败。

`WithLogger` 把少见但值得知道的事件记成 `log/slog` 结构化日志：

```go
db, err := xdoc.Open(path, xdoc.WithAutoRebuild(), xdoc.WithLogger(slog.Default()))
```

| 消息 | 级别 | 属性 |
|---|---|---|
| `xdoc: auto rebuild started` | Warn | `path`、`reason` |
| `xdoc: auto rebuild finished` | Info；有跳过或抢救时 Warn | `path`、`backup`、`collections`、`documents`、`skipped`、`salvaged`、`elapsed` |
| `xdoc: auto rebuild failed` | Error | `path`、`elapsed`、`error` |
| `xdoc: checkpoint failed` | Error | `logPages`、`moved`、`error` |
| `xdoc: database instance broken; reopen it` | Error | `error` |
| `xdoc: lock timeout` | Warn | `lock`（`shared gate` / `exclusive gate` / `collection <名字>`）、`error` |
| `xdoc: transaction deadlock, retrying` | Info | `attempt`、`backoff`、`error` |
| `xdoc: transaction deadlock, giving up` | Warn | `attempt`、`error` |

**不设就一条也不输出**，也不会退回到 `slog.Default()`。有了它，自动重建不再只有
[`_rebuild_errors`](#_rebuild_errors) 一份报告：跳过了几处在打开的那一刻就能知道。
日志都在放开锁之后才记，处理器慢只拖慢出事的那一方，不会拖住别的等锁者。

### 系统虚拟集合

名字以 `$` 打头的那一批集合，内容不是存在库里的文档，而是**现算出来的**——集合表、
索引表、库的状态、在途事务、页的分布。它们走的是普通查询那条路，`WHERE`、`ORDER BY`、
`GROUP BY`、投影一样管用：

```sql
SELECT $ FROM $cols WHERE type = 'user'
SELECT {pageType, n: COUNT($._id)} FROM $dump GROUP BY pageType
```

| 名字 | 一行是什么 | 拿它回答什么问题 |
|---|---|---|
| `$database` | 整个库，恒一行 | 文件多大、缓存用了多少、pragma 现在是什么 |
| `$cols` | 一个集合 | 库里都有什么 |
| `$indexes` | 一条索引 | 索引建在哪个表达式上、唯不唯一 |
| `$sequences` | 一个集合的自增值 | 下一个自增主键会是几 |
| `$transactions` | 一个在途事务 | 谁开着事务没结束 |
| `$snapshots` | 一个事务对一个集合的视图 | 谁占着这个集合 |
| `$open_cursors` | 一次没走完的查询 | **这个集合怎么写不进去了** |
| `$file` | 一份 JSON / CSV 文件里的一条记录 | 导入导出，见下一节 |
| `$dump` | 一页 | 空间去哪了、这一页是从数据文件还是日志读到的 |
| `$page_list` | 一页（只列挂在空闲链上的） | 哪些页还有空位、碎在哪 |
| `$query` | 内层 SQL 的一行结果 | 拿一条查询的结果当另一条的数据源（内层只收不带 `INTO` 的 `SELECT`） |

`$open_cursors` 是排查「写操作卡住」的第一站。遍历期间查询占着快照与集合锁，
调用方在 `for range` 里中途 `break` 去做别的事，这条查询就一直挂着；不看这张表的话，
现场只看得到「写操作在等锁」，看不出是谁占着：

```go
for d := range c.Query().All(ctx) {
    // 这里做了一件很慢的事，期间：
    // SELECT $ FROM $open_cursors
    //   → {collection: users, running: false, fetched: 1, sql: SELECT $ FROM users}
}
```

`running: false` 的意思是引擎停在那儿等调用方，`elapsedMS` 里**不含**调用方处理每行
的时间——要找的是「谁慢」，把等调用方的时间算进来会指向错的那一半。

三条与格式里的形态有意不同的地方：

- `$transactions` / `$snapshots` / `$open_cursors` **没有 `threadID` 字段**。
  事务在这套实现里不绑线程，一个事务可以在任意 goroutine 上推进。
- `$cols` 与 `$page_list` 里集合的先后是**名字序**，不是建集合的先后。集合表在这里
  是排序遍历的，为的是同一份内容每次写出的字节都相同。
- `$page_list(<越界页号>)` 报错之后**库照常能用**，不会把整个引擎标成坏的、此后连普通
  查询都重抛同一条错误。

名字后面可以带一个参数，括号里是**一个 JSON 值**而不是参数列表：

```sql
SELECT $ FROM $page_list(2)                    -- 只看第 2 页
SELECT $ FROM $dump(0)                         -- 带页号时多一个 buffer 字段（整页 8192 字节）
SELECT $ FROM $file({filename:'a.csv', delimiter:';'})
```

不是文档时，那个值整个落到唯一没有默认值的键上——`$page_list(2)` 因此等价于
`$page_list({pageID:2})`，`$file('a.json')` 等价于 `$file({filename:'a.json'})`。

### 把查询结果写成文件

`$file` 反过来也能当输出目标。格式从扩展名推，`format` 选项可以覆盖：

```sql
SELECT $ INTO $file('out.json') FROM users WHERE age > 20
SELECT $ INTO $file({filename:'out.csv', delimiter:';', header:false}) FROM users
SELECT $ FROM $file('in.json') WHERE $.age > 20
```

产出是写进去的篇数。选项：`filename`（必填）、`format`、`encoding`（默认 `utf-8`）、
`pretty` / `indent`（JSON 输出）、`overwritten`、`delimiter`（CSV）、
`header`（CSV：**读**时是列名数组，**写**时是「要不要写表头行」的布尔量——同名不同义，
这处不对称是格式定死的）。

要紧的几条：

- **默认只能碰打开库时工作目录之下的文件。** `filename` 按那个目录解析，底下走
  `os.Root`：绝对路径、`..` 逃出去、符号链接指到外面一律报错；`out.json`、`sub/in.csv`
  这类相对路径照常。打开之后进程再换工作目录，根也不跟着走。三个选项调整它：
  `WithFileRoot(dir)` 换一个根（相对的 `dir` 按打开时的工作目录解析）；
  `WithoutFileAccess()` 整个禁掉 `$file`，读写都报错，一篇都不写的导出也报；
  `WithUnrestrictedFileAccess()` 恢复不设限的旧行为，文件名原样交给操作系统——
  **SQL 来自不可信来源时别用**，`overwritten:true` 能覆写进程可写的任意文件。
- **写文件不在事务里。** `BEGIN` 之后导出、再 `ROLLBACK`，文件仍然在那儿。
  文件系统没有参与两阶段提交的办法。
- **一篇都没有时文件根本不建**，返回 0。一句筛不出东西的导出语句不会留下空文件，
  也不会覆盖同名文件。
- 目标已存在且没给 `overwritten` 就报错。
- **给了 `overwritten` 也不截断**：只是把游标放回 0 开始写，不动文件已有的长度。
  新内容比旧的短时，旧文件超出的那一截原样留在后面——写出来的 JSON 因此在 `]`
  之后多一段垃圾，不合法。CSV 同理。这是"打开或新建、不截断"的既定语义，不作修正：
  截断掉固然更"对"，可那样一来，按这个格式的输出写的读取方拿到这里的文件会走另一条
  路，而两边谁也不报错。要干净的覆盖，先把目标删掉再导。
- **CSV 读回来的值全是字符串**，没有类型推断：`1` 读回来是 `"1"`。
- **CSV 写出来的最后一行没有换行，而读侧会丢掉没有换行的最后一行**——所以
  CSV 自己往返会少一篇。这两条都是格式的既定行为，为的是与同格式写出的文件逐字节
  互通。要往返用 JSON。

### 其它

```go
names := db.CollectionNames()
ok, err := db.DropCollection(ctx, "tmp")
err = db.RenameCollection(ctx, "old", "new")
db, _ := xdoc.Open(path, xdoc.WithCacheSize(20000))  // 共享页缓存，单位是页
```

---

## 错误处理

可判别的哨兵错误：

| 错误 | 含义 |
|---|---|
| `ErrNotFound` | 按主键或查询没找到 |
| `ErrDeadlock` | 事务互等，回滚重试（`Transaction` 会自己重试） |
| `ErrAlreadyOpen` | 这个文件已经在本进程里以可写方式打开着 |
| `ErrNeedsRebuild` | 带「上次没有干净关闭」标记，要先 `Rebuild` 才能写；只读打开不受此限 |
| `ErrBroken` | 实例因一次写盘失败而作废，关掉重开 |
| `ErrClosed` | 库已经关了 |
| `ErrDuplicateKey` | 往唯一索引里插了已存在的键；主键重复请用 `Upsert`，这条是给二级唯一索引的 |

其余错误都用 `%w` 包装，`errors.Is` / `errors.As` 可用。

盘上的字节自相矛盾用 `IsCorrupt(err)` 判别，它同时认页级与文档级两层。这一类
**不该重试**：重试的结果永远一样，而每重试一次就晚一点发现该去恢复备份。

```go
if xdoc.IsCorrupt(err) { /* 从备份恢复，或 Rebuild 抢救还搬得动的部分 */ }
```

表达式或查询本身写坏了用 `IsExprError(err)` 判别。它答的不是「是哪一种语法错误」，
而是「这条表达式我们接不住」——查询条件来自客户端时，这就是该回 400 还是 500 的判据。

**它可能在迭代到第几百篇文档时才触发**，所以不能只在构建查询时校验一次：
`FIRST($.tags[*])` 要撞上一篇 `tags` 为空的文档才报错，而那篇文档在结果集的哪个位置
事先不知道。解析期和求值期它都认。

四路分流的完整写法：

```go
switch {
case errors.Is(err, xdoc.ErrNotFound):
    return http.StatusNotFound, nil
case errors.Is(err, xdoc.ErrDuplicateKey):
    return http.StatusConflict, nil
case xdoc.IsExprError(err):
    return http.StatusBadRequest, err // 客户端把查询写坏了
case xdoc.IsCorrupt(err):
    return http.StatusInternalServerError, err // 去恢复备份，别重试
default:
    return http.StatusInternalServerError, err
}
```

`ErrDuplicateKey` 上有一件事值得单说：**主键重复不该靠它**。`Upsert` 直接就是答案，
无竞态，还顺带告诉你插了几条改了几条。它是给二级唯一索引用的——用户名、邮箱这一类。
那种情形今天没有别的出路：事务里的集合句柄没有 `Query`，想在插之前先确认一遍就只剩
全集合扫描，每插一条扫一遍。

批量 `Insert` 里有一篇撞上时**整批都不生效**（见[集合与文档](#集合与文档)），
所以拿到这条错误不必再去猜前面几篇写进去没有。

**找不到不返回空文档**：那样调用方分不清"没有这条记录"和"这条记录的字段都是空的"，
而这两者在业务上往往要走完全不同的分支。

### 数据文件层的错误码

打开、加密、页读写这一层的错误链上带着一个错误码。码值是对外契约，**只增不重排**，
所以它可以进日志、进工单，用户按它检索到的东西不会在下一个版本对不上。
码本身是内部包里的类型，外面命名不出来，但它的方法集写得出来——用一个结构性接口取：

```go
var c interface {
    error
    String() string   // 码的符号名，如 INVALID_PASSWORD
    Critical() bool   // 引擎内部状态是否已不可信
}
if errors.As(err, &c) {
    log.Printf("xdoc code=%s critical=%v: %v", c.String(), c.Critical(), err)
}
```

`Critical()` 为真的只有 9xx 那一段（今天只有 `INVALID_DATAFILE_STATE`），意思是内存
里的页状态已经不可信，该关掉这个库而不是就地重试；1xx / 2xx 都是可以就地处理的。

**大多数错误没有码。** `ErrNotFound`、撞唯一索引、库已关闭这些走的是上面那些哨兵，
`errors.As` 到这个接口上不会命中——这一节只覆盖数据文件层。

#### 这几个码分不干净，不要拿它们做自动处置

打开一个加密库时能拿到的三个码，含义并不像名字看上去那么确定：

| 码 | 它真正回答的问题 |
|---|---|
| `NOT_ENCRYPTED` (216) | 文件第 0 个字节不是「已加密」标记 |
| `INVALID_PASSWORD` (217) | 口令校验块解不回来 |
| `INVALID_DATABASE` (103) | 上面两关之外的头部异常，比如校验块被清成全零 |

两处实测过的不可区分：

- 一段**纯随机字节**，只要第 0 个字节恰好是 1，`Open(path, WithPassword(…))` 报的
  就是 `[217 INVALID_PASSWORD] wrong password`。这不是实现偷懒：口令不对时解出来的本来
  就是伪随机字节，和"这根本不是本库的文件"在字节上分不开。码表自己也把这一点写在
  注释里——103 那一行的说明是「头页魔数或格式版本不对；口令错误也可能落到这里」。
- 一个第 0 字节为 0 的垃圾文件，和一个真正的明文库，同样都报
  `[216 NOT_ENCRYPTED] the file is not encrypted, but a password was given`。

所以这三个码适合拿去**给人看**（"口令可能不对，也可能选错了文件"），不适合接分支：
把 `INVALID_PASSWORD` 接到"提示用户重输口令"上，会让一个选错文件的人一遍遍重打
一个本来就正确的口令。

反过来，**忘了给口令**这一条恰好没有码：它在到加密层之前就被拦下了，报的是
`xtx: database is encrypted; a password is required`。上面三个码之外唯一能干净判定
的情形，偏偏不在码表上——这一节的价值在于说清哪些分不开，不在于给出一张完整的分流表。

---

## 性能

5 万篇文档（每篇约 250 字节）、两条二级索引，跑在普通固态盘的 ext4 上，各跑
5 轮取中位数：

| 操作 | 耗时 |
|---|---:|
| 批量写入 5 万篇 | 558 ms |
| 空集合上建两条索引 | 5.5 ms |
| 主键点查 ×1 万 | 72 ms |
| 索引等值查询 ×10 | 23 ms |
| 索引范围查询 ×10 | 170 ms |
| 全表扫描 | 57 ms |
| `Count()` 全表 | 7 ms |
| 分页 `Skip`+`Limit` ×100 | 472 ms |
| 取第一条（提前停止）×1000 | 8 ms |
| 有索引字段排序取前 100 | 0.2 ms |
| 无索引字段全量排序 5 万篇 | 116 ms |
| 分组统计 | 99 ms |
| 事务内写 5000 篇并提交 | 25 ms |
| 多键索引写入 2 万篇 | 192 ms |
| 关闭后重开并计数 | 9 ms |

20 万篇（87 MB）的库上，进程常驻内存：建库后 34 MB，全表扫描峰值 100 MB，
八路并发扫描 178 MB——**不随库的大小增长**，页缓存有上限，扫过的页不会被钉住。

### 一条写入路径上的取舍

逐篇写入每一次都要等日志落到盘上（见[持久性与崩溃语义](#持久性与崩溃语义)），
而这一等在普通固态盘上是 1.7 毫秒左右。一万篇文档：

| 写法 | 耗时 |
|---|---:|
| 逐篇 `Insert`，一万次提交 | 18.2 s |
| 一次 `Insert(ctx, docs...)`，一次提交 | 37 ms |

差了将近五百倍，而两者写出的文件完全一样。慢的不是写入，是等待——批量接口都收
变长参数，事务里的多次写入也只在提交时落一次盘。

> 数字来自一次具体的测量（WSL2 上的 ext4），只说明各操作之间的相对量级，不代表
> 任何绝对值。换文件系统结论会变：在 9p 挂载的 Windows 盘上单次页写入要 0.4 ms，
> 而在内存盘上落盘几乎不要钱——后者会让上面那五百倍的差距完全看不见。

---

## 限制

- **单进程，且一个进程里同一个库只能打开一份可写句柄**。第二次打开会被拒绝
  （`ErrAlreadyOpen`），因为两份句柄各有一份内存里的头页，各自分配页号、各自跑
  检查点，写出的日志互相覆盖——后关的那份赢，先关那份提交过的整个集合会连同全部
  文档一起消失，而写入当时返回的是成功。只读句柄不受限制，多开几份互不干扰。
  **默认的直连模式跨进程挡不住**，多进程同时打开同一个库会互相写坏。要跨进程共用
  同一个文件，显式换成[共享模式](#共享模式跨进程)——它按数据文件路径取一把
  `flock(2)`／`LockFileEx`，代价与几条不合直觉的既定行为都写在那一节里。
- **新建的库按码元比较字符串，因此区分大小写**。字符串比较规则决定索引里字符串键的
  物理顺序，而语言学排序（`ä` 排在 `a` 后面还是 `z` 后面、下划线算不算一个字符）
  由国际化库的版本与实现决定，两个实现即使都遵循同一套标准，在全角字母、汉字与
  拉丁字母的先后这些角落上仍会有出入。按码元比较没有这个余地：字符串编成 UTF-16
  码元后逐个比大小，任何实现算出来都是同一个顺序。规则连同它的标识写进文件头，
  别的程序打开时照它来。建库时想换成别的规则，用 `WithCollation`。

  打开一个**已有**的库时，`WithCollation` 给的规则与文件头里那条对不上会**报错**，
  而不是悄悄用文件里那条。索引里字符串键的物理顺序是建索引时按文件头那条排出来的，
  拿另一条规则去查就是在别人排好的顺序上做二分——漏掉的记录不报错，只是查不到。
  要换规则只有一条路：`Rebuild(path, WithCollation(…))`，它把内容重搬一遍、
  索引按新规则重排。

  **默认取「按码元」是有意的**：常见的另一种默认是「当前机器的区域 + 忽略大小写」，
  那样同一份代码在两台机器上建出的库，索引里字符串键的物理顺序可能不一样——一台上
  写进去的库，另一台打开时会沿着错误的分支查下去，静默漏记录，而两头都不报错。
  要那种默认，建库时写 `WithCollation("<你的区域>/IgnoreCase")`。
- **带「上次没有干净关闭」标记的库，可写打开会被拒**（`ErrNeedsRebuild`）。
  只读打开不受限制，`WithAutoRebuild` 可以让它自动重建再打开。

  **拒绝打开是有意的**：不设 `auto-rebuild` 时照常打开、接着往里写是更宽松的做法，
  但那个标记的含义是「内部结构可能不自洽」，在不自洽的结构上继续写会让
  不自洽处扩散——一次能救回九成内容的重建，拖到几百次写入之后可能一成都救不回，
  而这中间没有任何迹象。拒绝换来的代价是多一步人工决定，而那一步本来就该有人做：
  重建会替换原文件，这是不该由默认设置替人决定的事。
- **事务只认句柄**：回调里要用 `tx.Collection(…)`，`db.Collection(…)` 不进这个事务。
  把事务绑在执行流上在 Go 里没有对应物，见[事务只认句柄](#事务只认句柄不认调用它的那段代码)。
- **按码元排出来的顺序，与 Go 里 `a < b` 的顺序在辅助平面上是相反的**。UTF-16 把
  U+10000 以上的字符编成一对代理码元，首码元落在 U+D800–U+DBFF，于是 emoji 与扩展区
  汉字排在 U+E000–U+FFFF 那一段**之前**；而 Go 的字符串比较是按字节，对合法 UTF-8
  等同于按码点，那些字符排在**之后**。基本平面内两套顺序完全一致，所以这件事只在数据里
  真出现辅助平面字符时才露头：

  ```go
  xdoc.DefaultCollation().Compare("\U0001F600", "\uE000") // -1，emoji 在前
  "\U0001F600" < "\uE000"                        // false，emoji 在后
  ```

  影响的是拿本地排序结果去核对索引顺序、或在别处按码点建好索引再搬进来这两件事。
  库内部自始至终只用码元序，单用 xdoc 读写不会撞上。
- **打开既有库时用文件头里写的那条规则，不是本进程的偏好**。索引里的顺序是建索引时
  按那条规则排出来的，换一条去查就是在另一种顺序上做二分，漏掉的记录不会报错。
  认不出的区域号退回不依赖区域的那一档，没有对应实现的选项位（忽略符号、假名类型、
  标点优先）按忽略处理——能打开它，好过让一个只是区域号填得不同的库根本打不开。
- **用早先版本建的库要重建一次再用**（改动前那些版本）。两处顺序变了：
  一是字符串——它们的文件头写着「不依赖区域 + 忽略大小写」，而当时的实现对这条标识做
  的是「折成大写后按码元比」，现在同一个标识按语言学排序解释，两者在 `user_a` /
  `order-1` 这类带下划线连字符的键上顺序相反；二是 12 字节标识（默认主键）——此前按
  12 个字节顺着比，现在按它的四个字段比，其中时间戳与进程号是有符号的，而进程号那两
  个字节的高位为 1 时两种比法给出相反的次序。不重建就打开，等值与范围查找会静默少返回
  记录，继续写入还会把按老顺序排好的跳表改坏。
- **数组或文档做索引键、且库的规则不是按码元时，也要重建一次**。复合值的比较现在
  在进入数组/文档的那一层就换成按码元，不再往下传库的规则——两份实现对复合键必须
  排出同一个顺序，否则跳表查找会从错误的分支下探，静默漏行。按码元的库（新建库的
  默认）不受影响，键的字节顺序一个也没变；忽略大小写的库上语义确实变了：
  `$.tags = ["a"]` 不再匹配 `{tags:["A"]}`，唯一索引也不再把这两者判成重复。

  ```go
  // 重建后索引按新解释重排，文件头的标识不变，从此自洽
  _, err := xdoc.Rebuild("game.db")
  // 想顺便换成按码元比较（区分大小写、跨实现最稳）：
  _, err := xdoc.Rebuild("game.db", xdoc.WithCollation("127/Ordinal"))
  ```

  重建只顺着链表读，不做索引查找，所以即使当前规则与索引顺序不符也能把数据全搬过去。
- **汉字优先的排序规则（中文区域号那一类）只在键全是汉字时对得上**。那类规则把汉字
  整块排到拉丁字母之前，而这里的实现做不到整块调序。键里既有汉字又有拉丁字母时，
  索引查找会静默漏记录：实测纯汉字键 20/20 命中，中西混合键 4/20。碰上这种库，
  用 `Rebuild(path, WithCollation("127/Ordinal"))` 迁到按码元比较，是唯一可靠的出路。
- **非按码元比较的库上，`LIKE 'x%'` 走索引要扫完整段字符串键**。那类规则允许长度
  不同的两段相等（软连字符这类字符在排序里被整个忽略），前缀匹配因此没有可以就此
  收手的边界。结果正确，但代价接近全表扫。
- **区域数据是一份内嵌快照，且不含两处国际化库内部的规则**。表里 870 个区域的名字
  （不分大小写）、加上任意私有后缀之后的写法（`en-US-x-foo`）、以及往回缩一段能对上
  的写法（`de-XX` → `de`、`fr-FR-u-nu-latn` → `fr-FR`），一个不差。缩不到的两处：

  1. **语言认不出、地区认得出**时，完整的规则是货币符号与货币小数位取自地区（`und-JP`
     该是「根数据的分隔符 + ¥ + 0 位小数」，`de-US` 该是「德语的分隔符 + $」），
     这里给的是通用货币符号 `¤`。补上它要再搬一张「(区域, 货币) → 符号」的稀疏矩阵
     （同一种货币在不同区域里印法不同，美元在中文区域里是 `US$`）。
  2. **`-u-nu-<记数法>` 扩展**该换成这个区域里另一组数字符号（`ar-SA-u-nu-latn`
     该用拉丁的 `.` 与 `,`），这里只是把扩展段缩掉、用基础区域的符号。

  能触发这两条的名字（`de-US`、`und-JP`、`zz-ZZ-ZZ`、`ar-SA-u-nu-latn`）都不是任何
  一个真区域的名字。区域名**写法合不合规**的判定同样对拍过（两万个随机名字差 63 个，
  全是随机生成的乱名字，触发的是 `root-` 与 `x-` 两条内部分支）。
- **字段名的大小写折叠只折 ASCII**（这一条与上面的比较规则无关，走的是另一条规则）。
  `Ä` 与 `ä` 是两个不同的字段名。非 ASCII 的大小写映射随国际化库版本变化，折进去
  会让同一篇文档在不同机器上解析出不同的字段。
- **单篇文档上限约 16 MB**，索引键编码后上限 1023 字节。
- **文档与数组最多嵌套 1024 层**，最外层的文档算第 1 层。写的时候超了直接报错，
  自引用的文档也在这里报错而不是无限递归；读到超过这个深度的字节判成文档损坏，
  一段坏字节因此不会让进程栈溢出崩掉。
- **自增主键的序列只在内存里**，回滚过的号不退回，主键会有空洞。
- **别给向量字段建普通索引**：建得成，第一篇也写得进，写下的键却读不回来。索引键的
  类型字节只有低 6 位是类型，向量的编号 100 越出了这 6 位，读回来被掩成一个不存在的
  类型——从此每一次读到这个键的查找都报文档损坏，而报错处离那次写入已经隔了很远。
  排序键走同一条路。这是索引键编码本身的行为，不是这里额外加的限制。向量检索用
  `EnsureVectorIndex`，那是另一套索引，不走索引键这条路。
- **向量维数上限 65535**，长度字段只有 16 位，超了在写入侧就拒绝。走索引键那条路
  还另受 1023 字节的键长上限（键是 `[类型字节][维数 2 字节][维数 × 4 字节]`），
  维数超过 255 的向量连写都写不进去。
- **集合名不区分大小写**：`Users` 与 `users` 是同一个集合。集合表在文件头里是一篇
  文档，而文档的字段名本来就不区分大小写——内存里若按大小写区分，两者会各自建集合页、
  各自写数据，直到写回文件时才在文档那一层撞成一个，先写的那个集合连同它的文档全部
  消失。集合列表返回的是建它时用的那个写法。
- **打开一个带本实现维护不了的索引的库，可以读但不能写**，写入返回
  `xengine: collection has an index this implementation cannot maintain`。跳表索引与
  向量索引都维护得了，所以这条现在只会被将来出现的新索引种类触发。跳过维护它的后果
  不落在那条索引自己身上：它的节点仍指着已被删掉的文档，而那个数据块地址会被下一篇
  新文档复用，别的程序拿它去检索会返回**另一篇**文档。用整库重建过一遍就能写
- **一次提交在写日志时失败后，这个库实例作废**（`ErrBroken`），要关掉重开。写到一半的
  日志落没落盘说不准，内存里的文件头却只能按没提交算；继续用下去，下一次写进日志的文件头
  会与盘上那次提交对不上。所以作废之后实例不再往日志写任何东西，别的事务回滚也不去归还
  新扩的页（那几页宁可泄漏），重开会从文件读回干净的那份。还没开始写日志就失败的提交——
  比如两个事务并发各建一个集合、后提交的那个才发现集合表满了——只让它自己失败，文件头
  还原到提交之前，实例照常能用。
- **没有跨集合的一致性快照**：一个事务访问两个集合时，两个快照的读版本可能不同。
- **库级的 `Query()` 各自开一次性事务**，`ForUpdate` 的写锁只覆盖这次查询本身，
  不能用来跨越「查询与写回」做隔离。要那个隔离就用 `tx.Collection(name).Query()`，
  见[查出来再改回去](#查出来再改回去)。
- **改与删一个不存在的集合返回 0，不报错**。那是「没有任何一篇文档被改到」，
  与「改法本身有问题」是两回事；把它当错误会让按条件批改的空操作变成失败。
  写错集合名因此不会当场暴露。
- **`_id` 不能是 `null`、最小值或最大值**。后两个是跳表两端哨兵节点的键，拿它们
  去查会正好命中哨兵：读会照着哨兵那个空数据块解出「文档损坏」，删则会把哨兵本身
  摘掉、让整条索引没有终点。这三种值在写入、查找、删除三条路上都被拒。
- **索引表达式在建索引时就要解析得动**，方法名与形参个数也在解析期查表核对。
  一条求不出值的取键表达式若写进了集合页，那条索引每一次插入都要拿它求键，
  集合从此一条也插不进去，而报出来的错看起来像是这次插入的毛病。
- **`Close` 之后一切操作返回 `ErrClosed`**，`Close` 本身幂等。关闭之前就开着的事务
  和没走完的遍历也一样：下一步就拿到 `ErrClosed`，哪怕要读的页还在缓存里。只读打开的库关闭时
  不做检查点——它一个字节都不写。
- **向量索引的图形状是可复现的**。每个节点的层数若用无种子的随机流掷，同一批数据
  两次建出来的文件都不一样；这里沿用跳表那条固定种子的流，同一串操作永远建出同一
  张图。图的形状只影响近似检索的召回率，不影响哪条记录算不算命中。
- **向量检索还没接进查询构建器**：`Where` 那套条件与 `TopKNear` 目前不能组合，
  向量检索要单独调。
- **加密只挡得住随手一看，挡不住有心人**。方案是格式定死的，为了与同格式的库逐字节
  互读不作改动：每页用 **AES-ECB** 逐块加密，相同的明文块总是得到相同的密文块，
  文件里哪些块内容一样一眼可见（空白区、重复的文档片段都会露出轮廓）；密钥由
  **PBKDF2-HMAC-SHA1 只迭代 1000 次**派生，弱口令挡不住离线穷举；页上**没有完整性
  校验**，能写文件的人可以篡改某一页，或拿同一文件旧版本的整页换回去（重放）——
  后者解密完全正常，打开时不会报任何错。要防这些，在文件之外加一层：加密文件系统、
  带认证的备份、足够长的口令。
- **SQL 里没有 `SELECT DISTINCT`**：这个方言里没有这个关键字。去重要么用表达式方法
  `DISTINCT(...)`，要么用 `GroupBy`。

---

## 内部结构

二十一个内部包分成七层，公开的 `xdoc` 架在最上面。**每个包只 import 比它低的层**，
同层之间互不引用——所以这张表也是一张依赖图：改动一层只会波及它上面的。

```
7  xdoc       公开接口

6  xengine    文档级增删改、自增主键、索引同步、DDL
   xquery     查询优化与执行

5  xtx        事务：快照、四层查页、提交、回滚、检查点
   xsort      外部归并排序（输入超过内存预算时落盘归并）

4  xdisk      按偏移随机读写的存储（文件 / 内存）
   xsql       SQL 方言的词法与语法

3  xstore     文档分块存储、跳表索引、向量索引
   xbexpr     查询表达式：词法、语法、求值、内置方法
   xcrypt     数据文件加密
   xwal       预写日志的内存索引
   xsysfile   `$file` 虚拟集合：JSON / CSV 的读写

2  xpage      页头、页内段分配、四类页布局
   xmap       Go 结构体 ↔ 文档
   xvector    向量索引用到的度量与取值规则
   xjson      文档值 ↔ JSON 文本（含扩展记法）

1  xbson      文档模型、文档编码、索引键编码、跨类型全序

0  xbin       二进制原语：十进制、GUID、时间刻度、严格 UTF-8
   xcoll      字符串比较规则
   xerr       数据文件层的错误码体系
   xfmt       区域数据快照、数字与日期的解析和格式化
   xlock      跨进程库锁（只有共享模式用得上）
```

两处值得单说，因为按名字猜会猜反：

- **`xengine` 与 `xquery` 是同层的兄弟**，不是一个套着另一个。两边都架在
  `xtx` + `xstore` 上，互不 import——写这条路走 `xengine`，读这条路走 `xquery`。
- **`xbexpr` 不认识事务**。它只吃一篇文档和一组参数，够不着页、快照与索引，
  所以它在 `xtx` 的**下面**而不是上面。`xdoc.Eval` 能脱离库单独求值，
  正是因为这一层什么状态都不持有。

### 页

固定 8192 字节。一页的形状：

```
偏移 0        32                                              8192
+------------+----------------------+--------+----------------+
| 页头 32 字节 | 内容区（段从低地址长） |  空闲  | 槽表（从高向低长）|
+------------+----------------------+--------+----------------+
```

段在页内会因整理碎片而移动，但**槽号永不改变**——外部引用用 `(页号, 槽号)`
而不是字节偏移。

页头字段之间的一致性检查是**唯一的完整性防线**：这个格式没有页级校验和，位翻转、
撕裂写、错位写入都不会被发现。检查在每一页**从磁盘读进来时**跑一次，
挡住的价值不在于早点报错，而在于避免损坏扩散——一页不自洽的页拿去插入新段时，
新段会按错误的可用位置压在有效段上。

### 四层查页与版本号

四层查页：本事务已有的 → 本事务写进日志的 → 别人已提交且不晚于本快照版本的 →
数据文件。版本号是**全局提交序号**，一次提交让它加一，该事务写的所有页登记在这
同一个号上——于是一个读事务拿着自己开始时的号，就能看到那一刻的整个数据库。

### 校验

这个文件格式是公开的，读写它的不止一个程序。兼容性因此不是「应该没问题」，而是
逐字节验证过的：40 组检查与 177 项引擎层语义差分，在竞态检测器下通过且零数据
竞争。验证程序与它依赖的素材都不随本库分发。最硬的三条：

1. 新建的空库，页 0 的 8192 字节与格式规定的**完全一致**，两处除外：一是比较规则
   那两个字段（区域号与选项），那是建库时的选择，不同的程序有不同的默认值；
   二是偏移 32 起那 27 字节的格式标识，见[格式标识](#格式标识)。
2. 按同一格式写成的程序能打开本库产出的文件，读出全部文档、走二级索引查询命中、
   并追加写入。
3. 它还能在这个文件上跑完**整库重建**——那会走遍每个集合、每条索引、每篇文档并
   重建整个文件。结构上任何一处不自洽，重建都跑不完。

后两条是**把格式标识对齐之后**测的：那一串不一致时，对方在读第一页就以「不是本格式
的文件」退出，压根走不到结构这一层。所以它们验的是结构，不是开箱即用的互开。

#### 格式标识

页 0 偏移 32 起有 27 个字节是格式标识，本库写的是：

```
** This is a XDoc file **\x00\x00
```

25 个字符，右侧补两个零铺满槽位。补出来的零**也参与比对**，所以这 27 个字节里没有
一个是不受校验的。

这是格式里唯一一处带着库名的地方，后果是硬的：打开时的判据就是
`bytes.Equal` 这一串，没有任何宽容度。标识对不上的文件报

```
xpage: corrupt page: not a database file (bad magic)
```

而且 `IsCorrupt(err)` 为**真**——从调用方看，一个内容完好、只是标识不同的文件，
与一个真损坏的文件长得一模一样。反过来也一样：本库产出的文件交给一个按别的标识
去认的程序，它同样在读第一页就退出。

改法只有一个点：`internal/xpage/header.go` 里的 `magicText`。槽位长度 `MagicSize`
是由页布局推出来的，不跟着这个串走，所以换一串不会挪动它后面的版本号字段；
换成超长的串编译期就会停下（那里有一个长度为 `MagicSize - len(magicText)` 的数组）。

要与某个程序真正互开数据文件，就把这个常量改成它认的那一串重新编译——除此之外
整个格式一个字节都不用动。

### 边界情形上的选择

下面这些位置，宽松的做法会毁数据或让程序崩掉，本库选另一条路。**都不改变文件的
字节格式**，写出的文件照样互通。

| 位置 | 本库的做法 |
|---|---|
| NaN / 无穷参与比较 | NaN 最小、+∞ 最大的确定次序，不报错 |
| 文档之间比较 | **刻意不反对称**（只用左边的字段名去右边取值），键序用插入序；理由见 `internal/xbson/compare.go` |
| 跨类型数值比较 | 按数学值精确比，不转十进制丢精度 |
| 崩溃恢复的事务号 | 取最大值，且只增不减 |
| 提交 | 默认等待日志落盘 |
| 只读打开 | 一个字节都不改，残页只在内存里当它不存在 |
| 集合锁互等 | 等待图检测，被选中的一方回滚重试 |
| JSON 解析畸形输入 | 一律报错，不崩溃也不毁数据（一个值之后的尾随内容不算畸形，按这套记法的定义不看） |

每一处的具体理由写在对应的代码注释里。举一个：让比较函数在某些取值上失败，
等于让一条含 NaN 的文档把整个集合变成既查不动也删不掉——删除要先查找，
而查找会经过同一个比较。

### 几处容易踩的行为

下面这些都是有意的，但从调用方看不出来，写错了也不报错——所以集中记在这里，
免得下一个人在踩到时才发现。

| 位置 | 行为 |
|---|---|
| 主键类型推断 | `int8`/`int16`/`int32`/`uint8`/`uint16` 发 32 位自增，`int`/`int64`/`uint`/`uint32`/`uint64` 发 64 位自增，`Guid` 与 `ObjectID` 各自成档，**其余一律给 `ObjectID`**（`typed.go` 里的 `autoIDOf`） |
| `AutoIDNone` | 表示「别自动生成」：没带主键的文档会被**拒绝**，而不是替它补一个 |
| 嵌套深度 | 默认 20 层，**指针和接口每剥一级各占一层**。这一层不能省：`type P *P` 这种自指类型没有结构体层兜底，不计层就是无限递归 |
| 正则 | `regexp.Regexp` 只存模式串（`r.String()`），选项位既不单独存也不单独读——Go 的 `regexp` 没有可取出的选项位，写侧无解 |
| `url.URL` | 不分绝对与相对，一律存 `u.String()`，读回来走 `url.Parse` |
| `time.Duration` | 按**百纳秒**存成 64 位整数，与 Go 的纳秒差一百倍 |
| 文档字面量与参数绑定 | `Doc` / `Val` / `Param` 收 Go 原生值时过的是**默认映射器**，于是受 `TrimStrings` 与 `EmptyStringToNull` 影响；要跟着库自己的映射器走，用 `DB.Marshal` |
| 新建库的比较规则 | 「不依赖区域 + 按 UTF-16 码元」。文件头声称哪条规则，索引就是按哪条排出来的，两者对不上会让来读的程序在二分查找里**静默漏记录**；码元序不依赖任何国际化库的版本，所以拿它做默认 |

还有一处是平台定的：Go 的 goroutine 没有稳定身份，所以可重入的集合锁按事务句柄认，
不按执行流认——同一个事务在哪个 goroutine 上用都算同一个持有者。

### 日期带着一个时区标记，它决定读出来是几点

这个格式里的日期是「墙上刻度 + 一个标记」，本库存的是「绝对时刻 + 时区」。两种表示
等价，但有三件事必须一起看，少一件就对不上：

- **取时分秒取的是墙上读数。** 同一个时刻，`NOW()` 读本地的几点，`NOW_UTC()` 读世界时
  的几点，两个数差一个时区偏移。
- **比较先归到同一个绝对时刻。** 所以上一条里那两个值仍然相等，`NOW() = NOW_UTC()` 为真。
- **标记有三种，第三种最容易踩。** 一段不带时区的文本解析出来既不是本地也不是世界时，
  而是「没说清」。把它转成本地时，它**被当成世界时**换算（墙上读数往前挪一个偏移）；
  把它转成世界时，它**又被当成本地**。同一个值在两个方向上被解释成两种东西：

  ```
  HOUR(DATETIME("2020-01-15T10:30:00"))            → 10
  HOUR(TO_LOCAL(DATETIME("2020-01-15T10:30:00")))  → 18   ← 当成了世界时
  HOUR(TO_UTC(DATETIME("2020-01-15T10:30:00")))    → 2    ← 又当成了本地
  ```

解析时间文本一律落到本地：写了时区的换算过去，没写的按本地读。所以
`DATETIME("2020-01-15T10:30:00Z") = DATETIME("2020-01-15T10:30:00")` 在东八区是**假**。
换算之后越出可表示的年份时，整个解析失败交出 `null`，不截断也不报错——
`DATETIME("9999-12-31T23:59:59Z")` 在东八区就是这样。

还有一处只在很老的日期上露头：时区偏移永远取整到分钟。时区库里 1900 年前用的是
「地方平时」，带着秒（上海是 +8:05:43），那几秒被抹掉，于是
`SECOND(DATETIME("0001-01-01T00:00:00Z"))` 是 0 而不是 43。

### 十进制上几处只有看字节才发现的行为

`%` 两边都是十进制数时走的不是通用算法，是 x64 上一条 128÷96 的长除法。
每一步拿当前窗口的高 32 位字去试商，试商顶到 2^32 时硬件除法指令的商装不下，
CPU 抛出除法异常，运行时把它包成算术溢出——**哪怕余数明明放得下**：

```
79228162514264337593543950335 % 1.0000000000000000000000000001
```

数学上的答案是 `0.0771837485735662406456049673`，而这里**报错**。保留这个报错，是因为
换成给数的代价更大：同一条查询在两处一个给数一个报错，没人会想到是取模实现的差异，
只会以为是数据的问题。判据、它的边界（除数 64 位以下不抛、被除数 2^95−1 抛而 2^95
不抛）以及验证方式写在 `decModOverflows` 的注释里。这条行为绑在 x86/x64 且硬件内建
开着的前提上；关掉之后不抛，但给出的是一个错值。

**零的符号位同样是格式的一部分**，规则比看上去碎。十进制的零带一个符号位，它在文本上
完全看不见——打印不给负零加负号，JSON 里都是 `"0.0"`——但它要落盘（16 字节里 flags 的
第 31 位），所以只比对打印结果的对拍照不到它。每个算符的规则都不同：

- **乘、除**：符号是两边符号的异或，无条件，不看结果是不是零。`0.0 × -1` 是负零。
- **取模**：符号跟被除数，与除数无关。`-3 % 3` 是负零，`3 % -3` 不是。
- **加、减**：取决于两边的小数位数谁大、以及待放大的那一侧是不是零。可观察的后果
  是 `1.500 - 1.5` 给负零，而 `1.5 - 1.500` 给正零。

零结果上还有两处与符号无关、同样只有看字节才发现的分岔，都在乘法里：走不上
「两个尾数都放得进 32 位」那条快路径时，零结果的小数位数会连同符号一起被清掉
（`0.0 × 2.5` 是 `0.0`，而 `0.0 × 79228162514264337593543950335` 是 `0`）；位数
之和大到要缩掉 19 位以上时它干脆不算，直接交回全零。
