package xbexpr

import (
	"cmp"
	"fmt"
	"iter"
	"slices"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// MaxEvalDepth 是求值时的递归深度上限。
//
// 比解析期的 [MaxDepth] 宽：求值会经过路径、lambda 这些解析时不算一层的地方。
const MaxEvalDepth = 512

// MaxEvalValues 是一次求值最多能产出多少个值。
//
// 序列可以嵌套展开，[*] 套 MAP 套 [*] 很容易炸开；这个额度让失控的表达式
// 早点停下来，而不是把内存吃光。
const MaxEvalValues = 1 << 22

var (
	// ErrNotScalar 表示要一个值的地方遇上了产出一串值的表达式。
	ErrNotScalar = fmt.Errorf("%w: expression is not scalar", ErrExpr)

	// ErrEvalTooDeep 表示求值时的嵌套超过了 [MaxEvalDepth]。
	ErrEvalTooDeep = fmt.Errorf("%w: expression nesting too deep to evaluate", ErrExpr)

	// ErrTooManyValues 表示产出的值超过了 [MaxEvalValues]。
	ErrTooManyValues = fmt.Errorf("%w: expression produced too many values", ErrExpr)
)

// Execute 对一篇文档求值，产出一串值。
//
// 标量表达式也能这么用，它产出恰好一个值。
func Execute(n Node, root *xbson.Value, params *xbson.Document, coll xcoll.Collation) iter.Seq2[*xbson.Value, error] {
	return func(yield func(*xbson.Value, error) bool) {
		for v, err := range newEnv(root, params, coll).evalSeq(n) {
			if !yield(v, err) {
				return
			}
		}
	}
}

// ExecuteAggregate 在没有文档根的环境里求值，供分组之后的聚合表达式用。
//
// 此时直接引用文档字段是错的——分组之后那些字段已经没有唯一的取值了，
// 见 [PathNode.walkSteps] 里对 noRoot 的处理。
func ExecuteAggregate(n Node, params *xbson.Document, coll xcoll.Collation) iter.Seq2[*xbson.Value, error] {
	return func(yield func(*xbson.Value, error) bool) {
		for v, err := range newEnvNoRoot(nil, params, coll).evalSeq(n) {
			if !yield(v, err) {
				return
			}
		}
	}
}

// ExecuteAggregateScalar 同 [ExecuteAggregate]，但只取一个值，表达式产出一串时报错。
func ExecuteAggregateScalar(n Node, params *xbson.Document, coll xcoll.Collation) (*xbson.Value, error) {
	if n != nil && n.Cardinality() == Sequence {
		return nil, fmt.Errorf("%w: `%s` returns more than one value", ErrNotScalar, Print(n))
	}
	return newEnvNoRoot(nil, params, coll).evalScalar(n)
}

// ExecuteScalar 对一篇文档求值并只取一个值，表达式产出一串时报错。
func ExecuteScalar(n Node, root *xbson.Value, params *xbson.Document, coll xcoll.Collation) (*xbson.Value, error) {
	if n != nil && n.Cardinality() == Sequence {
		return nil, fmt.Errorf("%w: `%s` returns more than one value", ErrNotScalar, Print(n))
	}
	return newEnv(root, params, coll).evalScalar(n)
}

// IndexKeys 算出一篇文档在某个索引里该占的那些键，去重后按出现次序返回。
//
// 去重靠一个有序表加二分查找，比较时先比类型再比值，且一律按二进制序——
// 索引键的相等要看字节，不看排序规则。数组、文档、二进制三类不参与去重：
// 它们不作为索引键比较，每次出现都单独算一个。
//
// 求值前先把文档里的时间统一到 loc：索引键存的是绝对时刻，
// 带着不同时区进来会算出不同的键。
func IndexKeys(n Node, doc *xbson.Value, coll xcoll.Collation,
	loc *time.Location) ([]*xbson.Value, error) {
	var out []*xbson.Value

	var sorted []*xbson.Value
	for v, err := range Execute(n, retagDates(doc, loc), nil, coll) {
		if err != nil {
			return nil, err
		}
		if neverSameKey(v) {
			out = append(out, v)
			continue
		}
		i, found := slices.BinarySearchFunc(sorted, v, compareIndexKey)
		if found {
			continue
		}
		sorted = slices.Insert(sorted, i, v)
		out = append(out, v)
	}
	return out, nil
}

// neverSameKey 判断这个值属于不参与去重的那几类。
func neverSameKey(v *xbson.Value) bool {
	switch v.Type() {
	case xbson.TypeArray, xbson.TypeDocument, xbson.TypeBinary:
		return true
	}
	return false
}

// compareIndexKey 按类型加二进制序比较两个索引键。
func compareIndexKey(a, b *xbson.Value) int {
	if c := cmp.Compare(int(a.Type()), int(b.Type())); c != 0 {
		return c
	}
	return a.Compare(b, xcoll.Binary)
}

// retagDates 把值里所有时间换到 loc 上，递归进文档与数组。
//
// 先扫一遍看有没有需要换的，没有就原样返回——绝大多数文档不必重建。
func retagDates(v *xbson.Value, loc *time.Location) *xbson.Value {
	if !hasForeignDate(v, loc) {
		return v
	}
	switch v.Type() {
	case xbson.TypeDateTime:
		return v.In(loc)
	case xbson.TypeDocument:
		d, _ := v.AsDocument()
		out := xbson.NewDocument()
		for k, item := range d.Elements() {
			out.Set(k, retagDates(item, loc))
		}
		return out.Value()
	case xbson.TypeArray:
		a, _ := v.AsArray()
		out := xbson.NewArray()
		for _, item := range a.Items() {
			out.Append(retagDates(item, loc))
		}
		return out.Value()
	}
	return v
}

// hasForeignDate 判断值里有没有不在 loc 上的时间。
func hasForeignDate(v *xbson.Value, loc *time.Location) bool {
	switch v.Type() {
	case xbson.TypeDateTime:
		return v.Location() != loc
	case xbson.TypeDocument:
		d, _ := v.AsDocument()
		for _, item := range d.Elements() {
			if hasForeignDate(item, loc) {
				return true
			}
		}
	case xbson.TypeArray:
		a, _ := v.AsArray()
		for _, item := range a.Items() {
			if hasForeignDate(item, loc) {
				return true
			}
		}
	}
	return false
}

// budget 是一次求值的产出额度，全程共用一份。
type budget struct{ left int }

// take 扣掉一个额度，扣光时返回 false。
func (b *budget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// env 是一次求值的环境。
//
// 按值传递：往下走一层就复制一份改掉 current 或 depth，外层因此不受影响。
// 唯独额度是共享的指针，整棵树共用一份。
type env struct {
	// root 是整篇文档，路径里的 $ 指它。
	root *xbson.Value
	// current 是当前项，路径里的 @ 指它。
	current *xbson.Value
	// source 是数据源那一串值，[SourceNode] 产出它。
	source []*xbson.Value
	params *xbson.Document
	coll   xcoll.Collation
	depth  int
	b      *budget

	// noRoot 表示这一层不许直接引用文档字段，见 [ExecuteAggregate]。
	noRoot bool
}

// newEnvNoRoot 开一个没有文档根的环境。
func newEnvNoRoot(source []*xbson.Value, params *xbson.Document, coll xcoll.Collation) env {
	e := newEnv(nil, params, coll)
	e.source = source
	e.noRoot = true
	return e
}

// newEnv 开一个求值环境。root 为 nil 时给一篇空文档，免得处处判空。
func newEnv(root *xbson.Value, params *xbson.Document, coll xcoll.Collation) env {
	if root == nil {
		root = xbson.NewDocument().Value()
	}
	return env{
		root:    root,
		current: root,
		source:  []*xbson.Value{root},
		params:  params,
		coll:    coll,
		b:       &budget{left: MaxEvalValues},
	}
}

// ctx 把环境里方法能看到的那部分摘出来。
func (e env) ctx() *Ctx {
	return &Ctx{Root: e.root, Collation: e.coll, Params: e.params}
}

// withCurrent 换掉当前项，返回新的环境。
//
// 数据源同时重置成只有文档根一项：进了 lambda 之后，* 指的是这一篇文档。
func (e env) withCurrent(v *xbson.Value) env {
	e.current = v
	e.source = []*xbson.Value{e.root}
	return e
}

// deeper 下探一层，超过 [MaxEvalDepth] 就报错。
func (e env) deeper() (env, error) {
	if e.depth >= MaxEvalDepth {
		return e, fmt.Errorf("%w: limit is %d", ErrEvalTooDeep, MaxEvalDepth)
	}
	e.depth++
	return e, nil
}

// evalScalar 求一个值。
//
// 空节点算 Null。产出一串值的表达式在这里直接报错，并提示可以加 ANY/ALL
// 或者用 ARRAY() 收起来。
func (e env) evalScalar(n Node) (*xbson.Value, error) {
	if n == nil {
		return xbson.Null, nil
	}
	if n.Cardinality() == Sequence {
		return nil, fmt.Errorf("%w: `%s` returns more than one value, use ANY or ALL, or wrap it with ARRAY()",
			ErrNotScalar, Print(n))
	}
	e, err := e.deeper()
	if err != nil {
		return nil, err
	}

	switch t := n.(type) {
	case *ConstNode:
		return t.value(), nil
	case *ParameterNode:
		if e.params == nil || t == nil {
			return xbson.Null, nil
		}
		return e.params.Get(t.Name), nil
	case *ParenNode:
		if t == nil {
			return xbson.Null, nil
		}
		return e.evalScalar(t.Inner)
	case *PathNode:
		return t.pathScalar(e)
	case *DocumentNode:
		return t.documentLiteral(e)
	case *ArrayNode:
		return t.arrayLiteral(e)
	case *CallNode:
		return t.callScalar(e)
	case *FuncNode:
		return t.funcScalar(e)
	case *BinaryNode:
		return t.evalBinary(e)
	}
	return nil, errf("cannot evaluate a node of kind %d", n.Kind())
}

// evalSeq 求一串值。
//
// 标量表达式会被当成只有一项的序列。每产出一个值扣一份额度。
func (e env) evalSeq(n Node) iter.Seq2[*xbson.Value, error] {
	return func(yield func(*xbson.Value, error) bool) {
		if n == nil {
			return
		}
		if n.Cardinality() == Scalar {
			v, err := e.evalScalar(n)
			if err != nil {
				yield(nil, err)
				return
			}
			if !e.b.take() {
				yield(nil, ErrTooManyValues)
				return
			}
			yield(v, nil)
			return
		}

		e, err := e.deeper()
		if err != nil {
			yield(nil, err)
			return
		}

		switch t := n.(type) {
		case *ParenNode:
			if t == nil {
				return
			}
			for v, err := range e.evalSeq(t.Inner) {
				if !yield(v, err) {
					return
				}
			}
		case *SourceNode:
			for _, v := range e.source {
				if !e.emit(yield, v) {
					return
				}
			}
		case *PathNode:
			t.pathSeq(e, yield)
		case *CallNode:
			t.callSeq(e, yield)
		case *FuncNode:
			t.funcSeq(e, yield)
		default:
			yield(nil, errf("cannot evaluate a node of kind %d as a sequence", n.Kind()))
		}
	}
}

// emit 扣一份额度再产出一个值；额度用尽时产出 [ErrTooManyValues] 并中止。
func (e env) emit(yield func(*xbson.Value, error) bool, v *xbson.Value) bool {
	if !e.b.take() {
		yield(nil, ErrTooManyValues)
		return false
	}
	if v == nil {
		v = xbson.Null
	}
	return yield(v, nil)
}

// collect 把一个表达式产出的值全收进切片。
func (e env) collect(n Node) ([]*xbson.Value, error) {
	var out []*xbson.Value
	for v, err := range e.evalSeq(n) {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// scalarArg 求一个实参的标量值；表达式产出一串时收成数组，而不是像 [env.evalScalar] 那样报错。
func (e env) scalarArg(n Node) (*xbson.Value, error) {
	if n != nil && n.Cardinality() == Sequence {
		vals, err := e.collect(n)
		if err != nil {
			return nil, err
		}
		return seqOf(vals), nil
	}
	return e.evalScalar(n)
}

// seqValues 把一个实参当成一串值来取。
//
// 表达式本来就产出序列时直接用；否则先求出那一个值，再用 ITEMS() 摊开它——
// 数组会被摊成各项，其余值就是它自己一项。
func (e env) seqValues(n Node) iter.Seq2[*xbson.Value, error] {
	if n != nil && n.Cardinality() == Sequence {
		return e.evalSeq(n)
	}
	return func(yield func(*xbson.Value, error) bool) {
		v, err := e.evalScalar(n)
		if err != nil {
			yield(nil, err)
			return
		}
		spread, err := e.ctx().mITEMS([]*xbson.Value{v})
		if err != nil {
			yield(nil, err)
			return
		}
		for _, it := range items(spread) {
			if !e.emit(yield, it) {
				return
			}
		}
	}
}

// seqArg 同 [env.seqValues]，但把结果收成一个数组值，供 [ParamSeq] 形参使用。
func (e env) seqArg(n Node) (*xbson.Value, error) {
	var out []*xbson.Value
	for v, err := range e.seqValues(n) {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return seqOf(out), nil
}

// methodArgs 按方法签名逐个求出实参：标量形参走 scalarArg，序列形参走 seqArg。
//
// 连同查到的方法表项一起返回，调用方直接拿它去调。
func (n *CallNode) methodArgs(e env) ([]*xbson.Value, *def, error) {
	d := n.resolve()
	if d == nil {
		return nil, nil, errf("method %s does not exist or contains invalid parameters", upperASCII(n.Name))
	}
	args := make([]*xbson.Value, len(n.Args))
	for i, a := range n.Args {
		var v *xbson.Value
		var err error
		if d.info.Params[i] == ParamSeq {
			v, err = e.seqArg(a)
		} else {
			v, err = e.scalarArg(a)
		}
		if err != nil {
			return nil, nil, err
		}
		args[i] = v
	}
	return args, d, nil
}

// callScalar 调一个方法并取一个值。IIF 走短路分支，不在这里求两边。
func (n *CallNode) callScalar(e env) (*xbson.Value, error) {
	if n == nil {
		return xbson.Null, nil
	}
	if v, handled, err := n.condition(e); handled {
		return v, err
	}
	args, d, err := n.methodArgs(e)
	if err != nil {
		return nil, err
	}
	return d.call(n.Name, e.ctx(), args)
}

// callSeq 调一个方法并把结果摊成一串值产出。
func (n *CallNode) callSeq(e env, yield func(*xbson.Value, error) bool) {
	if n == nil {
		return
	}
	args, d, err := n.methodArgs(e)
	if err != nil {
		yield(nil, err)
		return
	}
	out, err := d.call(n.Name, e.ctx(), args)
	if err != nil {
		yield(nil, err)
		return
	}
	for _, v := range items(out) {
		if !e.emit(yield, v) {
			return
		}
	}
}

// condition 处理 IIF：只求条件命中的那一支。
//
// 第二个返回值说明这次调用是不是 IIF；不是就交回给常规路径。
// 放在这里而不是当成普通方法，正是为了不去求另一支——那一支可能会报错。
func (n *CallNode) condition(e env) (*xbson.Value, bool, error) {
	if !foldEqual(n.Name, "IIF") || len(n.Args) != 3 {
		return nil, false, nil
	}
	test, err := e.scalarArg(n.Args[0])
	if err != nil {
		return nil, true, err
	}
	b, ok := test.AsBoolean()
	if !ok {
		return nil, true, errf("IIF: condition must be a boolean, got %s", test.Type())
	}
	branch := n.Args[2]
	if b {
		branch = n.Args[1]
	}
	v, err := e.scalarArg(branch)
	return v, true, err
}

// funcScalar 求带 lambda 的内建函数的标量结果，只有 VECTOR_SIM 走这里。
func (n *FuncNode) funcScalar(e env) (*xbson.Value, error) {
	if n == nil {
		return xbson.Null, nil
	}
	if n.Name != FuncVectorSim {
		return nil, errf("%s returns a sequence", n.Name)
	}
	if len(n.Args) != 1 {
		return nil, errf("VECTOR_SIM takes exactly two arguments")
	}
	left, err := e.scalarArg(n.Input)
	if err != nil {
		return nil, err
	}
	right, err := e.scalarArg(n.Args[0])
	if err != nil {
		return nil, err
	}
	fn := Lookup("VECTOR_SIM", 2)
	if fn == nil {
		return nil, errf("method VECTOR_SIM does not exist or contains invalid parameters")
	}
	return fn(e.ctx(), []*xbson.Value{left, right})
}

// funcSeq 求 MAP、FILTER、SORT 的序列结果。
//
// MAP 对每一项算一遍 lambda 并把结果摊平；FILTER 只留下 lambda 为真的原项——
// 它产出的是原项，不是 lambda 的结果。
func (n *FuncNode) funcSeq(e env, yield func(*xbson.Value, error) bool) {
	if n == nil {
		return
	}
	switch n.Name {
	case FuncMap:
		if n.Lambda == nil {
			yield(nil, errf("MAP requires a `input => expression` body"))
			return
		}
		for item, err := range e.seqValues(n.Input) {
			if err != nil {
				yield(nil, err)
				return
			}
			inner := e.withCurrent(item)
			for v, err := range inner.evalSeq(n.Lambda) {
				if err != nil {
					yield(nil, err)
					return
				}
				if !yield(v, nil) {
					return
				}
			}
		}

	case FuncFilter:
		if n.Lambda == nil {
			yield(nil, errf("FILTER requires a `input => expression` body"))
			return
		}
		for item, err := range e.seqValues(n.Input) {
			if err != nil {
				yield(nil, err)
				return
			}
			keep, err := e.withCurrent(item).evalScalar(n.Lambda)
			if err != nil {
				yield(nil, err)
				return
			}

			if b, ok := keep.AsBoolean(); ok && b {
				if !e.emit(yield, item) {
					return
				}
			}
		}

	case FuncSort:
		n.sortSeq(e, yield)

	default:
		yield(nil, errf("%s does not produce a sequence", n.Name))
	}
}

// sortSeq 求 SORT 的结果。
//
// 排序要先看到全部输入，所以这里必须整串收进内存，无法流式产出。
func (n *FuncNode) sortSeq(e env, yield func(*xbson.Value, error) bool) {
	if n.Lambda == nil {
		yield(nil, errf("SORT requires a `input => key` body"))
		return
	}
	input, err := collect2(e.seqValues(n.Input))
	if err != nil {
		yield(nil, err)
		return
	}
	extra := make([]*xbson.Value, len(n.Args))
	for i, a := range n.Args {
		v, err := e.scalarArg(a)
		if err != nil {
			yield(nil, err)
			return
		}
		extra[i] = v
	}
	fn := LookupLambda("SORT", 2+len(extra))
	if fn == nil {
		yield(nil, errf("SORT takes at most one sort-order argument"))
		return
	}
	out, err := fn(e.ctx(), input, lambdaBody{body: n.Lambda, e: e}, extra)
	if err != nil {
		yield(nil, err)
		return
	}
	for _, v := range items(out) {
		if !e.emit(yield, v) {
			return
		}
	}
}

// collect2 把一个值加错误的序列收进切片，遇错即停。
func collect2(seq iter.Seq2[*xbson.Value, error]) ([]*xbson.Value, error) {
	var out []*xbson.Value
	for v, err := range seq {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// lambdaBody 把一段 lambda 和它的外层环境绑在一起，交给方法实现按项调用。
type lambdaBody struct {
	body Node
	e    env
}

// bind 用调用方给的上下文改写环境，再把当前项换成 current。
//
// ctx 里的根为空时保留原来的根——方法不一定关心根是什么。
func (l lambdaBody) bind(ctx *Ctx, current *xbson.Value) env {
	e := l.e
	if ctx != nil {
		if ctx.Root != nil {
			e.root = ctx.Root
		}
		e.params = ctx.Params
		e.coll = ctx.Collation
	}
	return e.withCurrent(current)
}

// Eval 对某一项算一遍 lambda，返回它产出的所有值。
func (l lambdaBody) Eval(ctx *Ctx, current *xbson.Value) ([]*xbson.Value, error) {
	return l.bind(ctx, current).collect(l.body)
}

// EvalScalar 对某一项算一遍 lambda，只取一个值。
func (l lambdaBody) EvalScalar(ctx *Ctx, current *xbson.Value) (*xbson.Value, error) {
	return l.bind(ctx, current).evalScalar(l.body)
}

// documentLiteral 求一个文档字面量。
//
// 只有一个字段且键以 $ 开头时，先试着按扩展写法解成对应的类型值，
// 比如 {$date: "..."} 解成时间、{$oid: "..."} 解成 ObjectId。
// 解不出来就老老实实当成一个普通字段。
func (n *DocumentNode) documentLiteral(e env) (*xbson.Value, error) {
	if n == nil {
		return xbson.NewDocument().Value(), nil
	}
	if len(n.Fields) == 1 && len(n.Fields[0].Key) > 0 && n.Fields[0].Key[0] == '$' {
		v, err := e.scalarArg(n.Fields[0].Value)
		if err != nil {
			return nil, err
		}
		if isString(v) {
			if out, ok, err := e.typedLiteral(n.Fields[0].Key, v); ok {
				return out, err
			}
		}

		d := xbson.NewDocument()
		d.Set(n.Fields[0].Key, v)
		return d.Value(), nil
	}

	d := xbson.NewDocument()
	for _, f := range n.Fields {
		v, err := e.scalarArg(f.Value)
		if err != nil {
			return nil, err
		}
		d.Set(f.Key, v)
	}
	return d.Value(), nil
}

// typedLiteral 按 $ 开头的键把字符串解成对应类型，第二个返回值说明这个键认不认得。
//
// 时间和 Decimal 走二进制排序规则解析：它们的文本格式与语言环境无关，
// 不该受排序规则影响。
func (e env) typedLiteral(key string, v *xbson.Value) (*xbson.Value, bool, error) {
	binCtx := &Ctx{Root: e.root, Collation: xcoll.Binary, Params: e.params}
	call := func(name string, ctx *Ctx) (*xbson.Value, bool, error) {
		fn := Lookup(name, 1)
		if fn == nil {
			return nil, false, nil
		}
		out, err := fn(ctx, []*xbson.Value{v})
		return out, true, err
	}
	switch key {
	case "$binary":
		return call("BINARY", e.ctx())
	case "$oid":
		return call("OBJECTID", e.ctx())
	case "$guid":
		return call("GUID", e.ctx())
	case "$date":
		return call("DATETIME", binCtx)
	case "$numberLong":
		return call("INT64", e.ctx())
	case "$numberDecimal":
		return call("DECIMAL", binCtx)
	case "$minValue":
		return xbson.MinValue, true, nil
	case "$maxValue":
		return xbson.MaxValue, true, nil
	}
	return nil, false, nil
}

// arrayLiteral 求一个数组字面量，各项产出一串时会被收成嵌套数组。
func (n *ArrayNode) arrayLiteral(e env) (*xbson.Value, error) {
	a := xbson.NewArray()
	if n == nil {
		return a.Value(), nil
	}
	for _, it := range n.Items {
		v, err := e.scalarArg(it)
		if err != nil {
			return nil, err
		}
		a.Append(v)
	}
	return a.Value(), nil
}
