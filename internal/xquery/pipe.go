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
// 集合入口按名字缓存一份，连着展开同一个集合时不必反复取快照。
func (src stage) include(path xbexpr.Node, params *xbson.Document,
	opts evalOpts, resolve func(string) (*refTarget, error)) stage {
	return func(yield func(row, error) bool) {
		var lastName string
		var last *refTarget

		doInclude := func(v *xbson.Document) error {
			refID := v.Get("$id")
			refCol := v.Get("$ref")
			name, ok := refCol.AsString()
			if refID.IsNull() || !ok {
				return nil
			}
			if name != lastName || last == nil {
				t, err := resolve(name)
				if err != nil {
					return err
				}
				lastName, last = name, t
			}
			if last == nil || last.pk == nil {
				return nil
			}
			st := xstore.New(last.pages)
			node, err := st.List(last.pk).Find(refID, false, Ascending, opts.coll)
			if err != nil {
				return err
			}
			if node == nil {
				v.Set("$missing", xbson.True)
				return nil
			}
			raw, err := st.ReadDocument(node.DataBlock(), nil)
			if err != nil {
				return err
			}

			ref, err := xbson.DecodeIn(raw, opts.loc)
			if err != nil {
				return err
			}
			v.Delete("$ref")
			for k, val := range ref.Elements() {
				if k == "_id" {
					continue
				}
				v.Set(k, val)
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
