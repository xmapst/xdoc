package xquery

import (
	"fmt"
	"iter"
	"strings"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
)

// hit 是一次索引命中：键、文档所在的数据块、以及索引节点自己的地址。
type hit struct {
	// key 是索引键。
	key xbson.Value

	// dataBlock 是文档所在的数据块地址。
	dataBlock xpage.Address

	// addr 是索引节点的地址，沿链前后走时要用。
	addr xpage.Address
}

// indexOp 是一种索引访问方式：怎么在索引上找出候选的文档。
//
// 各实现的差别只在 execute 怎么走链，其余方法都是给编排和 EXPLAIN 用的元信息。
type indexOp interface {
	// indexName 返回要用的索引名。
	indexName() string

	// order 返回遍历方向。
	order() Order

	// setOrder 改遍历方向。有些方式的次序是定死的，改不动就什么也不做。
	setOrder(Order)

	// cost 估这次访问的代价，越小越优先。
	cost(ix *xpage.CollectionIndex) uint32

	// mode 返回访问方式的描述，供 EXPLAIN 输出。
	mode() string

	// keyOrdered 说明产出是不是按索引键有序——有序才能省掉后面的排序。
	keyOrdered() bool

	// execute 走一遍索引，产出命中项。
	execute(p xstore.Pages, ix *xpage.CollectionIndex, coll xcoll.Collation) iter.Seq2[hit, error]
}

