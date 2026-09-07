package xquery

import (
	"cmp"
	"iter"
	"slices"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// group 是一组键相同的文档。
type group struct {
	key  *xbson.Value
	docs []*xbson.Document
}

// groupBy 把上游按分组键切成一组一组。
//
// **只并相邻的**：键一变就开新组，所以上游必须已经按这个键排好序。
func (src stage) groupBy(keyExpr xbexpr.Node, params *xbson.Document,
	opts evalOpts) iter.Seq2[group, error] {
	return func(yield func(group, error) bool) {
		next, stop := iter.Pull2(iter.Seq2[row, error](src))
		defer stop()

		keyOf := func(r row) (*xbson.Value, error) {
			return xbexpr.ExecuteScalar(keyExpr, r.doc.Value(), params, opts.coll)
		}

		r, err, ok := next()
		if !ok {
			return
		}
		for ok {
			if err != nil {
				yield(group{}, err)
				return
			}
			key, kerr := keyOf(r)
			if kerr != nil {
				yield(group{}, kerr)
				return
			}
			g := group{key: key, docs: []*xbson.Document{r.doc}}
			for {
				r, err, ok = next()
				if !ok || err != nil {
					break
				}
				k, kerr := keyOf(r)
				if kerr != nil {
					yield(group{}, kerr)
					return
				}
				if k.Compare(key, opts.coll) != 0 {
					break
				}
				g.docs = append(g.docs, r.doc)
			}
			if !yield(g, nil) {
				return
			}
		}
		if err != nil {
			yield(group{}, err)
		}
	}
}

// groupResult 是一组算出来的结果文档，keys 是它在后续排序里要用的排序键。
type groupResult struct {
	doc *xbson.Document

	keys []*xbson.Value
}

// selectGroupStage 对每一组算投影，顺带做 HAVING 过滤和排序键。
//
// 组内文档打包成一个数组喂给表达式，分组键和这个数组分别以具名参数给出，
// 所以聚合函数能直接对着整组算。投影表达式取整篇文档时，
// 输出的是 `{key, items}` 这样一篇两字段文档。
func (g *GroupPlan) selectGroupStage(groups iter.Seq2[group, error], resultOrder []OrderSegment,
	params *xbson.Document, opts evalOpts) groupStage {
	return func(yield func(groupResult, error) bool) {
		name := defaultFieldName(g.Select)
		selectNode := substituteSource(g.Select, sourceParam)
		havingNode := substituteSource(g.Having, sourceParam)
		orderNodes := make([]xbexpr.Node, len(resultOrder))
		for i, s := range resultOrder {
			orderNodes[i] = substituteSource(s.Expr, sourceParam)
		}

		for gr, err := range groups {
			if err != nil {
				yield(groupResult{}, err)
				return
			}
			items := xbson.NewArray()
			for _, d := range gr.docs {
				items.Append(d.Value())
			}
			p := withParam(withParam(params, groupKeyParam, gr.key), sourceParam, items.Value())

			if havingNode != nil {
				v, err := xbexpr.ExecuteAggregateScalar(havingNode, p, opts.coll)
				if err != nil {
					yield(groupResult{}, err)
					return
				}
				b, ok := v.AsBoolean()
				if !ok || !b {
					continue
				}
			}

			var doc *xbson.Document
			if isRootExpr(g.Select) {
				doc = xbson.NewDocument()
				doc.Set("key", gr.key)
				doc.Set("items", items.Value())
			} else {
				v, err := xbexpr.ExecuteAggregateScalar(selectNode, p, opts.coll)
				if err != nil {
					yield(groupResult{}, err)
					return
				}
				doc = wrapValue(v, name)
			}

			res := groupResult{doc: doc}
			if len(orderNodes) > 0 {
				res.keys = make([]*xbson.Value, len(orderNodes))
				for i, n := range orderNodes {
					v, err := xbexpr.ExecuteScalar(n, doc.Value(), p, opts.coll)
					if err != nil {
						yield(groupResult{}, err)
						return
					}
					res.keys[i] = v
				}
			}
			if !yield(res, nil) {
				return
			}
		}
	}
}

// sortBy 对分组结果排序，只留下需要的那一段。
//
// 堆里只留 offset+limit 条，再多的当场丢掉，不必把全部结果攒在内存里。
func (src groupStage) sortBy(segs []OrderSegment, offset, limit int,
	coll xcoll.Collation) docStage {
	return func(yield func(*xbson.Document, error) bool) {
		if limit <= 0 {
			return
		}

		keep := min(int64(offset)+int64(limit), int64(NoLimit))
		h := topGroups{segs: segs, coll: coll, keep: int(keep)}
		for r, err := range src {
			if err != nil {
				yield(nil, err)
				return
			}
			h.push(r)
		}

		for i, r := range h.sorted() {
			if i < offset {
				continue
			}
			if !yield(r.doc, nil) {
				return
			}
		}
	}
}

// rankedGroup 给结果配上流入次序，用来在排序键相等时保持稳定。
type rankedGroup struct {
	groupResult
	rank int
}

// topGroups 是个大顶堆，留住排序最靠前的若干条。
//
// 堆顶是这批里最靠后的那条；新来的比堆顶还靠后就直接丢，否则顶掉堆顶。
type topGroups struct {
	segs []OrderSegment
	coll xcoll.Collation
	keep int

	items []rankedGroup
	seen  int
}

// after 判断 a 是不是排在 b 后面；排序键相等时按流入次序定。
func (h *topGroups) after(a, b rankedGroup) bool {
	if d := h.compareKeys(a.keys, b.keys); d != 0 {
		return d > 0
	}
	return a.rank > b.rank
}

// push 收一条结果：堆没满就放进去，满了就与堆顶比。
func (h *topGroups) push(r groupResult) {
	it := rankedGroup{groupResult: r, rank: h.seen}
	h.seen++
	if len(h.items) < h.keep {
		h.items = append(h.items, it)
		h.up(len(h.items) - 1)
		return
	}

	if h.after(it, h.items[0]) {
		return
	}
	h.items[0] = it
	h.down(0)
}

// up 把第 i 项往上浮到合适位置。
func (h *topGroups) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.after(h.items[i], h.items[p]) {
			return
		}
		h.items[i], h.items[p] = h.items[p], h.items[i]
		i = p
	}
}

