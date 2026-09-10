package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xquery"
)

// Query 建一个查询构建器，句柄上挂着的 Include 一并带过去。
func (c *Collection) Query() *QueryBuilder {
	b := &QueryBuilder{c: c, q: xquery.NewQuery()}
	for _, inc := range c.includes {
		b = b.Include(inc)
	}
	return b
}

// QueryBuilder 一步步拼出一条查询。
//
// 它是**不可变**的：每一步返回一份新的，从同一个起点分出的两支互不影响。
// 就地改再返回自己会让复用变成陷阱，而且两边都不报错。
//
// 各步的解析错误记在 err 上，留到执行时才交出来——构建期的方法没有 error 出口。
type QueryBuilder struct {
	c *Collection
	q *xquery.Query

	// tx 非空表示这条查询走调用方的事务；为空时执行期自开一个一次性事务。
	tx *Tx

	// err 是构建期第一个出错的地方。一旦非空，后面的步骤全部跳过，
	// 每一个执行方法都会把它交出来。
	err error
}

// clone 复制一份构建器，切片与参数文档都深拷。
//
// 浅拷会让两支共用底层数组，改一支就改了另一支——那正是不可变要挡的事。
func (b *QueryBuilder) clone() *QueryBuilder {
	q := *b.q
	q.Where = slices.Clone(b.q.Where)
	q.Includes = slices.Clone(b.q.Includes)
	q.OrderBy = slices.Clone(b.q.OrderBy)
	if b.q.Params != nil {
		q.Params = b.q.Params.Clone()
	}
	return &QueryBuilder{c: b.c, q: &q, tx: b.tx, err: b.err}
}

// with 是每一步的共同外壳：先复制，再把改动作用在副本上。
//
// 已经出过错就直接返回，不再往下走：后面的步骤多半依赖前面的结果，
// 接着跑只会产生第二个更难懂的错误。
func (b *QueryBuilder) with(fn func(*xquery.Query) error) *QueryBuilder {
	n := b.clone()
	if n.err != nil {
		return n
	}
	n.err = fn(n.q)
	return n
}

// parse 解析一段表达式，错误里带上它是哪个子句的。
func (b *QueryBuilder) parse(what, expr string) (xbexpr.Node, error) {
	n, err := xbexpr.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("xdoc: %s expression %q: %w", what, expr, err)
	}
	return n, nil
}

// Where 追加一个条件，多次调用之间是「并且」。
//
// 收的必须是谓词（求值为布尔），一个裸路径不是谓词，会被拒绝。
func (b *QueryBuilder) Where(expr string) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("where", expr)
		if err != nil {
			return err
		}
		return q.AddWhere(n)
	})
}

// Param 绑定一个参数值，表达式里用 @name 引用。
//
// **不要把值拼进表达式串**：拼串要自己处理引号与转义，漏一处就是一条能被输入
// 内容改变含义的查询。参数走的是值这条路，不经过解析。
//
// Go 值用这个库的映射器编码（[WithMapper] 换过的那个）；已经是 *Value 的直接用，
// 代价是它绕过了这一层——那种值若是用默认映射器造的，规则可能与库里存的对不上，
// 查询照常返回，只是零行。
//
// 转不出来的值记成构建期错误而不是静默变成 Null：一个 Null 参数照样参与比较，
// 给出的是一份不对的结果。
func (b *QueryBuilder) Param(name string, v any) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		if q.Params == nil {
			q.Params = xbson.NewDocument()
		}
		if x, ok := v.(*Value); ok {
			q.Params.Set(name, x)
			return nil
		}
		val, err := b.c.db.Marshal(v)
		if err != nil {
			return fmt.Errorf("xdoc: query parameter %q: %w", name, err)
		}
		q.Params.Set(name, val)
		return nil
	})
}

// Select 设置投影表达式，决定每条结果长什么样。
func (b *QueryBuilder) Select(expr string) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("select", expr)
		if err != nil {
			return err
		}
		q.Select = n
		return nil
	})
}

// OrderBy 设置**第一个**排序键（升序），覆盖此前设过的排序。
// 追加次级键用 [QueryBuilder.ThenBy]。
func (b *QueryBuilder) OrderBy(expr string) *QueryBuilder { return b.order(expr, Asc, false) }

// OrderByDesc 设置第一个排序键（降序），覆盖此前设过的排序。
func (b *QueryBuilder) OrderByDesc(expr string) *QueryBuilder { return b.order(expr, Desc, false) }

// ThenBy 追加一个次级排序键（升序）。方向不从前一个键继承。
func (b *QueryBuilder) ThenBy(expr string) *QueryBuilder { return b.order(expr, Asc, true) }

// ThenByDesc 追加一个次级排序键（降序）。
func (b *QueryBuilder) ThenByDesc(expr string) *QueryBuilder { return b.order(expr, Desc, true) }

// order 是四个排序方法的共同实现：then 决定是覆盖还是追加。
func (b *QueryBuilder) order(expr string, o Order, then bool) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("order by", expr)
		if err != nil {
			return err
		}
		if then {
			return q.ThenBy(n, o)
		}
		return q.SetOrderBy(n, o)
	})
}