// runIndex 按计划选中的方式走索引，顺带做几项检查和去重。
//
// 向量索引只能经相似度运算走，当普通索引使会被挡下。
//
// **按数据块地址去重**：一篇文档可能在同一个索引里命中多次——
// 数组字段每一项都建了键。地址为空的命中项不参与去重。
func (p *Plan) runIndex(pages xstore.Pages, cp *xpage.CollectionPage,
	coll xcoll.Collation) hitSeq {
	return func(yield func(hit, error) bool) {
		if cp == nil {
			return
		}

		op := p.index
		ix, ok := cp.Index(op.indexName())
		if !ok {
			yield(hit{}, fmt.Errorf("%w: index %q does not exist on this collection",
				ErrInternal, op.indexName()))
			return
		}
		v, isVec := op.(*opVector)
		if isVec && !v.bindVector(cp) {
			yield(hit{}, fmt.Errorf("%w: vector index %q does not exist on this collection",
				ErrInternal, op.indexName()))
			return
		}
		if !isVec && ix.Kind == xpage.IndexVector {
			yield(hit{}, fmt.Errorf(
				"%w: %q is a vector index; it can only be searched through VECTOR_SIM, "+
					"not used as an ordinary index", ErrInternal, op.indexName()))
			return
		}
		seen := map[xpage.Address]bool{}
		for h, err := range op.execute(pages, ix, coll) {
			if err != nil {
				yield(hit{}, err)
				return
			}
			if !h.dataBlock.IsEmpty() {
				if seen[h.dataBlock] {
					continue
				}
				seen[h.dataBlock] = true
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}

// isSentinel 判断一个键是不是链两端的哨兵，那两个不是真数据。
func isSentinel(v *xbson.Value) bool {
	return v.Type() == xbson.TypeMinValue || v.Type() == xbson.TypeMaxValue
}

// readHit 从索引节点上读出一个命中项。
func readHit(n *xstore.Node) (hit, error) {
	var h hit
	if err := n.KeyInto(&h.key); err != nil {
		return hit{}, err
	}
	h.dataBlock, h.addr = n.DataBlock(), n.Addr
	return h, nil
}

// scan 沿索引的底层链往一个方向走。
type scan struct {
	pages xstore.Pages
	ord   Order
}

// step 取下一个节点的地址，方向由 ord 定。
func (s scan) step(n *xstore.Node) xpage.Address {
	if s.ord == Ascending {
		return n.Next0()
	}
	return n.Prev0()
}

// from 从某个节点开始走，每到一个就调一次 fn，fn 返回假就停。
//
// **走的步数有上限**：链要是被写坏成了环，这里能兜住而不是一直转下去。
func (s scan) from(addr xpage.Address, fn func(h hit) (bool, error)) error {
	st := xstore.New(s.pages)
	limit, hops := st.ChainLimit(), 0
	for !addr.IsEmpty() {
		if hops++; hops > limit {
			return fmt.Errorf("%w: index chain still going after %d nodes",
				xpage.ErrCorrupt, limit)
		}
		n, err := st.NodeAt(addr)
		if err != nil {
			return err
		}
		if n == nil {
			return nil
		}
		h, err := readHit(n)
		if err != nil {
			return err
		}
		next := s.step(n)
		cont, err := fn(h)
		if err != nil || !cont {
			return err
		}
		addr = next
	}
	return nil
}

// opAll 从头到尾走一遍索引，不做任何筛选。
type opAll struct {
	name string
	ord  Order
}

// indexName 返回索引名。
func (o *opAll) indexName() string { return o.name }

// order 返回遍历方向。
func (o *opAll) order() Order { return o.ord }

// setOrder 改遍历方向。
func (o *opAll) setOrder(v Order) { o.ord = v }

// keyOrdered 为真：整条链本来就是按键排好的。
func (o *opAll) keyOrdered() bool { return true }

// cost 恒为 costAll，是几种方式里最贵的。
func (o *opAll) cost(*xpage.CollectionIndex) uint32 { return costAll }

// mode 返回访问方式的描述。
func (o *opAll) mode() string { return fmt.Sprintf("FULL INDEX SCAN(%s)", o.name) }

// execute 走完整条链。
func (o *opAll) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	_ xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		for n, err := range xstore.New(p).List(ix).FindAll(o.ord) {
			if err != nil {
				yield(hit{}, err)
				return
			}
			h, err := readHit(n)
			if err != nil {
				yield(hit{}, err)
				return
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}

// opEquals 找出键等于某个值的全部命中项。
type opEquals struct {
	name  string
	value *xbson.Value
}

// indexName 返回索引名。
func (o *opEquals) indexName() string { return o.name }

// order 恒为升序。
func (o *opEquals) order() Order { return Ascending }

// setOrder 无事可做：等值查的次序无所谓。
func (o *opEquals) setOrder(Order) {}

// keyOrdered 为真：键全都一样，怎么排都算有序。
func (o *opEquals) keyOrdered() bool { return true }

// cost 看索引唯不唯一：唯一索引最多命中一条，最便宜。
func (o *opEquals) cost(ix *xpage.CollectionIndex) uint32 {
	if ix.Unique {
		return costEqualsUnique
	}
	return costEquals
}

// mode 返回访问方式的描述。
func (o *opEquals) mode() string {
	return fmt.Sprintf("INDEX SEEK(%s = %s)", o.name, o.value)
}

// execute 定位到一个等值节点，再把左右两边键相同的都捞出来。
//
// **唯一索引找到一条就收**。非唯一的要两个方向都走：查找落在哪一个
// 相同键上不确定，前后都可能还有。
func (o *opEquals) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	coll xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		st := xstore.New(p)
		start, err := st.List(ix).Find(o.value, false, Ascending, coll)
		if err != nil {
			yield(hit{}, err)
			return
		}
		if start == nil {
			return
		}
		h, err := readHit(start)
		if err != nil {
			yield(hit{}, err)
			return
		}
		if !yield(h, nil) {
			return
		}
		if ix.Unique {
			return
		}

		stop := false
		for _, dir := range []Order{Ascending, Descending} {
			n, err := st.NodeAt(h.addr)
			if err != nil {
				yield(hit{}, err)
				return
			}
			s := scan{pages: p, ord: dir}
			err = s.from(s.step(n), func(cur hit) (bool, error) {
				if isSentinel(&cur.key) || cur.key.Compare(o.value, coll) != 0 {
					return false, nil
				}
				if !yield(cur, nil) {
					stop = true
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				yield(hit{}, err)
				return
			}
			if stop {
				return
			}
		}
	}
}

// opRange 取一段键区间。两端各自可开可闭，方向可反。
type opRange struct {
	name       string
	start, end *xbson.Value
	startEq    bool
	endEq      bool
	ord        Order
}

// indexName 返回索引名。
func (o *opRange) indexName() string { return o.name }

// order 返回遍历方向。
func (o *opRange) order() Order { return o.ord }

// setOrder 改遍历方向。
func (o *opRange) setOrder(v Order) { o.ord = v }

// keyOrdered 为真：沿链走，天然按键有序。
func (o *opRange) keyOrdered() bool { return true }

// cost 恒为 costRange。
func (o *opRange) cost(*xpage.CollectionIndex) uint32 { return costRange }

// mode 按区间的形状给出描述：单边的写成大于小于，两端都有的写成 BETWEEN。
func (o *opRange) mode() string {
	switch {
	case o.start.Type() == xbson.TypeMinValue && !o.endEq:
		return fmt.Sprintf("INDEX SCAN(%s < %s)", o.name, o.end)
	case o.start.Type() == xbson.TypeMinValue && o.endEq:
		return fmt.Sprintf("INDEX SCAN(%s <= %s)", o.name, o.end)
	case o.end.Type() == xbson.TypeMaxValue && !o.startEq:
		return fmt.Sprintf("INDEX SCAN(%s > %s)", o.name, o.start)
	case o.end.Type() == xbson.TypeMaxValue && o.startEq:
		return fmt.Sprintf("INDEX SCAN(%s >= %s)", o.name, o.start)
	}
	return fmt.Sprintf("INDEX RANGE SCAN(%s BETWEEN %s AND %s)", o.name, o.start, o.end)
}

// execute 走一遍区间。
//
// 反向遍历时把两端连同各自的开闭一起对调，后面的逻辑就不必再分方向。
// 起点闭区间时要**先往回走一段**：定位落在哪一个相同键上不确定，
// 前面可能还有键相同的。哨兵一律跳过但不停下。
func (o *opRange) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	coll xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		start, end := o.start, o.end
		startEq, endEq := o.startEq, o.endEq
		if o.ord == Descending {
			start, end = end, start
			startEq, endEq = endEq, startEq
		}

		first, err := o.locate(p, ix, start, coll)
		if err != nil {
			yield(hit{}, err)
			return
		}
		if first == nil {
			return
		}

		stop := false
		emit := func(h hit) bool {
			if isSentinel(&h.key) {
				return true
			}
			if !yield(h, nil) {
				stop = true
				return false
			}
			return true
		}

		openEnd := endEq && end.Type() == xbson.TypeMaxValue

		withinEnd := func(k *xbson.Value) bool {
			if openEnd {
				return true
			}
			d := k.Compare(end, coll)
			return (endEq && d == 0) || d == -int(o.ord)
		}

		if startEq {
			back := scan{pages: p, ord: -o.ord}
			err = back.from(back.step(first), func(cur hit) (bool, error) {
				if isSentinel(&cur.key) || cur.key.Compare(start, coll) != 0 {
					return false, nil
				}
				if withinEnd(&cur.key) && !emit(cur) {
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				yield(hit{}, err)
				return
			}
			if stop {
				return
			}
		}

		inStart := true
		fwd := scan{pages: p, ord: o.ord}
		err = fwd.from(first.Addr, func(cur hit) (bool, error) {
			if inStart {
				if cur.key.Compare(start, coll) == 0 {
					if startEq && withinEnd(&cur.key) && !emit(cur) {
						return false, nil
					}
					return true, nil
				}
				inStart = false
			}
			if !withinEnd(&cur.key) {
				return false, nil
			}
			return emit(cur), nil
		})
		if err != nil {
			yield(hit{}, err)
			return
		}
	}
}

// locate 找到区间的起点。起点是最小值或最大值时直接取链的两端，不必查找。
func (o *opRange) locate(p xstore.Pages, ix *xpage.CollectionIndex,
	start *xbson.Value, coll xcoll.Collation) (*xstore.Node, error) {
	st := xstore.New(p)
	switch start.Type() {
	case xbson.TypeMinValue:
		return st.NodeAt(ix.Head)
	case xbson.TypeMaxValue:
		return st.NodeAt(ix.Tail)
	}
	return st.List(ix).Find(start, true, o.ord, coll)
}

// opLike 做模式匹配。
//
// 有固定前缀时能定位到一段再匹配，否则只能整条链扫过去。
type opLike struct {
	name    string
	pattern string
	prefix  string
	ord     Order

	// match 判断一个字符串合不合模式。
	match func(s string) (bool, error)
}

// indexName 返回索引名。
func (o *opLike) indexName() string { return o.name }

// order 返回遍历方向。
func (o *opLike) order() Order { return o.ord }

// setOrder 改遍历方向。
func (o *opLike) setOrder(v Order) { o.ord = v }

// keyOrdered 只在没有前缀时才为真。
//
// **有前缀反而无序**：那条路要先往回走一段再往前走，产出不是按键排的。
func (o *opLike) keyOrdered() bool { return o.prefix == "" }

// cost 有前缀时按等值查算，没有就是全扫。
func (o *opLike) cost(*xpage.CollectionIndex) uint32 {
	if o.prefix != "" {
		return costEquals
	}
	return costAll
}

// mode 返回访问方式的描述，区分有没有走到前缀定位。
func (o *opLike) mode() string {
	kind := "FULL INDEX SCAN"
	if o.prefix != "" {
		kind = "INDEX SEEK (+RANGE SCAN)"
	}
	return fmt.Sprintf("%s(%s LIKE %q)", kind, o.name, o.pattern)
}

// execute 按有没有固定前缀分两条路。
func (o *opLike) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	coll xcoll.Collation) iter.Seq2[hit, error] {
	if o.prefix == "" {
		return o.scanAll(p, ix)
	}
	return o.scanPrefix(p, ix, coll)
}

// scanAll 整条链扫过去逐个匹配。非字符串的键直接跳过。
func (o *opLike) scanAll(p xstore.Pages, ix *xpage.CollectionIndex) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		for n, err := range xstore.New(p).List(ix).FindAll(o.ord) {
			if err != nil {
				yield(hit{}, err)
				return
			}
			h, err := readHit(n)
			if err != nil {
				yield(hit{}, err)
				return
			}
			s, ok := h.key.AsString()
			if !ok {
				continue
			}
			ok, err = o.match(s)
			if err != nil {
				yield(hit{}, err)
				return
			}
			if ok && !yield(h, nil) {
				return
			}
		}
	}
}

// scanPrefix 定位到前缀那一段，往两边各走到不再有这个前缀为止。
//
// **前缀检查只在排序规则保证「相等则等长」时才做**：有些规则下
// 两个长度不同的串也可能算相等，那样按字节截取比前缀会漏。
// 不保证时就退回逐个匹配，慢但不会漏。
func (o *opLike) scanPrefix(p xstore.Pages, ix *xpage.CollectionIndex,
	coll xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		first, err := xstore.New(p).List(ix).Find(xbson.String(o.prefix), true, o.ord, coll)
		if err != nil {
			yield(hit{}, err)
			return
		}
		if first == nil {
			return
		}
		stop := false
		visit := func(cur hit) (bool, error) {
			if isSentinel(&cur.key) {
				return false, nil
			}
			s, ok := cur.key.AsString()
			if !ok {
				return false, nil
			}
			if coll.SameLengthWhenEqual() && !hasPrefix(s, o.prefix, coll) {
				return false, nil
			}
			ok, err := o.match(s)
			if err != nil {
				return false, err
			}
			if ok && !cur.dataBlock.IsEmpty() {
				if !yield(cur, nil) {
					stop = true
					return false, nil
				}
			}
			return true, nil
		}

		back := scan{pages: p, ord: -o.ord}
		if err = back.from(first.Addr, visit); err != nil {
			yield(hit{}, err)
			return
		}
		if stop {
			return
		}
		fwd := scan{pages: p, ord: o.ord}
		if err = fwd.from(fwd.step(first), visit); err != nil {
			yield(hit{}, err)
		}
	}
}

