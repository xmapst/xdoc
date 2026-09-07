package xdoc

import (
	"context"
	"fmt"
	"iter"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// manyBatch 是按条件批改／批删一次推进多少篇。
//
// 分批的意义是内存与匹配到的篇数无关：一次改一千万篇也不会把这一千万篇同时
// 摆在内存里。
const manyBatch = 1000

// UpdateMany 把 predicate 选中的文档按 transform 改一遍，返回改了几篇。
//
// transform 求出的是一篇文档，与原文档**合并**：写到的键覆盖，没写的原样留着。
// predicate 为空串表示全选。
//
// 整批一个事务：任何一篇失败，一篇也不生效。主键改不得——transform 给出一个不同
// 的 _id 会让整批失败，而不是把文档悄悄搬到另一个主键下面。
func (c *Collection) UpdateMany(ctx context.Context, transform, predicate string) (int, error) {
	tn, err := parseTransform(transform)
	if err != nil {
		return 0, err
	}
	total := 0
	err = c.db.Transaction(ctx, func(t *Tx) error {
		var err error
		total, err = t.updateManyIn(ctx, c.name, tn, predicate, nil)
		return err
	})
	return total, err
}

// UpdateMany 与 [Collection.UpdateMany] 一样，但在调用方已有的事务里做。
func (c *TxCollection) UpdateMany(ctx context.Context, transform, predicate string) (int, error) {
	tn, err := parseTransform(transform)
	if err != nil {
		return 0, err
	}
	return c.tx.updateManyIn(ctx, c.name, tn, predicate, nil)
}

// DeleteMany 删掉 predicate 选中的文档，返回删了几篇。
//
// 整批一个事务，predicate 为空串表示全删。
func (c *Collection) DeleteMany(ctx context.Context, predicate string) (int, error) {
	total := 0
	err := c.db.Transaction(ctx, func(t *Tx) error {
		var err error
		total, err = t.deleteManyIn(ctx, c.name, predicate, nil)
		return err
	})
	return total, err
}

// DeleteMany 与 [Collection.DeleteMany] 一样，但在调用方已有的事务里做。
func (c *TxCollection) DeleteMany(ctx context.Context, predicate string) (int, error) {
	return c.tx.deleteManyIn(ctx, c.name, predicate, nil)
}

// DeleteAll 清空集合，索引定义留着。
func (c *Collection) DeleteAll(ctx context.Context) (int, error) {
	return c.DeleteMany(ctx, "")
}

// DeleteAll 在这个事务里清空集合，索引定义留着。
func (c *TxCollection) DeleteAll(ctx context.Context) (int, error) {
	return c.DeleteMany(ctx, "")
}

// transform 是一条编译好的改写表达式。
type transform struct {
	node xbexpr.Node
}

// parseTransform 先把改写表达式解析出来。
//
// 解析放在开事务之前：写错表达式是调用方的问题，不该先开一个事务再回滚。
func parseTransform(src string) (transform, error) {
	n, err := xbexpr.Parse(src)
	if err != nil {
		return transform{}, fmt.Errorf("xdoc: transform expression %q: %w", src, err)
	}
	return transform{node: n}, nil
}

// updateManyIn 是批改的实现：分批取出、逐篇改写、整批写回。
func (t *Tx) updateManyIn(ctx context.Context, name string, tn transform,
	predicate string, params *xbson.Document) (int, error) {
	rel, err := t.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()

	total := 0
	err = t.eachMatched(ctx, name, predicate, params, func(batch []*Document) error {
		out := make([]*Document, 0, len(batch))
		for _, d := range batch {
			m, err := tn.apply(d, t.db.core.Collation(), params)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		n, err := t.db.engine.UpdateIn(ctx, t.tx, name, out)
		total += n
		return err
	})
	return total, err
}

// deleteManyIn 是批删的实现：分批取出主键、整批删掉。
func (t *Tx) deleteManyIn(ctx context.Context, name, predicate string, params *xbson.Document) (int, error) {
	rel, err := t.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()

	total := 0
	err = t.eachMatched(ctx, name, predicate, params, func(batch []*Document) error {
		ids := make([]*Value, 0, len(batch))
		for _, d := range batch {
			if id := d.Get("_id"); id != nil {
				ids = append(ids, id)
			}
		}
		n, err := t.db.engine.DeleteIn(ctx, t.tx, name, ids)
		total += n
		return err
	})
	return total, err
}

// apply 把改写表达式作用在一篇文档上，得到要写回去的那一篇。
//
// 表达式必须求出一篇文档，否则报错——它表达的是"要改哪几个字段"。
//
// 主键单独看住：改写结果没带 _id 就把原来的补回去；带了但与原来不等则报错。
// 放任它改等于把文档搬到另一个主键下面，而那在写回时表现为"多出一篇、少了一篇"。
func (tn transform) apply(doc *Document, coll xcoll.Collation, params *xbson.Document) (*Document, error) {
	v, err := xbexpr.ExecuteScalar(tn.node, doc.Value(), params, coll)
	if err != nil {
		return nil, err
	}
	ext, ok := v.AsDocument()
	if !ok {
		return nil, fmt.Errorf("xdoc: transform must produce a document, got %s", v.Type())
	}
	out := xbexpr.Extend(doc, ext)
	old := doc.Get("_id")
	switch nid := out.Get("_id"); {
	case nid == nil:
		out.Set("_id", old)
	case old != nil && nid.Compare(old, coll) != 0:
		return nil, fmt.Errorf("xdoc: transform cannot change _id (%s -> %s)", old, nid)
	}
	return out, nil
}

// eachMatched 按 _id 递增分批取出匹配的文档，每批交给 fn。
//
// 用「上一批最后一个 _id」当游标往前推，而不是 OFFSET：改写与删除都会让匹配集合
// 在推进过程中变化，按偏移分页会漏掉或重复。
//
// 每批都 ForUpdate，取的是写锁——这一批在改写与写回之间不会被别人动。
//
// 取回来的不足一批就说明到头了，不必再多查一次空结果。
func (t *Tx) eachMatched(ctx context.Context, name, predicate string, params *xbson.Document,
	fn func([]*Document) error) error {
	var last *Value
	for {
		qb := t.Collection(name).Query().OrderBy("_id").Limit(manyBatch).ForUpdate()
		if predicate != "" {
			qb = qb.Where(predicate)
		}

		if params != nil {
			for _, k := range params.Keys() {
				qb = qb.Param(k, params.Get(k))
			}
		}
		if last != nil {
			qb = qb.Where("$._id > @__xdoc_last").Param("__xdoc_last", last)
		}
		batch := make([]*Document, 0, manyBatch)
		for d, err := range qb.All(ctx) {
			if err != nil {
				return err
			}
			batch = append(batch, d)
		}
		if len(batch) == 0 {
			return nil
		}
		last = batch[len(batch)-1].Get("_id")
		if err := fn(batch); err != nil {
			return err
		}

		if len(batch) < manyBatch {
			return nil
		}
	}
}

// Min 取 keySelector 的最小值，空串表示按主键。
//
// 走索引的一端，不扫全表。
func (c *Collection) Min(ctx context.Context, keySelector string) (*Value, error) {
	return c.extreme(ctx, keySelector, false)
}

// Max 取 keySelector 的最大值，空串表示按主键。同样走索引的一端。
func (c *Collection) Max(ctx context.Context, keySelector string) (*Value, error) {
	return c.extreme(ctx, keySelector, true)
}

// extreme 是 [Collection.Min] 与 [Collection.Max] 的共同实现：按该键排序取第一条。
//
// 投影只留这一个键，所以取回来的文档只有一个字段，直接取它即可。
func (c *Collection) extreme(ctx context.Context, keySelector string, desc bool) (*Value, error) {
	if keySelector == "" {
		keySelector = "$._id"
	}
	qb := c.Query().Select(keySelector)
	if desc {
		qb = qb.OrderByDesc(keySelector)
	} else {
		qb = qb.OrderBy(keySelector)
	}
	d, err := qb.First(ctx)
	if err != nil {
		return nil, err
	}
	keys := d.Keys()
	if len(keys) == 0 {
		return xbson.Null, nil
	}
	return d.Get(keys[0]), nil
}

// LongCount 与 [Collection.Count] 一样，只是返回 int64。
func (c *Collection) LongCount(ctx context.Context) (int64, error) {
	n, err := c.Count(ctx)
	return int64(n), err
}

// InsertBulk 写入一批文档，返回写进去的篇数。
//
// batchSize 不起作用：[Collection.Insert] 本来就是整批一个事务，
// 再切开只会多出几次落盘等待，而落盘等待正是批量写入要省掉的那一项。
func (c *Collection) InsertBulk(ctx context.Context, docs []*Document, batchSize int) (int, error) {
	return c.Insert(ctx, docs...)
}

// Find 按条件遍历，可跳过前 skip 篇、最多取 limit 篇。
//
// predicate 为空串表示不过滤；limit 为负表示不限。
func (c *Collection) Find(ctx context.Context, predicate string, skip, limit int) iter.Seq2[*Document, error] {
	qb := c.Query()
	if predicate != "" {
		qb = qb.Where(predicate)
	}
	if skip > 0 {
		qb = qb.Skip(skip)
	}
	if limit >= 0 {
		qb = qb.Limit(limit)
	}
	return qb.All(ctx)
}

// FindOne 取第一篇匹配的文档，没有则返回 [ErrNotFound]。
func (c *Collection) FindOne(ctx context.Context, predicate string) (*Document, error) {
	qb := c.Query()
	if predicate != "" {
		qb = qb.Where(predicate)
	}
	return qb.First(ctx)
}

// FindAll 按主键升序遍历整个集合。
func (c *Collection) FindAll(ctx context.Context) iter.Seq2[*Document, error] {
	return c.All(ctx, Asc)
}

// CountWhere 数一遍匹配的篇数，predicate 为空串表示全数。
func (c *Collection) CountWhere(ctx context.Context, predicate string) (int, error) {
	qb := c.Query()
	if predicate != "" {
		qb = qb.Where(predicate)
	}
	return qb.Count(ctx)
}

// LongCountWhere 与 [Collection.CountWhere] 一样，只是返回 int64。
func (c *Collection) LongCountWhere(ctx context.Context, predicate string) (int64, error) {
	n, err := c.CountWhere(ctx, predicate)
	return int64(n), err
}

// ExistsWhere 报告有没有匹配的文档，取够一篇就停。
func (c *Collection) ExistsWhere(ctx context.Context, predicate string) (bool, error) {
	qb := c.Query()
	if predicate != "" {
		qb = qb.Where(predicate)
	}
	return qb.Exists(ctx)
}
