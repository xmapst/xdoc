package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xstore"
)

// Order 是遍历方向。
type Order = xstore.Order

const (
	// Asc 从小到大。
	Asc = xstore.Asc

	// Desc 从大到小。
	Desc = xstore.Desc
)

// Collection 是一个集合的句柄。
//
// 集合不需要预先创建，第一次写入时连同它的主键索引一起建出来。
//
// 句柄是值语义的：[Collection.Include] 与 [Collection.WithAutoID] 返回新句柄，
// 不改原来那个。可以被多个 goroutine 同时使用。
type Collection struct {
	db   *DB
	name string
	auto AutoID

	includes []string
}

// Name 返回集合名。
func (c *Collection) Name() string { return c.name }

// AutoID 返回这个句柄用的主键生成方式。
func (c *Collection) AutoID() AutoID { return c.auto }

// Include 让这个句柄之后的所有读都展开 expr 指的引用，[Collection.FindByID]
// 与 [Collection.All] 也算在内。
//
// 返回的是新句柄，原句柄不受影响。挂上它之后读走的是查询那条路
// （引用展开只在那里实现），所以主键点查会比不挂时慢一点。
func (c *Collection) Include(expr string) *Collection {
	n := *c
	n.includes = append(append([]string(nil), c.includes...), expr)
	return &n
}

// WithAutoID 返回一个改用 a 生成主键的新句柄，覆盖库级默认。
func (c *Collection) WithAutoID(a AutoID) *Collection {
	n := *c
	n.auto = a
	return &n
}

// Insert 写入若干篇文档，返回写进去的篇数。
//
// **整批一个事务**：任何一篇失败（比如撞唯一索引），整批都不生效。这不是保守
// ——一篇文档要写进数据页、还要在每条索引上各建一个节点，冲突是建到某条索引时
// 才发现的，那时数据块和前面几条索引的节点都已经落下去了。
//
// 没带主键的文档由 [Collection.AutoID] 那一档现发一个，并写回 docs 里那篇文档。
func (c *Collection) Insert(ctx context.Context, docs ...*Document) (int, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.db.engine.Insert(ctx, c.name, docs, c.auto)
}

// InsertOne 写入一篇文档并返回它的主键，主键是现发的时候尤其有用。
func (c *Collection) InsertOne(ctx context.Context, doc *Document) (*Value, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	if _, err := c.db.engine.Insert(ctx, c.name, []*Document{doc}, c.auto); err != nil {
		return nil, err
	}
	return doc.Get(xengine.IDField), nil
}

// Update 按主键整篇改写，返回**真的改掉了几条**。
//
// 找不到的既不算失败也不算更新。这个数和提交的条数不一致时，往往正是要查的地方。
func (c *Collection) Update(ctx context.Context, docs ...*Document) (int, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.db.engine.Update(ctx, c.name, docs)
}

// UpsertResult 分开报插入与更新的条数，调用方通常要区别对待这两者。
type UpsertResult struct {
	Inserted int
	Updated  int
}

// Upsert 按主键存在与否分别插入或改写。
//
// 主键重复该用它而不是接 [ErrDuplicateKey]：这里无竞态，还顺带告诉你插了几条
// 改了几条。
func (c *Collection) Upsert(ctx context.Context, docs ...*Document) (UpsertResult, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return UpsertResult{}, err
	}
	defer rel()
	n, err := c.db.engine.Upsert(ctx, c.name, docs, c.auto)
	if err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Inserted: n, Updated: len(docs) - n}, nil
}

// Delete 按主键删除，静默跳过不存在的主键，返回真的删掉的条数。
func (c *Collection) Delete(ctx context.Context, ids ...*Value) (int, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.db.engine.Delete(ctx, c.name, ids)
}

// FindByID 按主键取一篇文档，找不到返回 [ErrNotFound]。
//
// 不返回一篇空文档：那样调用方分不清"没有这条记录"和"这条记录的字段都是空的"，
// 而这两者在业务上往往要走完全不同的分支。
//
// 句柄上挂了 [Collection.Include] 时走查询那条路，好让引用能展开；
// 两条路的"找不到"都归一成同一个错误。
func (c *Collection) FindByID(ctx context.Context, id *Value) (*Document, error) {
	if len(c.includes) > 0 {
		d, err := c.Query().Where("$._id = @id").Param("id", id).First(ctx)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s in %q", ErrNotFound, id, c.name)
		}
		return d, err
	}
	rel, err := c.db.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	d, err := c.db.engine.Find(ctx, c.name, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: %s in %q", ErrNotFound, id, c.name)
	}
	return d, nil
}

// Exists 报告这个主键在不在，不把文档读出来交给调用方。
func (c *Collection) Exists(ctx context.Context, id *Value) (bool, error) {
	rel, err := c.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	d, err := c.db.engine.Find(ctx, c.name, id)
	return d != nil, err
}

// All 按主键顺序遍历整个集合。
//
// 遍历期间占着一个快照，中途 break 之后要让 for 循环正常退出，资源才会还回去
// ——用 Go 的 range-over-func 时这是自动的。
//
// 挂了 [Collection.Include] 时改走查询，用 ORDER BY 复现同样的主键顺序。
func (c *Collection) All(ctx context.Context, order Order) iter.Seq2[*Document, error] {
	if len(c.includes) > 0 {
		qb := c.Query()
		if order == Desc {
			return qb.OrderByDesc("$._id").All(ctx)
		}
		return qb.OrderBy("$._id").All(ctx)
	}

	return c.db.enterSeq(ctx, func() iter.Seq2[*Document, error] {
		return c.db.engine.Scan(ctx, c.name, order)
	})
}

