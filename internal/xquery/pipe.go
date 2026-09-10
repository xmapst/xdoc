package xquery

import (
	"iter"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xsort"
	"github.com/xmapst/xdoc/internal/xstore"
)

// refTarget 是一个被引用集合的入口：它的页面视图和主键索引。
type refTarget struct {
	pages xstore.Pages
	pk    *xpage.CollectionIndex
}

// find 按主键读出被引用的文档并解码，不存在时返回 nil。
func (t *refTarget) find(id *xbson.Value, opts evalOpts) (*xbson.Document, error) {
	st := xstore.New(t.pages)
	node, err := st.List(t.pk).Find(id, false, Ascending, opts.coll)
	if err != nil || node == nil {
		return nil, err
	}
	raw, err := st.ReadDocument(node.DataBlock(), nil)
	if err != nil {
		return nil, err
	}
	return xbson.DecodeIn(raw, opts.loc)
}

// refCacheSize 是每个被引用集合在一次展开里最多记住多少篇文档。
const refCacheSize = 64

// refCache 是一次展开里某个被引用集合的入口，连同已经读出来的文档。
//
// docs 以主键的索引键编码为键，值为 nil 表示目标不存在。**只在展开阶段自己手里用，
// 交出去的一律是克隆**。
type refCache struct {
	target *refTarget
	docs   map[string]*xbson.Document
}

// put 记下一篇文档；记满了随手丢掉一篇，只求挡住同一个 `$id` 反复出现的情形。
func (c *refCache) put(key string, doc *xbson.Document) {
	if len(c.docs) >= refCacheSize {
		for k := range c.docs {
			delete(c.docs, k)
			break
		}
	}
	c.docs[key] = doc
}

// stage 是流水线上的一段，流过的是带地址的文档。
type stage iter.Seq2[row, error]

type (
	// hitSeq 是索引命中项的序列，还没去取文档。
	hitSeq iter.Seq2[hit, error]

	// groupStage 是分组结果的序列。
	groupStage iter.Seq2[groupResult, error]

	// docStage 是最终文档的序列，流水线的出口。
	docStage iter.Seq2[*xbson.Document, error]
)

// loadDocs 把命中项换成文档。
//
// 每产出一条就过一次 safepoint，让长查询有机会让出写锁、也能被取消。
func (src hitSeq) loadDocs(lk lookup, safepoint func() error) stage {
	return func(yield func(row, error) bool) {
		for h, err := range src {
			if err != nil {
				yield(row{}, err)
				return
			}
			r, err := lk.load(h)
			if err != nil {
				yield(row{}, err)
				return
			}
			if !yield(r, nil) {
				return
			}
			if safepoint != nil {
				if err := safepoint(); err != nil {
					yield(row{}, err)
					return
				}
			}
		}
	}
}

