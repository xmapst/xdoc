package xdoc

import (
	"fmt"
	"iter"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// Expr 是一条编译好的表达式，可以对多篇文档反复求值。
//
// 它自带比较规则与参数编码方式：从 [Compile] 来的用默认的那套，从 [DB.Compile]
// 来的用库自己那套。同一条表达式在两种规则下结果可以不同——"ABC" = "abc"
// 在忽略大小写的库上为真，按码元的库上为假。
type Expr struct {
	node xbexpr.Node
	src  string
	coll xcoll.Collation

	marshal func(any) (*Value, error)
}

// Compile 编译一条表达式，用默认的比较规则与默认映射器编码参数。
//
// 要跟着某个库走（比较规则、映射器都用它的），用 [DB.Compile]。
func Compile(expr string) (*Expr, error) {
	return compileExpr(expr, xcoll.Default, func(v any) (*Value, error) { return Val(v), nil })
}

// Compile 编译一条表达式，用这个库的比较规则与映射器。
//
// 与包级的 [Compile] 的分别只在这两处，而它们会改变结果：比较规则决定
// "ABC" = "abc" 真不真，映射器决定参数编成什么值。
func (db *DB) Compile(expr string) (*Expr, error) {
	return compileExpr(expr, db.Collation(), db.Marshal)
}

// compileExpr 是两个入口的共同实现：解析、归一、连同规则与编码器一起存下来。
func compileExpr(expr string, coll xcoll.Collation, marshal func(any) (*Value, error)) (*Expr, error) {
	n, err := xbexpr.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("xdoc: expression %q: %w", expr, err)
	}
	return &Expr{node: n, src: xbexpr.Print(n), coll: coll, marshal: marshal}, nil
}

// Source 返回归一之后的源串，不是调用方写的原文。
//
// 归一规则是格式的一部分：索引的取键表达式存的就是这个串，写出的字节必须一致。
func (e *Expr) Source() string { return e.src }

// String 与 [Expr.Source] 一样，返回归一之后的源串。
func (e *Expr) String() string { return e.src }

// Indexable 报告这条表达式能不能拿来建索引。
//
// 要求有三条，判据落在整棵树上：至少引用一处文档字段、用到的方法都不易变
// （NOW()、GUID()、RANDOM() 这些不行）、不带参数。所以 CONCAT($.a, GUID())
// 也不行。不满足的表达式建出来的索引查不到自己写进去的记录，而且删不掉
// ——删除要先按键查到它。
func (e *Expr) Indexable() bool { return xbexpr.IsIndexable(e.node) }

// Value 对一篇文档求出**一个**值。
//
// 遇到会摊开成多个值的表达式（$.tags[*]）报错；要那些值用 [Expr.Values]。
// doc 为 nil 表示与文档无关，`1 + 2` 这类照样求得出。
func (e *Expr) Value(doc *Document, args ...any) (*Value, error) {
	params, err := e.params(args)
	if err != nil {
		return nil, err
	}
	return xbexpr.ExecuteScalar(e.node, docValue(doc), params, e.coll)
}

// Values 对一篇文档求值并摊开成零到多个值。
//
// 参数转换失败时不 panic，而是让迭代的第一步交出那个错误。
func (e *Expr) Values(doc *Document, args ...any) iter.Seq2[*Value, error] {
	params, err := e.params(args)
	if err != nil {
		return func(yield func(*Value, error) bool) { yield(nil, err) }
	}
	return xbexpr.Execute(e.node, docValue(doc), params, e.coll)
}

// params 把调用方给的实参整理成参数文档。
//
// 三种写法：单个 *Document 或文档值直接用；单个 map[string]any 按键绑定；
// 其余按位置绑定，参数名是下标（@0、@1）。除前两种之外的值都过这条表达式
// 自带的编码器，所以从 [DB.Compile] 来的表达式认得库上注册的自定义类型。
func (e *Expr) params(args []any) (*Document, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if len(args) == 1 {
		switch v := args[0].(type) {
		case *Document:
			return v, nil
		case *Value:
			if d, ok := v.AsDocument(); ok {
				return d, nil
			}
		case map[string]any:
			out := xbson.NewDocument()
			for k, x := range v {
				val, err := e.marshal(x)
				if err != nil {
					return nil, fmt.Errorf("xdoc: expression parameter %q: %w", k, err)
				}
				out.Set(k, val)
			}
			return out, nil
		}
	}
	out := xbson.NewDocument()
	for i, a := range args {
		val, err := e.marshal(a)
		if err != nil {
			return nil, fmt.Errorf("xdoc: expression parameter %d: %w", i, err)
		}
		out.Set(strconv.Itoa(i), val)
	}
	return out, nil
}

// docValue 把文档包成值，nil 文档给 nil 而不是一篇空文档——求值器靠这个区分。
func docValue(d *Document) *Value {
	if d == nil {
		return nil
	}
	return d.Value()
}

// Eval 编译并求出一个值，是 [Compile] 加 [Expr.Value] 的快捷写法。
//
// 同一条表达式要对多篇文档求值时别用它：那样每篇都重新解析一遍。
func Eval(expr string, doc *Document, args ...any) (*Value, error) {
	e, err := Compile(expr)
	if err != nil {
		return nil, err
	}
	return e.Value(doc, args...)
}

// Eval 用这个库的比较规则与映射器求出一个值。
func (db *DB) Eval(expr string, doc *Document, args ...any) (*Value, error) {
	e, err := db.Compile(expr)
	if err != nil {
		return nil, err
	}
	return e.Value(doc, args...)
}
