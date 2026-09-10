package xquery

import (
	"fmt"
	"strings"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
)

// maxWhereDepth 限住 WHERE 里与运算的嵌套层数，防住畸形查询把栈递归爆掉。
const maxWhereDepth = 256

// primaryIndexName 是主键索引名，primaryIndexExpr 是它的表达式。挑不出别的索引时就用它做全表扫描。
const primaryIndexName = "_id"

const primaryIndexExpr = "$._id"

// optimizer 是一次计划编排的中间状态。
//
// terms 是拆开的过滤条件，plan 是正在成形的计划。
type optimizer struct {
	q      *Query
	cp     *xpage.CollectionPage
	opts   evalOpts
	source *virtualSource

	terms []xbexpr.Node
	plan  *Plan

	// vectorOrderConsumed 记着排序已经由向量索引兑现，后面就不必再排一遍。
	vectorOrderConsumed bool
}

// optimize 把一条查询排成执行计划。
//
// **几步是有先后的**：先拆开过滤条件、做等价改写，才谈得上算用到哪些字段；
// 字段定了才挑得了索引；索引定了才知道排序和分组要不要自己再排一遍；
// 最后按前面的结果决定引用在哪个位置展开。
func (q *Query) optimize(collection string, cp *xpage.CollectionPage,
	source *virtualSource, opts evalOpts) (*Plan, error) {
	if err := q.normalize(); err != nil {
		return nil, err
	}
	o := &optimizer{
		q:      q,
		cp:     cp,
		opts:   opts,
		source: source,
		plan: &Plan{
			Collection: collection,
			ForUpdate:  q.ForUpdate,
			Select:     q.Select,
			SelectAll:  usesSource(q.Select),
			Offset:     q.Offset,
			Limit:      q.Limit,
		},
	}
	for _, step := range []func() error{
		o.splitTerms,
		o.rewriteTerms,
		o.defineFields,
		o.defineIndex,
		o.defineOrderBy,
		o.defineGroupBy,
		o.defineIncludes,
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return o.plan, nil
}

// splitTerms 把 WHERE 按与运算拆成一条条独立的条件。
//
// 拆开是为了让每一条都能单独拿去比索引。或运算不拆——它整体才是一个条件。
// 过滤条件里不许用数据源本身。
func (o *optimizer) splitTerms() error {
	var add func(n xbexpr.Node, depth int) error
	add = func(n xbexpr.Node, depth int) error {
		if depth > maxWhereDepth {
			return fmt.Errorf("%w: WHERE nesting deeper than %d", ErrQuery, maxWhereDepth)
		}
		if n == nil {
			return fmt.Errorf("%w: WHERE contains a nil expression", ErrQuery)
		}
		if usesSource(n) {
			return fmt.Errorf("%w: WHERE filter cannot use `*` in `%s`", ErrQuery, sourceText(n))
		}
		u := unparen(n)
		b := binaryOf(u)
		if b == nil {
			return fmt.Errorf("%w: `%s` is not a valid predicate", ErrQuery, sourceText(n))
		}
		switch {
		case b.Op.IsPredicate() || b.Op == xbexpr.OpOr:
			o.terms = append(o.terms, u)
			return nil
		case b.Op == xbexpr.OpAnd:
			if err := add(b.Left, depth+1); err != nil {
				return err
			}
			return add(b.Right, depth+1)
		}
		return fmt.Errorf("%w: `%s` is not a valid predicate", ErrQuery, sourceText(n))
	}
	for _, w := range o.q.Where {
		if err := add(w, 0); err != nil {
			return err
		}
	}
	return nil
}

// rewriteTerms 做一处等价改写：把「多值 = 单值路径」翻成「路径 IN 多值」。
//
// 改写之后左边是路径，右边是能先算出来的值，才有机会去查索引。
func (o *optimizer) rewriteTerms() error {
	for i, t := range o.terms {
		b := binaryOf(t)
		if b == nil || b.Left == nil || b.Right == nil {
			continue
		}
		if b.Op.Base() != xbexpr.OpEQ || b.Op.Quantifier() != xbexpr.QuantAny {
			continue
		}
		if b.Left.Cardinality() != xbexpr.Sequence {
			continue
		}
		if _, ok := b.Right.(*xbexpr.PathNode); !ok {
			continue
		}
		o.terms[i] = &xbexpr.BinaryNode{
			Op:    xbexpr.OpIn,
			Left:  b.Right,
			Right: &xbexpr.CallNode{Name: "ARRAY", Args: []xbexpr.Node{b.Left}},
		}
	}
	return nil
}

// defineFields 收齐查询各处引用到的顶层字段名，按大小写不敏感去重。
//
// **只要有一处引用了整篇文档，字段表就清空**——那表示什么都得读，
// 再列字段没有意义。
func (o *optimizer) defineFields() error {
	var all []xbexpr.Node
	all = append(all, o.q.Select)
	all = append(all, o.terms...)
	all = append(all, o.q.Includes...)
	if o.q.GroupBy != nil {
		all = append(all, o.q.GroupBy)
	}
	if o.q.Having != nil {
		all = append(all, o.q.Having)
	}
	for _, s := range o.q.OrderBy {
		all = append(all, s.Expr)
	}

	seen := map[string]bool{}
	var fields []string
	for _, n := range all {
		for _, f := range fieldsOf(n) {
			k := strings.ToUpper(f)
			if seen[k] {
				continue
			}
			seen[k] = true
			fields = append(fields, f)
		}
	}
	if seen["$"] {
		fields = nil
	}
	o.plan.Fields = fields
	return nil
}

// candidate 是一个备选的索引访问方式：怎么访问、代价多少、兑现了哪条过滤条件、索引表达式是什么。
type candidate struct {
	op   indexOp
	cost uint32

	term xbexpr.Node

	expr string
}

// defineIndex 挑定索引访问方式，剩下的过滤条件留给流水线。
//
// 虚拟源没得挑。其余先看向量索引，再按代价挑普通索引，都挑不出就全表扫描。
// 被索引兑现的那条条件不必再过一遍滤。
//
// 如果查询只用到一个字段、而索引正好就建在这个字段上，那连数据块都不用读。
func (o *optimizer) defineIndex() error {
	var selected xbexpr.Node

	switch {
	case o.source != nil:
		o.plan.index = &opVirtual{src: o.source}
		o.plan.IndexCost = 0
		o.plan.IndexExpr = ""
	default:
		c, err := o.trySelectVectorIndex()
		if err != nil {
			return err
		}
		if c == nil {
			if c, err = o.chooseIndex(); err != nil {
				return err
			}
		}
		if c == nil {
			c = &candidate{
				op:   &opAll{name: primaryIndexName, ord: Ascending},
				cost: costAll,
				expr: primaryIndexExpr,
			}
		}
		o.plan.index = c.op
		o.plan.IndexCost = c.cost
		o.plan.IndexExpr = c.expr
		selected = c.term
	}

	if _, vec := o.plan.index.(*opVector); !vec &&
		len(o.plan.Fields) == 1 && o.plan.IndexExpr == "$."+o.plan.Fields[0] {
		o.plan.IsIndexKeyOnly = true
	}

	for _, t := range o.terms {
		if selected != nil && sameNode(t, selected) {
			continue
		}
		o.plan.Filters = append(o.plan.Filters, t)
	}
	return nil
}

// sameNode 比的是**是不是同一个节点**，不是长得一不一样。
func sameNode(a, b xbexpr.Node) bool { return a == b }

// chooseIndex 在过滤条件里挑一个代价最低的索引。
//
// 一条都挑不出时退一步：如果查询要分组或排序，或者只涉及一个字段，
// 就找一个建在那个表达式上的索引来全扫——扫出来是有序的，
// 后面能省掉一次排序。
func (o *optimizer) chooseIndex() (*candidate, error) {
	if o.cp == nil {
		return nil, nil
	}
	indexes := o.cp.Indexes()
	if o.q.PrimaryOnly {
		indexes = nil
		if ix, ok := o.cp.Index(primaryIndexName); ok {
			indexes = []xpage.CollectionIndex{*ix}
		}
	}

	preferred := ""
	if len(o.plan.Fields) == 1 {
		preferred = "$." + o.plan.Fields[0]
	}

	var best *candidate
	for _, t := range o.terms {
		if !isPredicate(t) {
			continue
		}
		c, err := o.candidateFor(indexes, t)
		if err != nil {
			return nil, err
		}
		if c == nil {
			continue
		}

		if best == nil || c.cost < best.cost {
			best = c
		}
	}
	if best != nil {
		return best, nil
	}

	if len(o.q.OrderBy) == 0 && o.q.GroupBy == nil && preferred == "" {
		return nil, nil
	}
	var wanted []string
	if o.q.GroupBy != nil {
		wanted = append(wanted, sourceText(o.q.GroupBy))
	}
	if len(o.q.OrderBy) > 0 {
		wanted = append(wanted, sourceText(o.q.OrderBy[0].Expr))
	}
	if preferred != "" {
		wanted = append(wanted, preferred)
	}
	for _, w := range wanted {
		if ix := findIndex(indexes, w); ix != nil {
			return &candidate{
				op:   &opAll{name: ix.Name, ord: Ascending},
				cost: costAll,
				expr: canonicalExpr(ix.Expression),
			}, nil
		}
	}
	return nil, nil
}

// candidateFor 看一条过滤条件能不能落到某个索引上。
//
// 两边哪边是索引表达式、哪边是能先算出来的值，都试一遍；
// 值在左边时把比较方向翻过来。**值必须正好算出一个**，多了少了都不认。
func (o *optimizer) candidateFor(indexes []xpage.CollectionIndex, term xbexpr.Node) (*candidate, error) {
	b := binaryOf(term)
	if b == nil || b.Left == nil || b.Right == nil {
		return nil, nil
	}

	var ix *xpage.CollectionIndex
	var value xbexpr.Node

	switch {
	case b.Left.Cardinality() == xbexpr.Sequence && b.Right.Cardinality() == xbexpr.Scalar:
		if b.Op.Quantifier() != xbexpr.QuantAny {
			return nil, nil
		}
		if !isValueExpr(b.Right) {
			return nil, nil
		}
		ix, value = findIndex(indexes, sourceText(b.Left)), b.Right
	default:
		if c := findIndex(indexes, sourceText(b.Left)); c != nil && isValueExpr(b.Right) {
			ix, value = c, b.Right
		} else if c := findIndex(indexes, sourceText(b.Right)); c != nil && isValueExpr(b.Left) {
			ix, value = c, b.Left
		}
	}
	if ix == nil {
		return nil, nil
	}

	op := b.Op.Base()
	if isValueExpr(b.Left) {
		op = flipComparison(op)
	}

	v, n, err := o.opts.scalarValue(value, o.q.Params)
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, nil
	}
	idxOp := o.createIndexOp(op, ix.Name, v)
	if idxOp == nil {
		return nil, nil
	}
	return &candidate{
		op:   idxOp,
		cost: idxOp.cost(ix),
		term: term,
		expr: canonicalExpr(ix.Expression),
	}, nil
}

