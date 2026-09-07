package xdoc

import (
	"context"
	"fmt"
	"iter"
	"reflect"

	"github.com/xmapst/xdoc/internal/xengine"
)

// TypedCollection 是收发 T 而不是 *Document 的集合句柄。
//
// 它是 [Collection] 外面的一层：编解码走库自己那个映射器，其余行为完全一致。
// 形状与 T 不同的查询（比如分组投影）用 [TypedQuery.Raw] 拿回产出文档的构建器。
type TypedCollection[T any] struct {
	c *Collection
}

// Typed 取一个收发 T 的集合句柄。
//
// 主键生成方式按 T 的主键字段类型挑（见 [autoIDOf]）：ID int64 就发整数，
// ID Guid 就发 GUID。T 上没有主键字段时沿用库级默认。
func (db *DB) Typed[T any](name string) *TypedCollection[T] {
	c := db.Collection(name)
	if a, ok := autoIDOf(db.mapper, reflect.TypeFor[T]()); ok {
		c = c.WithAutoID(a)
	}
	return &TypedCollection[T]{c: c}
}

// Decode 把一篇文档解成 T，类型从实参来。
//
// 它用的是这个库的映射器（[WithMapper] 换过的那个），而包级的
// [UnmarshalAs] 用默认那套——事务、分组投影、Explain 交出来的都是 *Document，
// 把它们解回结构体要走这里，否则两条路的规则可能对不上。
func (db *DB) Decode[T any](d *Document) (T, error) {
	if d == nil {
		var zero T
		return zero, errNilDocument
	}
	return db.mapper.UnmarshalAs[T](DocValue(d))
}