// down 把第 i 项往下沉到合适位置。
func (h *topGroups) down(i int) {
	for {
		l, last := 2*i+1, i
		if l < len(h.items) && h.after(h.items[l], h.items[last]) {
			last = l
		}
		if r := l + 1; r < len(h.items) && h.after(h.items[r], h.items[last]) {
			last = r
		}
		if last == i {
			return
		}
		h.items[i], h.items[last] = h.items[last], h.items[i]
		i = last
	}
}

// sorted 取出堆里的全部结果并排好序。**调用后堆就空了。**
func (h *topGroups) sorted() []rankedGroup {
	out := h.items
	h.items = nil
	slices.SortFunc(out, func(a, b rankedGroup) int {
		if d := h.compareKeys(a.keys, b.keys); d != 0 {
			return d
		}
		return cmp.Compare(a.rank, b.rank)
	})
	return out
}

// compareKeys 逐级比排序键，按每一级各自的方向。
func (h *topGroups) compareKeys(a, b []*xbson.Value) int {
	n := min(len(a), len(b), len(h.segs))
	for i := range n {
		d := a[i].Compare(b[i], h.coll)
		if d != 0 {
			return int(h.segs[i].Order) * d
		}
	}
	return 0
}

// skipTake 跳过前 offset 条再取 limit 条。
func (src docStage) skipTake(offset, limit int) docStage {
	return docStage(skipTake(iter.Seq2[*xbson.Document, error](src), offset, limit))
}

// docs 丢掉排序键，只留结果文档。
func (src groupStage) docs() docStage {
	return func(yield func(*xbson.Document, error) bool) {
		for r, err := range src {
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(r.doc, nil) {
				return
			}
		}
	}
}