// Count 数一遍集合里有多少篇文档。
//
// 它要走一遍索引，不是现成的计数。需要频繁读就自己维护一个。
func (c *Collection) Count(ctx context.Context) (int, error) {
	return c.Query().Count(ctx)
}

// EnsureIndex 保证一条名为 name、取键表达式为 expr 的索引存在，返回是不是新建的。
//
// **可以重复调**：已存在且表达式相同就什么也不做，所以"每次启动都保证索引在"是
// 正常写法。已存在但表达式不同会报错——那是两条不同的索引共用一个名字，
// 静默改掉它会让此前按旧表达式写进去的键全部失效，而查询照常返回，只是结果不全。
//
// 建在已有数据上会自动回填。取键表达式必须可索引，判据见 [Expr.Indexable]。
// 会摊开成多个键的表达式（$.tags[*]）上不能加唯一约束：一篇文档在这条索引里留下
// 的是一串键，"唯一"是指这串键整体唯一还是串里每个键唯一，说不清楚。
func (c *Collection) EnsureIndex(ctx context.Context, name, expr string, unique bool) (bool, error) {
	rel, err := c.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.db.engine.EnsureIndex(ctx, c.name, name, expr, unique)
}

// EnsureIndexOn 与 [Collection.EnsureIndex] 一样，但索引名从表达式推出来。
//
// 推法是归一之后删掉所有非 ASCII 字母数字：$.name → name，UPPER($.n) → UPPERn，
// $.a[*].b → MAPab（它归一成 MAP($.a[*]=>@.b)）。推法是格式的一部分，
// 所以同一条表达式在哪里推出来的都是同一个名字。
//
// 一个 ASCII 字母数字都不剩时（比如 $.名字）报错，要显式给名字。
func (c *Collection) EnsureIndexOn(ctx context.Context, expr string, unique bool) (bool, error) {
	name := xengine.DeriveIndexName(expr)
	if name == "" {
		return false, fmt.Errorf("xdoc: cannot derive an index name from %q, pass one explicitly", expr)
	}
	return c.EnsureIndex(ctx, name, expr, unique)
}

// DropIndex 删掉一条索引，返回它原先在不在。
//
// 主键索引恒存在，删不掉。
func (c *Collection) DropIndex(ctx context.Context, name string) (bool, error) {
	rel, err := c.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.db.engine.DropIndex(ctx, c.name, name)
}

// Drop 删掉整个集合连同它的所有索引，返回它原先在不在。
func (c *Collection) Drop(ctx context.Context) (bool, error) {
	ctx, box := c.db.notifyScope(ctx)
	defer box.flush()
	rel, err := c.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.db.engine.DropCollection(ctx, c.name)
}

// Doc 用交替的键值对拼一篇文档：Doc("a", 1, "b", "x")。
//
// 键必须是字符串，不是的那一对整个跳过；值走 [Val] 的自动转换。
// 落单的最后一个实参也会被忽略。
//
// 值用的是默认映射器，不是某个库的。要跟着库走（[WithMapper] 换过的那个），
// 用 [DB.MarshalDocument]。
func Doc(kv ...any) *Document {
	d := xbson.NewDocument()
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		d.Set(k, Val(kv[i+1]))
	}
	return d
}

// Val 把一个 Go 值转成字段值。
//
// 已经是 *Value 的原样返回。**转不动时静默返回 Null**，因为包级构造器没有地方
// 放这个错误——要错误用 [DB.Marshal] 或 [MarshalValue]。
//
// 用的是默认映射器：同一个值经 [DB.Marshal] 编出来的可以完全不同，
// 拿这里的结果去查一个按库的规则写进去的字段，查询照常返回，只是零行。
func Val(v any) *Value {
	if x, ok := v.(*Value); ok {
		return x
	}
	out, err := xmapMarshal(v)
	if err != nil {
		return Null()
	}
	return out
}

// Null 造一个空值。
func Null() *Value { return xbson.Null }

// Int32 造一个 32 位整数值。
func Int32(v int32) *Value { return xbson.Int32(v) }

// Int64 造一个 64 位整数值。
func Int64(v int64) *Value { return xbson.Int64(v) }

// Double 造一个双精度浮点值。
func Double(v float64) *Value { return xbson.Double(v) }

// String 造一个字符串值。
func String(v string) *Value { return xbson.String(v) }

// Bool 造一个布尔值。
func Bool(v bool) *Value { return xbson.Boolean(v) }

// Binary 造一个二进制值，切片不拷贝。
func Binary(v []byte) *Value { return xbson.Binary(v) }

// OID 造一个 [ObjectID] 值。
func OID(v ObjectID) *Value { return xbson.OID(v) }

// NewObjectID 生成一个新的 [ObjectID]。
func NewObjectID() ObjectID { return xbson.NewObjectID() }

// Arr 造一个数组值。
func Arr(vs ...*Value) *Value { return xbson.NewArray(vs...).Value() }

// DocValue 把一篇文档包成值。
func DocValue(d *Document) *Value { return d.Value() }