// hasPrefix 按排序规则比前缀。
func hasPrefix(s, prefix string, coll xcoll.Collation) bool {
	if len(s) < len(prefix) {
		return false
	}
	return coll.Equal(s[:len(prefix)], prefix)
}

// likePrefix 取模式里第一个通配符之前的部分；没有通配符时整个模式都是前缀。
func likePrefix(pattern string) string {
	i := strings.IndexAny(pattern, "%_")
	if i < 0 {
		return pattern
	}
	return pattern[:i]
}

// opIn 依次对一组值做等值查。
type opIn struct {
	name   string
	values []*xbson.Value
	ord    Order
}

// indexName 返回索引名。
func (o *opIn) indexName() string { return o.name }

// order 返回遍历方向。
func (o *opIn) order() Order { return o.ord }

// setOrder 改遍历方向。
func (o *opIn) setOrder(v Order) { o.ord = v }

// keyOrdered 为假：产出按值列表的次序，不是按键的次序。
func (o *opIn) keyOrdered() bool { return false }

// cost 是单次等值查的代价乘以值的个数。
func (o *opIn) cost(ix *xpage.CollectionIndex) uint32 {
	unit := costEquals
	if ix.Unique {
		unit = costEqualsUnique
	}
	return uint32(len(o.values)) * unit
}