// filter 按表达式挑文档。**算不出布尔值的一律当不通过。**
func (src stage) filter(expr xbexpr.Node, params *xbson.Document, opts evalOpts) stage {
	return func(yield func(row, error) bool) {
		for r, err := range src {
			if err != nil {
				yield(row{}, err)
				return
			}
			v, err := xbexpr.ExecuteScalar(expr, r.doc.Value(), params, opts.coll)
			if err != nil {
				yield(row{}, err)
				return
			}
			b, ok := v.AsBoolean()
			if !ok || !b {
				continue
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

// include 把文档里的引用字段就地展开成被引用的那篇文档。
//
// 引用长成 `{$ref: 集合名, $id: 主键}`。展开时删掉 `$ref`、留下 `$id`，
// 再把目标文档除 `_id` 外的字段合并进来；目标不存在就打上 `$missing`。
// 路径指向数组时，逐项展开其中的文档。
//
// 集合入口按名字缓存一份，被引用的文档按主键记住一小批：同一个 `$id`
// 反复出现时不必每次都查找、读取、解码。**合并进来的是克隆**，
// 调用方改动返回的文档不会串到缓存、进而串到别的文档里。
func (src stage) include(path xbexpr.Node, params *xbson.Document,
	opts evalOpts, resolve func(string) (*refTarget, error)) stage {
	return func(yield func(row, error) bool) {
		refs := make(map[string]*refCache)
		var idKey []byte

		doInclude := func(v *xbson.Document) error {
			refID := v.Get("$id")
			name, ok := v.Get("$ref").AsString()
			if refID.IsNull() || !ok {
				return nil
			}
			rc := refs[name]
			if rc == nil {
				t, err := resolve(name)
				if err != nil {
					return err
				}
				rc = &refCache{target: t, docs: make(map[string]*xbson.Document)}
				refs[name] = rc
			}
			if rc.target == nil || rc.target.pk == nil {
				return nil
			}

			// 主键编不成索引键的不进缓存，照旧每次去查。
			var ref *xbson.Document
			var err error
			idKey, err = refID.AppendIndexKey(idKey[:0])
			cacheable, hit := err == nil, false
			if cacheable {
				ref, hit = rc.docs[string(idKey)]
			}
			if !hit {
				if ref, err = rc.target.find(refID, opts); err != nil {
					return err
				}
				if cacheable {
					rc.put(string(idKey), ref)
				}
			}

			if ref == nil {
				v.Set("$missing", xbson.True)
				return nil
			}
			v.Delete("$ref")
			for k, val := range ref.Elements() {
				if k == "_id" {
					continue
				}
				v.Set(k, val.Clone())
			}
			return nil
		}

		for r, err := range src {
			if err != nil {
				yield(row{}, err)
				return
			}
			for v, err := range xbexpr.Execute(path, r.doc.Value(), params, opts.coll) {
				if err != nil {
					yield(row{}, err)
					return
				}
				if d, ok := v.AsDocument(); ok {
					if err := doInclude(d); err != nil {
						yield(row{}, err)
						return
					}
					continue
				}
				a, ok := v.AsArray()
				if !ok {
					continue
				}
				for _, item := range a.Items() {
					d, ok := item.AsDocument()
					if !ok {
						continue
					}
					if err := doInclude(d); err != nil {
						yield(row{}, err)
						return
					}
				}
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

// skipTake 跳过前 offset 条再取 limit 条。
func (src stage) skipTake(offset, limit int) stage {
	return stage(skipTake(iter.Seq2[row, error](src), offset, limit))
}

// skipTake 是分页的通用实现；不跳也不限时直接把上游还回去，不多包一层。
func skipTake[T any](src iter.Seq2[T, error], offset, limit int) iter.Seq2[T, error] {
	if offset <= 0 && limit >= NoLimit {
		return src
	}
	return func(yield func(T, error) bool) {
		if limit <= 0 {
			return
		}
		var zero T
		skipped, taken := 0, 0
		for v, err := range src {
			if err != nil {
				yield(zero, err)
				return
			}
			if skipped < offset {
				skipped++
				continue
			}
			if !yield(v, nil) {
				return
			}
			taken++
			if limit < NoLimit && taken >= limit {
				return
			}
		}
	}
}

// sortBy 走外部排序，再按名次取回文档。
//
// **排的是「键加地址」而不是文档本身**：文档留在盘上，排完再按地址读回来，
// 所以内存占用与文档大小无关。上游的错误经 srcErr 带出——
// 喂给排序器的那个回调没法直接报错。
func (src stage) sortBy(segs []OrderSegment, offset, limit int, lk lookup,
	disk *xsort.Disk, params *xbson.Document, opts evalOpts) stage {
	return func(yield func(row, error) bool) {
		orders := make([]xsort.Order, len(segs))
		for i, s := range segs {
			orders[i] = xsort.Order(s.Order)
		}
		sorter, err := disk.NewSorter(opts.coll, orders...)
		if err != nil {
			yield(row{}, err)
			return
		}
		defer func() { _ = sorter.Close() }()

		var srcErr error
		keys := func(emit func(*xbson.Value, xpage.Address) bool) {
			for r, err := range src {
				if err != nil {
					srcErr = err
					return
				}
				k, err := r.sortKey(segs, params, opts)
				if err != nil {
					srcErr = err
					return
				}
				if !emit(k, r.addr) {
					return
				}
			}
		}
		if err := sorter.Insert(keys); err != nil {
			yield(row{}, err)
			return
		}
		if srcErr != nil {
			yield(row{}, srcErr)
			return
		}

		skipped, taken := 0, 0
		if limit <= 0 {
			return
		}
		for _, addr := range sorter.All() {
			if skipped < offset {
				skipped++
				continue
			}
			doc, err := lk.loadAddr(addr)
			if err != nil {
				yield(row{}, err)
				return
			}
			if !yield(row{doc: doc, addr: addr}, nil) {
				return
			}
			taken++
			if limit < NoLimit && taken >= limit {
				return
			}
		}
		if err := sorter.Err(); err != nil {
			yield(row{}, err)
		}
	}
}

// sortKey 算出一条记录的排序键；多级排序时打包成数组，逐项比。
func (r row) sortKey(segs []OrderSegment, params *xbson.Document,
	opts evalOpts) (*xbson.Value, error) {
	root := r.doc.Value()
	if len(segs) == 1 {
		return xbexpr.ExecuteScalar(segs[0].Expr, root, params, opts.coll)
	}
	a := xbson.NewArray()
	for _, s := range segs {
		v, err := xbexpr.ExecuteScalar(s.Expr, root, params, opts.coll)
		if err != nil {
			return nil, err
		}
		a.Append(v)
	}
	return a.Value(), nil
}

// selectOne 对每篇文档算一次投影，一进一出。
func (src stage) selectOne(expr xbexpr.Node, params *xbson.Document,
	opts evalOpts) docStage {
	return func(yield func(*xbson.Document, error) bool) {
		name := defaultFieldName(expr)
		for r, err := range src {
			if err != nil {
				yield(nil, err)
				return
			}
			v, err := xbexpr.ExecuteScalar(expr, r.doc.Value(), params, opts.coll)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(wrapValue(v, name), nil) {
				return
			}
		}
	}
}

// selectAll 把上游整个当成一个数组来算，用于聚合类投影。
//
// **要先把全部文档攒进内存**，与 [stage.selectOne] 的流式处理不同。
func (src stage) selectAll(expr xbexpr.Node, params *xbson.Document,
	opts evalOpts) docStage {
	return func(yield func(*xbson.Document, error) bool) {
		name := defaultFieldName(expr)
		items := xbson.NewArray()
		for r, err := range src {
			if err != nil {
				yield(nil, err)
				return
			}
			items.Append(r.doc.Value())
		}
		node := substituteSource(expr, sourceParam)
		p := withParam(params, sourceParam, items.Value())
		for v, err := range xbexpr.ExecuteAggregate(node, p, opts.coll) {
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(wrapValue(v, name), nil) {
				return
			}
		}
	}
}

// sourceParam 是聚合表达式里指代整批文档的参数名。
const sourceParam = "__source"

// groupKeyParam 是分组表达式里指代当前分组键的参数名。
const groupKeyParam = "key"

// withParam 在参数表上添一项。**克隆一份再改**，不动调用方手里那份。
func withParam(params *xbson.Document, name string, v *xbson.Value) *xbson.Document {
	var d *xbson.Document
	if params == nil {
		d = xbson.NewDocument()
	} else {
		d = params.Clone()
	}
	d.Set(name, v)
	return d
}

// wrapValue 把一个值包成文档；本来就是文档的原样返回，不再套一层。
func wrapValue(v *xbson.Value, name string) *xbson.Document {
	if d, ok := v.AsDocument(); ok {
		return d
	}
	d := xbson.NewDocument()
	d.Set(name, v)
	return d
}