// GroupBy 按表达式分组，分组后的投影里用 @key 取组键。
func (b *QueryBuilder) GroupBy(expr string) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("group by", expr)
		if err != nil {
			return err
		}
		return q.SetGroupBy(n)
	})
}

// Having 在分组之后筛掉不满足条件的组。
func (b *QueryBuilder) Having(expr string) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("having", expr)
		if err != nil {
			return err
		}
		return q.SetHaving(n)
	})
}

// Include 让 expr 指的引用在结果里展开成被引的那篇文档。
func (b *QueryBuilder) Include(expr string) *QueryBuilder {
	return b.with(func(q *xquery.Query) error {
		n, err := b.parse("include", expr)
		if err != nil {
			return err
		}
		return q.AddInclude(n)
	})
}

// Skip 跳过前 n 条结果。
func (b *QueryBuilder) Skip(n int) *QueryBuilder {
	return b.with(func(q *xquery.Query) error { q.Skip(n); return nil })
}

// Limit 最多取 n 条。
//
// 它是真惰性的：取够就停，底下不会扫完整个集合。
func (b *QueryBuilder) Limit(n int) *QueryBuilder {
	return b.with(func(q *xquery.Query) error { q.Take(n); return nil })
}

// ForUpdate 让这条查询取写锁，与并发的写串行开。
//
// **它只覆盖这次查询本身**：库级查询各自开一次性事务，结果一交出来锁就还了。
// 拿它做「查出来再改回去」挡不住别人在你读完与写回之间改同一批文档。
// 要那个隔离，用 [TxCollection.Query]。
func (b *QueryBuilder) ForUpdate() *QueryBuilder {
	return b.with(func(q *xquery.Query) error { q.ForUpdate = true; return nil })
}

// primaryOnly 让这条查询只走主键索引，见 [xquery.Query.PrimaryOnly]。
func (b *QueryBuilder) primaryOnly() *QueryBuilder {
	return b.with(func(q *xquery.Query) error { q.PrimaryOnly = true; return nil })
}

// All 遍历查询结果。
//
// 遍历期间占着一个快照，中途 break 之后要让 for 循环正常退出，资源才会还回去。
func (b *QueryBuilder) All(ctx context.Context) iter.Seq2[*Document, error] {
	if b.err != nil {
		return seqErr[*Document](b.err)
	}

	return b.c.db.enterSeq(ctx, func() iter.Seq2[*Document, error] {
		return b.all(ctx)
	})
}

// all 是遍历的实现：必要时自开一次性事务，并把这次遍历登记进 $open_cursors。
//
// 计时在每次交出一行前停、拿回控制权后再开，所以 $open_cursors 里的耗时**不含**
// 调用方处理每行的时间。
func (b *QueryBuilder) all(ctx context.Context) iter.Seq2[*Document, error] {
	return func(yield func(*Document, error) bool) {
		ex, err := b.c.db.executor()
		if err != nil {
			yield(nil, err)
			return
		}

		t := b.tx
		if t == nil {
			own, err := b.c.db.BeginTrans(ctx)
			if err != nil {
				yield(nil, err)
				return
			}

			defer func() { _ = own.Rollback() }()
			t = own
		}

		cur, done := b.track(t)
		defer done()

		for d, err := range b.run(ctx, ex, t) {
			cur.stop()
			cur.fetched.Add(1)
			ok := yield(d, err)
			cur.start()
			if !ok || err != nil {
				return
			}
		}
	}
}

// run 按集合是普通集合还是 $ 打头的系统虚拟集合，分派到两条执行路径。
func (b *QueryBuilder) run(ctx context.Context, ex *xquery.Executor, t *Tx) iter.Seq2[*Document, error] {
	if isSystemName(b.c.name) {
		src, err := b.c.db.sysSource(ctx, b.c.name, t)
		if err != nil {
			return seqErr[*Document](err)
		}
		return ex.QuerySource(ctx, src, b.q)
	}
	return ex.QueryIn(ctx, t.tx, b.c.name, b.q)
}

