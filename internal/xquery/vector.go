package xquery

import (
	"iter"
	"math"
	"slices"
	"strings"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xvector"
)

// costVector 是向量索引的代价。选中之后没有别的路可比，值多少不重要。
const costVector uint32 = 1

// opVector 是向量索引访问方式：找出离目标最近的若干条。
//
// 结果按距离由近到远给出，与普通索引按键排序不是一回事。
type opVector struct {
	// name 是向量索引名，target 是要比的目标向量，maxDist 是距离上限。
	name    string
	target  []float32
	maxDist float64

	// limit 是最多取几条，0 表示不限。
	limit int

	// vx 是索引的元信息，由 bindVector 在执行前填上。
	vx *xpage.VectorIndex
}

// indexName 返回向量索引名。
func (o *opVector) indexName() string { return o.name }

// order 恒为升序：距离由近到远。
func (o *opVector) order() Order { return Ascending }

// setOrder 无事可做：向量搜索的次序是距离，改不了。
func (o *opVector) setOrder(Order) {}

// keyOrdered 为真：结果按距离有序。
func (o *opVector) keyOrdered() bool { return true }

// cost 恒为 costVector。
func (o *opVector) cost(*xpage.CollectionIndex) uint32 { return costVector }

// mode 返回访问方式的描述。
func (o *opVector) mode() string { return "VECTOR INDEX SEARCH" }

// bindVector 按名字找到向量索引并记下来；集合里没有这个索引就返回假。
func (o *opVector) bindVector(cp *xpage.CollectionPage) bool {
	vx, ok := cp.VectorIndexByName(o.name)
	if !ok {
		return false
	}
	o.vx = vx
	return true
}

// execute 做一次向量搜索，把结果变成一串命中项。
//
// **键位置放的是 Null**：向量搜索没有索引键这一说，只有数据块地址有用。
func (o *opVector) execute(p xstore.Pages, _ *xpage.CollectionIndex,
	_ xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		if o.vx == nil {
			return
		}
		hits, err := xstore.New(p).Vector(o.vx).Search(o.target, o.maxDist, o.limit)
		if err != nil {
			yield(hit{}, err)
			return
		}
		for _, h := range hits {
			if !yield(hit{key: *xbson.Null, dataBlock: h.DataBlock}, nil) {
				return
			}
		}
	}
}

// trySelectVectorIndex 看这条查询能不能走向量索引。
//
// 先在过滤条件里找形如「相似度 < 某个数」的式子；找不到再看排序键里
// 有没有按相似度排的——那种情况距离不设上限，靠 LIMIT 收口，
// 并记下排序已经由索引兑现。
//
// 最后要求索引表达式与查询里写的一致，且**维数完全相同**。
func (o *optimizer) trySelectVectorIndex() (*candidate, error) {
	if o.cp == nil || len(o.cp.VectorIndexes()) == 0 {
		return nil, nil
	}
	var (
		expr    string
		target  []float32
		maxDist float64

		term      xbexpr.Node
		fromOrder bool
	)
	for _, t := range o.terms {
		e, tg, md, ok, err := o.vectorPredicate(t)
		if err != nil {
			return nil, err
		}
		if ok {
			expr, target, maxDist, term = e, tg, md, t
			break
		}
	}
	if expr == "" && len(o.q.OrderBy) > 0 {
		for _, seg := range o.q.OrderBy {
			e, tg, ok, err := o.vectorSimExpr(seg.Expr)
			if err != nil {
				return nil, err
			}
			if ok {
				expr, target, maxDist, fromOrder = e, tg, math.MaxFloat64, true
				break
			}
		}
	}
	if expr == "" || target == nil {
		return nil, nil
	}

	limit := 0
	if o.q.Limit != NoLimit {
		limit = o.q.Limit
	}

	vxs := o.cp.VectorIndexes()
	for i := range vxs {
		ix, ok := o.cp.Index(vxs[i].Name)
		if !ok {
			continue
		}
		ce := canonicalExpr(ix.Expression)

		if !strings.EqualFold(ce, expr) {
			continue
		}
		if int(vxs[i].Dimensions) != len(target) {
			continue
		}
		if fromOrder {
			o.vectorOrderConsumed = true
		}
		return &candidate{
			op:   &opVector{name: vxs[i].Name, target: target, maxDist: maxDist, limit: limit},
			cost: costVector,
			term: term,
			expr: ce,
		}, nil
	}
	return nil, nil
}

// vectorPredicate 从一个过滤条件里拆出向量表达式、目标向量和距离上限。
//
// 只认相似度在小的一侧的比较：`sim(...) < d` 或 `d > sim(...)`。
func (o *optimizer) vectorPredicate(t xbexpr.Node) (string, []float32, float64, bool, error) {
	b := binaryOf(t)
	if b == nil || b.Left == nil || b.Right == nil {
		return "", nil, 0, false, nil
	}
	var simSide, numSide xbexpr.Node
	switch b.Op.Base() {
	case xbexpr.OpLT, xbexpr.OpLTE:
		simSide, numSide = b.Left, b.Right
	case xbexpr.OpGT, xbexpr.OpGTE:
		simSide, numSide = b.Right, b.Left
	default:
		return "", nil, 0, false, nil
	}
	e, tg, ok, err := o.vectorSimExpr(simSide)
	if err != nil || !ok {
		return "", nil, 0, false, err
	}
	d, ok, err := o.constDouble(numSide)
	if err != nil || !ok {
		return "", nil, 0, false, err
	}
	return e, tg, d, true, nil
}

// vectorSimExpr 从一个相似度运算里拆出左边的字段表达式和右边算出来的目标向量。
func (o *optimizer) vectorSimExpr(n xbexpr.Node) (string, []float32, bool, error) {
	b := binaryOf(n)
	if b == nil || b.Op.Base() != xbexpr.OpVectorSim || b.Left == nil || b.Right == nil {
		return "", nil, false, nil
	}
	src := sourceText(b.Left)
	if src == "" {
		return "", nil, false, nil
	}
	v, _, err := o.opts.scalarValue(b.Right, o.q.Params)
	if err != nil {
		return "", nil, false, err
	}
	tg, ok := targetVector(v)
	if !ok {
		return "", nil, false, nil
	}
	return src, tg, true, nil
}

// constDouble 把一个与文档无关的表达式算成浮点数。非数或者 NaN 都算不认。
func (o *optimizer) constDouble(n xbexpr.Node) (float64, bool, error) {
	v, _, err := o.opts.scalarValue(n, o.q.Params)
	if err != nil {
		return 0, false, err
	}
	if v == nil || !v.Type().IsNumber() {
		return 0, false, nil
	}
	d, ok := xvector.ToDouble(v)
	if !ok || math.IsNaN(d) {
		return 0, false, nil
	}
	return d, true, nil
}

// targetVector 把一个值取成向量。
//
// 向量值直接克隆；数组要求每一项都是数字，**有一个 Null 就整个不认**。
func targetVector(v *xbson.Value) ([]float32, bool) {
	if v == nil || v.Type() == xbson.TypeNull {
		return nil, false
	}
	if f, ok := v.AsVector(); ok {
		return slices.Clone(f), true
	}
	a, ok := v.AsArray()
	if !ok {
		return nil, false
	}
	out := make([]float32, 0, a.Len())
	for _, it := range a.Items() {
		if it.Type() == xbson.TypeNull {
			return nil, false
		}
		f, ok := xvector.ToDouble(it)
		if !ok {
			return nil, false
		}
		out = append(out, float32(f))
	}
	return out, true
}
