package xquery

import (
	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
)

const (
	// 索引代价，越小越优先。这几个数只用来在候选索引之间排序，没有别的含义。
	//
	// 命中唯一索引的等值查最便宜，普通等值次之，范围再次之，
	// 用索引但要逐条过滤更贵，全扫最贵。
	costEqualsUnique uint32 = 1

	costEquals uint32 = 10

	costRange uint32 = 20

	costScan uint32 = 80

	costAll uint32 = 100
)

// GroupPlan 是分组那一段的计划。
type GroupPlan struct {
	// Expr 是分组键。
	Expr xbexpr.Node

	// Having 是分组之后的过滤条件。
	Having xbexpr.Node

	// Select 是对每一组算的投影表达式。
	Select xbexpr.Node

	// OrderBy 是分组之后的排序。
	OrderBy []OrderSegment
}

// Plan 是排好的执行计划。
//
// 字段的排列是按大小归类的，与执行次序无关。执行次序见 pipe.go。
type Plan struct {
	// index 是选中的索引访问方式，nil 表示这次不走索引。
	index          indexOp
	Select         xbexpr.Node
	GroupBy        *GroupPlan
	Collection     string
	IndexExpr      string
	Fields         []string
	Filters        []xbexpr.Node
	IncludeBefore  []xbexpr.Node
	IncludeAfter   []xbexpr.Node
	OrderBy        []OrderSegment
	Offset         int
	Limit          int
	IndexCost      uint32
	ForUpdate      bool
	IsIndexKeyOnly bool
	SelectAll      bool
}

// IndexName 返回选中的索引名；没走索引时为空串。
func (p *Plan) IndexName() string {
	if p.index == nil {
		return ""
	}
	return p.index.indexName()
}

// IndexMode 返回索引的访问方式描述，比如全表扫描或等值查找。
func (p *Plan) IndexMode() string {
	if p.index == nil {
		return ""
	}
	return p.index.mode()
}

// IndexOrder 返回索引的遍历方向。
func (p *Plan) IndexOrder() Order {
	if p.index == nil {
		return Ascending
	}
	return p.index.order()
}

// Explain 把计划写成一篇文档，供 EXPLAIN 输出。
//
// **字段名 snaphost 是拼错的**，但它是输出格式的一部分，改了会打断依赖它的调用方。
func (p *Plan) Explain() *xbson.Document {
	d := xbson.NewDocument()
	d.Set("collection", xbson.String(p.Collection))

	d.Set("snaphost", xbson.String(snapshotMode(p.ForUpdate)))
	d.Set("pipe", xbson.String(pipeName(p.GroupBy != nil)))

	idx := xbson.NewDocument()
	if n := p.IndexName(); n != "" {
		idx.Set("name", xbson.String(n))
	} else {
		idx.Set("name", xbson.Null)
	}
	if virtual := p.index != nil && p.isVirtual(); virtual {
		idx.Set("expr", xbson.Null)
		idx.Set("order", xbson.Int32(0))
	} else {
		idx.Set("expr", xbson.String(p.IndexExpr))
		idx.Set("order", xbson.Int32(int32(p.IndexOrder())))
	}
	idx.Set("mode", xbson.String(p.IndexMode()))
	idx.Set("cost", xbson.Int32(int32(p.IndexCost)))
	d.Set("index", idx.Value())

	lookup := xbson.NewDocument()
	lookup.Set("loader", xbson.String(p.loaderName()))
	if len(p.Fields) == 0 {
		lookup.Set("fields", xbson.String("$"))
	} else {
		lookup.Set("fields", stringArray(p.Fields).Value())
	}
	d.Set("lookup", lookup.Value())

	if len(p.IncludeBefore) > 0 {
		d.Set("includeBefore", exprArray(p.IncludeBefore).Value())
	}
	if len(p.Filters) > 0 {
		d.Set("filters", exprArray(p.Filters).Value())
	}
	if len(p.OrderBy) > 0 {
		d.Set("orderBy", orderArray(p.OrderBy).Value())
	}
	if p.Limit != NoLimit {
		d.Set("limit", xbson.Int32(int32(p.Limit)))
	}
	if p.Offset != 0 {
		d.Set("offset", xbson.Int32(int32(p.Offset)))
	}
	if len(p.IncludeAfter) > 0 {
		d.Set("includeAfter", exprArray(p.IncludeAfter).Value())
	}

	if p.GroupBy != nil {
		g := xbson.NewDocument()
		g.Set("expr", xbson.String(sourceText(p.GroupBy.Expr)))
		g.Set("having", optionalSource(p.GroupBy.Having))
		g.Set("select", optionalSource(p.GroupBy.Select))
		if len(p.GroupBy.OrderBy) > 0 {
			g.Set("orderBy", orderArray(p.GroupBy.OrderBy).Value())
		}
		d.Set("groupBy", g.Value())
	} else {
		s := xbson.NewDocument()
		s.Set("expr", xbson.String(sourceText(p.Select)))
		s.Set("all", xbson.Boolean(p.SelectAll))
		d.Set("select", s.Value())
	}
	return d
}

// loaderName 说明每条结果是怎么取回来的。
//
// 不走索引或要取整篇文档时是 document；虚拟数据源是 virtual；
// 只需要索引键本身时是 index——那种情况连数据块都不用读。
func (p *Plan) loaderName() string {
	switch {
	case p.index == nil:
		return "document"
	case p.isVirtual():
		return "virtual"
	case p.IsIndexKeyOnly:
		return "index"
	}
	return "document"
}

// snapshotMode 把是否要写锁翻成描述文本。
func snapshotMode(forUpdate bool) string {
	if forUpdate {
		return "write"
	}
	return "read"
}

// pipeName 按分不分组给出流水线的名字。
func pipeName(grouped bool) string {
	if grouped {
		return "groupByPipe"
	}
	return "queryPipe"
}

// stringArray 把一串字符串装成数组值。
func stringArray(ss []string) *xbson.Array {
	a := xbson.NewArray()
	for _, s := range ss {
		a.Append(xbson.String(s))
	}
	return a
}

// exprArray 把一串表达式还原成文本再装成数组值。
func exprArray(ns []xbexpr.Node) *xbson.Array {
	a := xbson.NewArray()
	for _, n := range ns {
		a.Append(xbson.String(sourceText(n)))
	}
	return a
}

// orderArray 把排序键装成数组值。
func orderArray(segs []OrderSegment) *xbson.Array {
	a := xbson.NewArray()
	for _, s := range segs {
		d := xbson.NewDocument()
		d.Set("expr", xbson.String(sourceText(s.Expr)))
		d.Set("order", xbson.Int32(int32(s.Order)))
		a.Append(d.Value())
	}
	return a
}

// optionalSource 把一个可能为空的表达式还原成文本；为空时给 Null。
func optionalSource(n xbexpr.Node) *xbson.Value {
	if n == nil {
		return xbson.Null
	}
	return xbson.String(sourceText(n))
}