// Slice 把查询结果收成一个切片，中途出错就整个放弃。
func (b *QueryBuilder) Slice(ctx context.Context) ([]*Document, error) {
	var out []*Document
	for d, err := range b.All(ctx) {
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// First 取第一条结果，没有则返回 [ErrNotFound]。
//
// 自动加了 LIMIT 1，所以底下取够一条就停。
func (b *QueryBuilder) First(ctx context.Context) (*Document, error) {
	for d, err := range b.Limit(1).All(ctx) {
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	return nil, fmt.Errorf("%w: no document matched in %q", ErrNotFound, b.c.name)
}

// Count 数一遍匹配的条数，不把文档解出来交给调用方。
func (b *QueryBuilder) Count(ctx context.Context) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	rel, err := b.c.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	ex, err := b.c.db.executor()
	if err != nil {
		return 0, err
	}
	if isSystemName(b.c.name) {
		src, err := b.c.db.sysSource(ctx, b.c.name, b.tx)
		if err != nil {
			return 0, err
		}
		return ex.CountSource(ctx, src, b.q)
	}
	if b.tx != nil {
		return ex.CountIn(ctx, b.tx.tx, b.c.name, b.q)
	}
	return ex.Count(ctx, b.c.name, b.q)
}

// Exists 报告有没有匹配的结果，取够一条就停。
func (b *QueryBuilder) Exists(ctx context.Context) (bool, error) {
	_, err := b.First(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Explain 交出执行计划：走了哪条索引、扫描区间多大、要不要额外排序。
//
// 计划里显示全表扫描而你以为它该走索引，多半是索引的取键表达式与查询里的写法
// 对不上——**匹配是按归一后的表达式源文本比的**，$.age 与 $["age"] 不互认。
//
// 计划文档的键名是格式的一部分，包括那个拼错的 snaphost。集合不存在时一行都不出。
func (b *QueryBuilder) Explain(ctx context.Context) (*Document, error) {
	if b.err != nil {
		return nil, b.err
	}
	rel, err := b.c.db.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	ex, err := b.c.db.executor()
	if err != nil {
		return nil, err
	}
	if isSystemName(b.c.name) {
		src, err := b.c.db.sysSource(ctx, b.c.name, b.tx)
		if err != nil {
			return nil, err
		}

		return ex.ExplainSource(ctx, b.c.name, src, b.q)
	}
	return ex.Explain(ctx, b.c.name, b.q)
}

// Offset 是 [QueryBuilder.Skip] 的别名。
func (b *QueryBuilder) Offset(n int) *QueryBuilder { return b.Skip(n) }

// FirstOrDefault 取第一条结果，没有则返回 (nil, nil) 而不是错误。
func (b *QueryBuilder) FirstOrDefault(ctx context.Context) (*Document, error) {
	d, err := b.First(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return d, err
}

// Single 取唯一一条结果：一条也没有或多于一条都报错。
func (b *QueryBuilder) Single(ctx context.Context) (*Document, error) {
	d, err := b.singleOf(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: no document matched in %q", ErrNotFound, b.c.name)
	}
	return d, nil
}

// SingleOrDefault 与 [QueryBuilder.Single] 一样，但一条也没有时返回 (nil, nil)。
func (b *QueryBuilder) SingleOrDefault(ctx context.Context) (*Document, error) {
	return b.singleOf(ctx)
}

// singleOf 取至多一条结果，多于一条报错。
//
// 取 LIMIT 2 而不是全取：判断"是不是只有一条"只需要看有没有第二条。
func (b *QueryBuilder) singleOf(ctx context.Context) (*Document, error) {
	var found *Document
	n := 0
	for d, err := range b.Limit(2).All(ctx) {
		if err != nil {
			return nil, err
		}
		n++
		if n > 1 {
			return nil, fmt.Errorf("xdoc: more than one document matched in %q", b.c.name)
		}
		found = d
	}
	return found, nil
}

// LongCount 与 [QueryBuilder.Count] 一样，只是返回 int64。
func (b *QueryBuilder) LongCount(ctx context.Context) (int64, error) {
	n, err := b.Count(ctx)
	return int64(n), err
}

// Into 把结果写进另一个集合，返回写了几篇，主键生成方式用库级默认。
func (b *QueryBuilder) Into(ctx context.Context, collection string) (int, error) {
	return b.IntoWithAutoID(ctx, collection, b.c.db.auto)
}

// IntoWithAutoID 把结果写进另一个集合，并指定源文档没带主键时现发哪一种。
//
// 已经在事务里就用那个事务，否则自开一个：读与写必须在同一个事务里，
// 否则写到一半失败会留下半份结果。
func (b *QueryBuilder) IntoWithAutoID(ctx context.Context, collection string, auto AutoID) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.tx != nil {
		return b.intoWithin(ctx, b.tx, collection, auto)
	}
	total := 0
	err := b.c.db.Transaction(ctx, func(t *Tx) error {
		nb := b.clone()
		nb.tx = t
		var err error
		total, err = nb.intoWithin(ctx, t, collection, auto)
		return err
	})
	return total, err
}

// intoWithin 边读边按批写入目标集合。
//
// 分批攒着写而不是一篇一篇写：写入的开销主要在每次提交的落盘等待上，
// 而这里整个在一个事务里，攒批只是为了不把全部结果同时摆在内存里。
func (b *QueryBuilder) intoWithin(ctx context.Context, t *Tx, collection string, auto AutoID) (int, error) {
	dst := t.Collection(collection).WithAutoID(auto)
	total := 0
	batch := make([]*Document, 0, manyBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := dst.Insert(ctx, batch...)
		total += n
		batch = batch[:0]
		return err
	}
	for d, err := range b.All(ctx) {
		if err != nil {
			return total, err
		}
		batch = append(batch, d)
		if len(batch) == manyBatch {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	return total, flush()
}