// mode 返回访问方式的描述，带上全部候选值。
func (o *opIn) mode() string {
	parts := make([]string, len(o.values))
	for i, v := range o.values {
		parts[i] = v.String()
	}
	return fmt.Sprintf("INDEX SEEK(%s IN [%s])", o.name, strings.Join(parts, ","))
}

// execute 逐个值做等值查。
func (o *opIn) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	coll xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		for _, v := range o.values {
			eq := &opEquals{name: o.name, value: v}
			for h, err := range eq.execute(p, ix, coll) {
				if err != nil {
					yield(hit{}, err)
					return
				}
				if !yield(h, nil) {
					return
				}
			}
		}
	}
}

// dedupValues 按排序规则去掉重复的值，保持原次序。
//
// 两两相比，值多了会慢；IN 的列表通常很短，够用。
func dedupValues(vals []*xbson.Value, coll xcoll.Collation) []*xbson.Value {
	out := make([]*xbson.Value, 0, len(vals))
	for _, v := range vals {
		dup := false
		for _, u := range out {
			if u.Compare(v, coll) == 0 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// opScan 整条链扫过去，用 keep 逐个筛。等值和区间都表达不了的条件走这条路。
type opScan struct {
	name string
	desc string
	ord  Order
	// keep 判断一个键要不要留下。
	keep func(k *xbson.Value) bool
}

// indexName 返回索引名。
func (o *opScan) indexName() string { return o.name }

// order 返回遍历方向。
func (o *opScan) order() Order { return o.ord }

// setOrder 改遍历方向。
func (o *opScan) setOrder(v Order) { o.ord = v }

// keyOrdered 为真：沿链走，筛掉一些不影响次序。
func (o *opScan) keyOrdered() bool { return true }

// cost 恒为 costScan：要走完整条链，但比什么索引都不用强。
func (o *opScan) cost(*xpage.CollectionIndex) uint32 { return costScan }

// mode 返回访问方式的描述。
func (o *opScan) mode() string { return fmt.Sprintf("FULL INDEX SCAN(%s %s)", o.name, o.desc) }

// execute 走完整条链，只留下 keep 认可的。
func (o *opScan) execute(p xstore.Pages, ix *xpage.CollectionIndex,
	_ xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		for n, err := range xstore.New(p).List(ix).FindAll(o.ord) {
			if err != nil {
				yield(hit{}, err)
				return
			}
			h, err := readHit(n)
			if err != nil {
				yield(hit{}, err)
				return
			}
			if o.keep(&h.key) && !yield(h, nil) {
				return
			}
		}
	}
}