// autoIDOf 按 T 的主键字段类型推出该用哪种自动生成方式。
//
// 第二个返回值为 false 表示推不出来——没有主键字段，或者字段上标了 noauto，
// 两种都该沿用库级默认而不是在这里替调用方决定。
//
// 指针先剥到底：ID *int64 与 ID int64 该发同一种。整数按宽度分两档，
// 认不出的类型一律给 ObjectID。
//
// 它是包内的私有推断而不是 [Mapper] 上的公开方法：这段判断属于"集合怎么建"，
// 对外只留 [TypedCollection.AutoID] 一个出口——挑中了哪一档看得见，怎么挑的看不见。
func autoIDOf(m *Mapper, t reflect.Type) (AutoID, bool) {
	for f := range m.Fields(t) {
		if !f.IsID {
			continue
		}
		if !f.AutoID {
			return AutoIDNone, false
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft {
		case reflect.TypeFor[Guid]():
			return AutoIDGUID, true
		case reflect.TypeFor[ObjectID]():
			return AutoIDObjectID, true
		}
		switch ft.Kind() {
		case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint8, reflect.Uint16:
			return AutoIDInt32, true
		case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
			return AutoIDInt64, true
		default:
			return AutoIDObjectID, true
		}
	}
	return AutoIDNone, false
}

// Raw 返回底下那个收发 *Document 的句柄。
func (t *TypedCollection[T]) Raw() *Collection { return t.c }

// Name 返回集合名。
func (t *TypedCollection[T]) Name() string { return t.c.Name() }

// AutoID 返回这个句柄用的主键生成方式。
func (t *TypedCollection[T]) AutoID() AutoID { return t.c.AutoID() }

// Include 返回一个之后的读都展开 expr 所指引用的新句柄，原句柄不受影响。
func (t *TypedCollection[T]) Include(expr string) *TypedCollection[T] {
	n := *t
	n.c = t.c.Include(expr)
	return &n
}

// WithAutoID 返回一个改用 a 生成主键的新句柄，覆盖按 T 推出来的那一档。
func (t *TypedCollection[T]) WithAutoID(a AutoID) *TypedCollection[T] {
	return &TypedCollection[T]{c: t.c.WithAutoID(a)}
}

// Insert 写入若干个值，返回写进去的个数。整批一个事务。
//
// 生成的主键会**按指针写回**传进来的对象（Typed[*User]）；按值传则写不回去，
// Go 里改不到调用方手上那份副本。要拿主键就用 [TypedCollection.InsertOne]。
func (t *TypedCollection[T]) Insert(ctx context.Context, vs ...T) (int, error) {
	docs, err := t.encodeAll(vs)
	if err != nil {
		return 0, err
	}
	n, err := t.c.Insert(ctx, docs...)
	if err != nil {
		return n, err
	}

	for i, v := range vs {
		if i >= len(docs) {
			break
		}
		t.writeBackID(v, docs[i].Get(xengine.IDField))
	}
	return n, nil
}

// InsertOne 写入一个值并返回它的主键。
func (t *TypedCollection[T]) InsertOne(ctx context.Context, v T) (*Value, error) {
	d, err := t.c.db.mapper.MarshalDocument(v)
	if err != nil {
		return nil, err
	}
	id, err := t.c.InsertOne(ctx, d)
	if err != nil {
		return nil, err
	}
	t.writeBackID(v, id)
	return id, nil
}

// writeBackID 把现发的主键写回调用方的对象，只在 T 是非 nil 指针时有意义。
//
// **写不回去不算失败**：那时插入已经提交了，报错会让调用方以为没写进去而重试
// 一遍，于是多出一条记录。
func (t *TypedCollection[T]) writeBackID(v T, id *Value) {
	if id == nil || id.IsNull() {
		return
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	_ = t.c.db.mapper.SetPrimaryKey(v, id)
}

// Update 按主键整篇改写，返回真的改掉了几条。
func (t *TypedCollection[T]) Update(ctx context.Context, vs ...T) (int, error) {
	docs, err := t.encodeAll(vs)
	if err != nil {
		return 0, err
	}
	return t.c.Update(ctx, docs...)
}

// Upsert 按主键存在与否分别插入或改写。
func (t *TypedCollection[T]) Upsert(ctx context.Context, vs ...T) (UpsertResult, error) {
	docs, err := t.encodeAll(vs)
	if err != nil {
		return UpsertResult{}, err
	}
	return t.c.Upsert(ctx, docs...)
}

// decodeSeq 把一串文档边遍历边解成 T。
//
// 解不动就地停下并交出错误，不跳过那一篇——跳过等于让调用方拿到一份"少了几条
// 但看起来正常"的结果。
func (db *DB) decodeSeq[T any](src iter.Seq2[*Document, error], what, coll string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for d, err := range src {
			var out T
			if err != nil {
				yield(out, err)
				return
			}
			if err := db.mapper.Unmarshal(DocValue(d), &out); err != nil {
				yield(out, fmt.Errorf("xdoc: decode %s from %q: %w", what, coll, err))
				return
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}

// decodeOne 把一篇文档解成 T，错误里带上是哪个集合的哪一篇。
func (db *DB) decodeOne[T any](d *Document, what, coll string) (T, error) {
	var out T
	if err := db.mapper.Unmarshal(DocValue(d), &out); err != nil {
		return out, fmt.Errorf("xdoc: decode %s from %q: %w", what, coll, err)
	}
	return out, nil
}

// collect 把一个迭代器收成切片，中途出错就整个放弃。
func collect[T any](src iter.Seq2[T, error]) ([]T, error) {
	var out []T
	for v, err := range src {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Delete 按主键删除，静默跳过不存在的主键。
func (t *TypedCollection[T]) Delete(ctx context.Context, ids ...*Value) (int, error) {
	return t.c.Delete(ctx, ids...)
}

// FindByID 按主键取一个值，找不到返回 [ErrNotFound]。
func (t *TypedCollection[T]) FindByID(ctx context.Context, id *Value) (T, error) {
	d, err := t.c.FindByID(ctx, id)
	if err != nil {
		var zero T
		return zero, err
	}
	return t.c.db.decodeOne[T](d, id.String(), t.c.name)
}

// Exists 报告这个主键在不在。
func (t *TypedCollection[T]) Exists(ctx context.Context, id *Value) (bool, error) {
	return t.c.Exists(ctx, id)
}

// Count 数一遍集合里有多少篇文档，要走一遍索引。
func (t *TypedCollection[T]) Count(ctx context.Context) (int, error) { return t.c.Count(ctx) }

// All 按主键顺序遍历，边走边解成 T。
func (t *TypedCollection[T]) All(ctx context.Context, order Order) iter.Seq2[T, error] {
	return t.c.db.decodeSeq[T](t.c.All(ctx, order), "a document", t.c.name)
}

// Slice 把整个集合按主键顺序收成一个切片。
func (t *TypedCollection[T]) Slice(ctx context.Context, order Order) ([]T, error) {
	return collect(t.All(ctx, order))
}

// EnsureIndex 保证一条索引存在，语义同 [Collection.EnsureIndex]。
func (t *TypedCollection[T]) EnsureIndex(ctx context.Context, name, expr string, unique bool) (bool, error) {
	return t.c.EnsureIndex(ctx, name, expr, unique)
}

// EnsureIndexOn 与上面一样，但索引名从表达式推出来。
func (t *TypedCollection[T]) EnsureIndexOn(ctx context.Context, expr string, unique bool) (bool, error) {
	return t.c.EnsureIndexOn(ctx, expr, unique)
}

// DropIndex 删掉一条索引，返回它原先在不在。
func (t *TypedCollection[T]) DropIndex(ctx context.Context, name string) (bool, error) {
	return t.c.DropIndex(ctx, name)
}

// Drop 删掉整个集合连同它的所有索引。
func (t *TypedCollection[T]) Drop(ctx context.Context) (bool, error) { return t.c.Drop(ctx) }

// encodeAll 把一批值编成文档，错误里带上是第几个。
//
// 任何一个编不出来就整批放弃，不写进去一半。
func (t *TypedCollection[T]) encodeAll(vs []T) ([]*Document, error) {
	docs := make([]*Document, 0, len(vs))
	for i, v := range vs {
		d, err := t.c.db.mapper.MarshalDocument(v)
		if err != nil {
			return nil, fmt.Errorf("xdoc: encode item %d for %q: %w", i, t.c.name, err)
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// IDField 是主键字段名。
const IDField = xengine.IDField

// Query 建一个产出 T 的查询构建器。
func (t *TypedCollection[T]) Query() *TypedQuery[T] {
	return &TypedQuery[T]{t: t, b: t.c.Query()}
}

// TypedQuery 是产出 T 的查询构建器，与 [QueryBuilder] 一样**不可变**：
// 每一步返回一份新的，从同一个起点分出的两支互不影响。
type TypedQuery[T any] struct {
	t *TypedCollection[T]
	b *QueryBuilder
}

// 以下各步与 [QueryBuilder] 上的同名方法语义一致，只是产出类型是 T。
func (q *TypedQuery[T]) Where(expr string) *TypedQuery[T] { return q.wrap(q.b.Where(expr)) }

// Param 绑定一个参数值。
func (q *TypedQuery[T]) Param(name string, v any) *TypedQuery[T] {
	return q.wrap(q.b.Param(name, v))
}

// OrderBy 设置第一个排序键（升序）。
func (q *TypedQuery[T]) OrderBy(expr string) *TypedQuery[T] { return q.wrap(q.b.OrderBy(expr)) }

// OrderByDesc 设置第一个排序键（降序）。
func (q *TypedQuery[T]) OrderByDesc(expr string) *TypedQuery[T] {
	return q.wrap(q.b.OrderByDesc(expr))
}

// ThenBy 追加一个次级排序键（升序）。
func (q *TypedQuery[T]) ThenBy(expr string) *TypedQuery[T] { return q.wrap(q.b.ThenBy(expr)) }

// ThenByDesc 追加一个次级排序键（降序）。
func (q *TypedQuery[T]) ThenByDesc(expr string) *TypedQuery[T] {
	return q.wrap(q.b.ThenByDesc(expr))
}

// Include 展开一处引用。
func (q *TypedQuery[T]) Include(expr string) *TypedQuery[T] { return q.wrap(q.b.Include(expr)) }

// Skip 跳过前 n 条结果。
func (q *TypedQuery[T]) Skip(n int) *TypedQuery[T] { return q.wrap(q.b.Skip(n)) }

// Limit 最多取 n 条。
func (q *TypedQuery[T]) Limit(n int) *TypedQuery[T] { return q.wrap(q.b.Limit(n)) }

// ForUpdate 让这条查询取写锁。
func (q *TypedQuery[T]) ForUpdate() *TypedQuery[T] { return q.wrap(q.b.ForUpdate()) }

// Raw 返回底下那个产出 *Document 的构建器。
//
// 结果形状与 T 不同的查询（分组投影之类）从这里接出去，再用 [DB.Decode]
// 解成另一个结构体。
func (q *TypedQuery[T]) Raw() *QueryBuilder { return q.b }

// wrap 把底层构建器的新形态重新包成 TypedQuery，保持不可变语义。
func (q *TypedQuery[T]) wrap(b *QueryBuilder) *TypedQuery[T] {
	return &TypedQuery[T]{t: q.t, b: b}
}

// All 遍历查询结果，边走边解成 T。
func (q *TypedQuery[T]) All(ctx context.Context) iter.Seq2[T, error] {
	return q.t.c.db.decodeSeq[T](q.b.All(ctx), "a query result", q.t.c.name)
}

// Slice 把查询结果收成一个切片。
func (q *TypedQuery[T]) Slice(ctx context.Context) ([]T, error) {
	return collect(q.All(ctx))
}

// First 取第一条结果，没有则返回 [ErrNotFound]。
func (q *TypedQuery[T]) First(ctx context.Context) (T, error) {
	d, err := q.b.First(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	return q.t.c.db.decodeOne[T](d, "a query result", q.t.c.name)
}

// Count 数一遍匹配的条数，不解码。
func (q *TypedQuery[T]) Count(ctx context.Context) (int, error) { return q.b.Count(ctx) }

// Exists 报告有没有匹配的结果，取够一条就停。
func (q *TypedQuery[T]) Exists(ctx context.Context) (bool, error) { return q.b.Exists(ctx) }

// Explain 交出执行计划，形状见 [QueryBuilder.Explain]。
func (q *TypedQuery[T]) Explain(ctx context.Context) (*Document, error) { return q.b.Explain(ctx) }