// flipComparison 把比较运算左右调个个儿：大于变小于，如此类推。相等之类原样返回。
func flipComparison(op xbexpr.Operator) xbexpr.Operator {
	switch op {
	case xbexpr.OpGT:
		return xbexpr.OpLT
	case xbexpr.OpGTE:
		return xbexpr.OpLTE
	case xbexpr.OpLT:
		return xbexpr.OpGT
	case xbexpr.OpLTE:
		return xbexpr.OpGTE
	default:
	}

	return op
}

// createIndexOp 按比较运算造出对应的索引访问方式。
//
// 单边的比较用最小值或最大值补齐成一个闭区间。不等要逐条比，
// 只能扫着过滤。IN 的值不是数组时退化成等值查。
func (o *optimizer) createIndexOp(op xbexpr.Operator, name string, v *xbson.Value) indexOp {
	switch op {
	case xbexpr.OpEQ:
		return &opEquals{name: name, value: v}
	case xbexpr.OpBetween:
		a, ok := v.AsArray()
		if !ok || a.Len() != 2 {
			return nil
		}
		return &opRange{name: name, start: a.At(0), end: a.At(1), startEq: true, endEq: true, ord: Ascending}
	case xbexpr.OpLike:
		s, ok := v.AsString()
		if !ok {
			return nil
		}
		return o.newLike(name, s)
	case xbexpr.OpGT:
		return &opRange{name: name, start: v, end: xbson.MaxValue, startEq: false, endEq: true, ord: Ascending}
	case xbexpr.OpGTE:
		return &opRange{name: name, start: v, end: xbson.MaxValue, startEq: true, endEq: true, ord: Ascending}
	case xbexpr.OpLT:
		return &opRange{name: name, start: xbson.MinValue, end: v, startEq: true, endEq: false, ord: Ascending}
	case xbexpr.OpLTE:
		return &opRange{name: name, start: xbson.MinValue, end: v, startEq: true, endEq: true, ord: Ascending}
	case xbexpr.OpNE:
		coll := o.opts.coll
		return &opScan{
			name: name,
			desc: "!= " + v.String(),
			ord:  Ascending,
			keep: func(k *xbson.Value) bool { return k.Compare(v, coll) != 0 },
		}
	case xbexpr.OpIn:
		a, ok := v.AsArray()
		if !ok {
			return &opEquals{name: name, value: v}
		}
		return &opIn{name: name, values: dedupValues(a.Items(), o.opts.coll), ord: Ascending}
	default:
	}
	return nil
}

