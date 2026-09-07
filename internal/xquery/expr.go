package xquery

import (
	"strings"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
)

// fieldsOf 取表达式引用到的顶层字段名。
func fieldsOf(n xbexpr.Node) []string { return xbexpr.Fields(n) }

// isValueExpr 判断一个表达式与文档无关，也就是能脱离文档先算出来。
func isValueExpr(n xbexpr.Node) bool { return len(fieldsOf(n)) == 0 }

// usesSource 判断表达式里有没有用到数据源本身。
func usesSource(n xbexpr.Node) bool {
	for node := range xbexpr.Walk(n) {
		if _, ok := node.(*xbexpr.SourceNode); ok {
			return true
		}
	}
	return false
}

// unparen 剥掉最外层的所有括号。
func unparen(n xbexpr.Node) xbexpr.Node {
	for {
		p, ok := n.(*xbexpr.ParenNode)
		if !ok || p == nil || p.Inner == nil {
			return n
		}
		n = p.Inner
	}
}

// binaryOf 剥掉括号后取二元运算节点；不是就返回 nil。
func binaryOf(n xbexpr.Node) *xbexpr.BinaryNode {
	b, ok := unparen(n).(*xbexpr.BinaryNode)
	if !ok {
		return nil
	}
	return b
}

// isPredicate 判断一个表达式是不是比较运算——那类才可能拿去查索引。
func isPredicate(n xbexpr.Node) bool {
	b := binaryOf(n)
	return b != nil && b.Op.IsPredicate()
}

// sourceText 把表达式还原成文本。
func sourceText(n xbexpr.Node) string { return xbexpr.Print(n) }

// canonicalExpr 规范化一段表达式文本：解析再还原。解析不通就原样返回。
func canonicalExpr(src string) string {
	n, err := xbexpr.Parse(src)
	if err != nil {
		return src
	}
	return xbexpr.Print(n)
}

// scalarValue 脱离文档算出一个表达式的值，第二个返回值是它一共产出几个值。
//
// **最多数到 2 就停**：调用方只关心「恰好一个」还是「不止一个」。
func (opts evalOpts) scalarValue(n xbexpr.Node, params *xbson.Document) (*xbson.Value, int, error) {
	var first *xbson.Value
	count := 0
	for v, err := range xbexpr.Execute(n, nil, params, opts.coll) {
		if err != nil {
			return nil, 0, err
		}
		if count == 0 {
			first = v
		}
		count++
		if count > 1 {
			break
		}
	}
	if first == nil {
		return xbson.Null, count, nil
	}
	return first, count, nil
}

// substituteSource 把表达式里的数据源换成一个具名参数的展开。
//
// 递归重建整棵树而不是就地改：原树可能被别处共用。
func substituteSource(n xbexpr.Node, param string) xbexpr.Node {
	switch t := n.(type) {
	case nil:
		return nil
	case *xbexpr.SourceNode:
		return &xbexpr.CallNode{Name: "ITEMS", Args: []xbexpr.Node{&xbexpr.ParameterNode{Name: param}}}
	case *xbexpr.ParenNode:
		if t == nil {
			return n
		}
		return &xbexpr.ParenNode{Inner: substituteSource(t.Inner, param)}
	case *xbexpr.CallNode:
		if t == nil {
			return n
		}
		args := make([]xbexpr.Node, len(t.Args))
		for i, a := range t.Args {
			args[i] = substituteSource(a, param)
		}
		return &xbexpr.CallNode{Name: t.Name, Args: args}
	case *xbexpr.FuncNode:
		if t == nil {
			return n
		}
		args := make([]xbexpr.Node, len(t.Args))
		for i, a := range t.Args {
			args[i] = substituteSource(a, param)
		}
		return &xbexpr.FuncNode{
			Name:   t.Name,
			Input:  substituteSource(t.Input, param),
			Lambda: substituteSource(t.Lambda, param),
			Args:   args,
		}
	case *xbexpr.BinaryNode:
		if t == nil {
			return n
		}
		return &xbexpr.BinaryNode{
			Op:    t.Op,
			Left:  substituteSource(t.Left, param),
			Right: substituteSource(t.Right, param),
		}
	case *xbexpr.ArrayNode:
		if t == nil {
			return n
		}
		items := make([]xbexpr.Node, len(t.Items))
		for i, it := range t.Items {
			items[i] = substituteSource(it, param)
		}
		return &xbexpr.ArrayNode{Items: items}
	case *xbexpr.DocumentNode:
		if t == nil {
			return n
		}
		fs := make([]xbexpr.DocField, len(t.Fields))
		for i, f := range t.Fields {
			fs[i] = xbexpr.DocField{Key: f.Key, Value: substituteSource(f.Value, param)}
		}
		return &xbexpr.DocumentNode{Fields: fs}
	case *xbexpr.PathNode:
		if t == nil || len(t.Steps) == 0 {
			return n
		}
		steps := make([]xbexpr.PathStep, len(t.Steps))
		copy(steps, t.Steps)
		for i := range steps {
			if steps[i].Expr != nil {
				steps[i].Expr = substituteSource(steps[i].Expr, param)
			}
		}
		return &xbexpr.PathNode{Steps: steps, Root: t.Root, Scope: t.Scope}
	}
	return n
}

// defaultFieldName 由表达式引用到的字段名推一个列名；推不出时用 expr。
func defaultFieldName(n xbexpr.Node) string {
	var parts []string
	for _, f := range fieldsOf(n) {
		if f != "$" {
			parts = append(parts, f)
		}
	}
	if len(parts) == 0 {
		return "expr"
	}
	return strings.Join(parts, "_")
}