// newLike 造一个前缀匹配的索引访问方式。
//
// 匹配函数复用同一棵表达式树，每次只改左边那个常量，省掉反复搭树的开销。
// **这也意味着它不能并发调用。**
func (o *optimizer) newLike(name, pattern string) indexOp {
	left := &xbexpr.ConstNode{}
	node := &xbexpr.BinaryNode{
		Op:    xbexpr.OpLike,
		Left:  left,
		Right: &xbexpr.ConstNode{Value: xbson.String(pattern)},
	}
	opts := o.opts
	return &opLike{
		name:    name,
		pattern: pattern,
		prefix:  likePrefix(pattern),
		ord:     Ascending,
		match: func(s string) (bool, error) {
			left.Value = xbson.String(s)

			r, err := xbexpr.ExecuteScalar(node, nil, nil, opts.coll)
			if err != nil {
				return false, err
			}
			ok, _ := r.AsBoolean()
			return ok, nil
		},
	}
}

// findIndex 按规范化后的表达式找索引。
func findIndex(indexes []xpage.CollectionIndex, expr string) *xpage.CollectionIndex {
	if expr == "" {
		return nil
	}
	for i := range indexes {
		if canonicalExpr(indexes[i].Expression) == expr {
			return &indexes[i]
		}
	}
	return nil
}

// defineOrderBy 决定排序要不要自己再做一遍。
//
// 向量索引已经按距离排好了，直接作罢。否则看第一级排序键是不是正好就是
// 索引表达式：是的话把索引调成那个方向，**且只有一级排序时才算彻底兑现**，
// 多级的还得自己排。
//
// 分组查询的排序对的是分组结果，文档流上的顺序兑现不了，原样留给分组那一段。
func (o *optimizer) defineOrderBy() error {
	if len(o.q.OrderBy) == 0 {
		return nil
	}
	if o.q.GroupBy != nil {
		o.plan.OrderBy = o.q.OrderBy
		return nil
	}
	if o.vectorOrderConsumed {
		o.plan.OrderBy = nil
		return nil
	}
	segs := o.q.OrderBy
	if o.plan.index != nil &&
		o.plan.IndexExpr != "" &&
		sourceText(segs[0].Expr) == o.plan.IndexExpr &&
		o.plan.index.keyOrdered() {
		o.plan.index.setOrder(segs[0].Order)
		if len(segs) == 1 {
			return nil
		}
	}
	o.plan.OrderBy = segs
	return nil
}

// defineGroupBy 排出分组那一段。
//
// 分组只并相邻的，所以除非索引已经按分组键有序，否则得先补一次排序。
func (o *optimizer) defineGroupBy() error {
	if o.q.GroupBy == nil {
		return nil
	}
	g := &GroupPlan{
		Expr:   o.q.GroupBy,
		Having: o.q.Having,
		Select: o.q.Select,
	}
	ordered := o.plan.index != nil &&
		o.plan.IndexExpr != "" &&
		sourceText(o.q.GroupBy) == o.plan.IndexExpr &&
		o.plan.index.keyOrdered()
	if !ordered {
		g.OrderBy = []OrderSegment{{Expr: o.q.GroupBy, Order: Ascending}}
	}
	o.plan.GroupBy = g
	return nil
}

// defineIncludes 决定每个引用在流水线的哪个位置展开。
//
// 过滤或排序要用到的必须提前展开；用不到的推到分页之后，能少展开很多条。
//
// **排序时会两边都放一份**：排序按地址把文档重新读回来，
// 之前展开的内容就丢了，得再展开一次。
func (o *optimizer) defineIncludes() error {
	for _, inc := range o.q.Includes {
		fs := fieldsOf(inc)
		if len(fs) != 1 {
			return fmt.Errorf("%w: INCLUDE `%s` must reference exactly one field", ErrQuery, sourceText(inc))
		}
		field := fs[0]
		used := false
		for _, f := range o.plan.Filters {
			if containsField(f, field) {
				used = true
				break
			}
		}
		if !used {
			for _, s := range o.plan.OrderBy {
				if containsField(s.Expr, field) {
					used = true
					break
				}
			}
		}
		if used {
			o.plan.IncludeBefore = append(o.plan.IncludeBefore, inc)
		}
		if !used || len(o.plan.OrderBy) > 0 {
			o.plan.IncludeAfter = append(o.plan.IncludeAfter, inc)
		}
	}
	return nil
}

// containsField 判断表达式里有没有引用某个字段，大小写不敏感。
func containsField(n xbexpr.Node, field string) bool {
	for _, f := range fieldsOf(n) {
		if strings.EqualFold(f, field) {
			return true
		}
	}
	return false
}
