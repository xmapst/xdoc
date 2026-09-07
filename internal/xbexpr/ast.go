// Package xbexpr 是文档表达式：解析成语法树，再对着一篇文档求值。
//
// 一个表达式算出来可能是一个值，也可能是一串值——路径里的 [*]、过滤器、
// MAP 之流都会展开成多项，这一点由 [Cardinality] 标记，[Eval] 与 [EvalSeq]
// 分别对应两种取法。取不到东西通常给 Null 而不是报错：文档里字段缺失、
// 类型不齐是常态。
//
// 内建方法在各 method_*.go 的 init 里登记进一张按「名字加实参个数」索引的表，
// 所以同名不同实参个数算两个方法。标成易变的方法（取当前时间、随机数）
// 不能用来建索引，见 [IsIndexable]。
//
// [Print] 把语法树还原成文本，且能再解析回同一棵树——索引表达式就是按文本
// 存在集合页上的。
package xbexpr

import (
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
)

// Kind 区分语法树节点的种类，省得到处做类型断言。
type Kind uint8

const (
	KindInvalid Kind = iota
	KindConst
	KindPath
	KindDocument
	KindArray
	KindParameter
	KindParen
	KindCall
	KindFunc
	KindBinary
	KindSource
)

// Cardinality 说明一个节点算出来是一个值还是一串值。
//
// 同一棵树两种都能求：[Eval] 取标量，[EvalSeq] 取序列，标量会被当成只有一项的序列。
type Cardinality uint8

const (
	// Scalar 表示算出来正好一个值。
	Scalar Cardinality = iota

	// Sequence 表示算出来零到多个值。
	Sequence
)

// String 返回 scalar 或 sequence。
func (c Cardinality) String() string {
	if c == Sequence {
		return "sequence"
	}
	return "scalar"
}

// Node 是语法树的节点。
//
// 实现都在本包内：未导出的 node 方法把这个接口封死，外部无法再加新节点类型。
type Node interface {
	// Kind 返回节点种类。
	Kind() Kind

	// Cardinality 返回这个节点算出一个值还是一串值。
	Cardinality() Cardinality

	// String 把节点还原成表达式文本。
	String() string

	// Children 遍历直接子节点，次序与求值次序一致。
	Children() iter.Seq[Node]

	appendTo(b []byte) []byte

	node()
}

// ConstNode 是一个字面量。
type ConstNode struct {
	Value *xbson.Value
}

// Kind 返回 [KindConst]。
func (n *ConstNode) Kind() Kind { return KindConst }

// Cardinality 恒为 [Scalar]。
func (n *ConstNode) Cardinality() Cardinality { return Scalar }
func (n *ConstNode) node()                    {}

// value 取字面量的值，节点或值为空时当 Null。
func (n *ConstNode) value() *xbson.Value {
	if n == nil || n.Value == nil {
		return xbson.Null
	}
	return n.Value
}

// RootKind 说明一条路径从哪里起步。
type RootKind uint8

const (
	// RootDocument 从整篇文档起步，写作 $。
	RootDocument RootKind = iota

	// RootCurrent 从当前项起步，写作 @；在过滤器和 lambda 里指被遍历的那一项。
	RootCurrent
)

// String 返回 $ 或 @。
func (r RootKind) String() string {
	if r == RootCurrent {
		return "@"
	}
	return "$"
}

// Scope 说明这条路径是在哪一层里解析的。
//
// 它与 [RootKind] 不同：RootKind 是写法，Scope 是查询规划时定下的归属。
type Scope uint8

const (
	// ScopeRoot 指向整篇文档。
	ScopeRoot Scope = iota

	// ScopeCurrent 指向当前项。
	ScopeCurrent

	// ScopeSource 指向数据源本身，也就是 SQL 里的 * 那一层。
	ScopeSource
)

// StepKind 区分路径上的一步是怎么走的。
type StepKind uint8

const (
	// StepField 取文档的一个字段。
	StepField StepKind = iota

	// StepIndex 取数组的第几项，下标是写死的常量，负数从末尾数起。
	StepIndex

	// StepParamIndex 取数组的第几项，下标由一个表达式算出来。
	StepParamIndex

	// StepAll 展开整个数组，写作 [*]。它只能是最后一步，结果是一串值。
	StepAll

	// StepFilter 展开数组并按条件过滤，写作 [表达式]。只能是最后一步，结果是一串值。
	StepFilter
)

// PathStep 是路径上的一步。哪些字段有意义取决于 Kind。
type PathStep struct {
	// Kind 决定这一步怎么走，也决定下面哪个字段有效。
	Kind StepKind

	// Name 是字段名，[StepField] 用。
	Name string

	// Index 是数组下标，[StepIndex] 用；负数从末尾数起。
	Index int

	// Expr 是下标表达式或过滤条件，[StepParamIndex] 与 [StepFilter] 用。
	Expr Node
}

// PathNode 是一条取值路径，比如 $.a.b[0] 或 @.tags[*]。
type PathNode struct {
	Steps []PathStep
	Root  RootKind

	Scope Scope
}

// Kind 返回 [KindPath]。
func (n *PathNode) Kind() Kind { return KindPath }

// Cardinality 只看最后一步：[StepAll] 与 [StepFilter] 产出一串值，其余都是一个值。
func (n *PathNode) Cardinality() Cardinality {
	if n == nil || len(n.Steps) == 0 {
		return Scalar
	}
	if k := n.Steps[len(n.Steps)-1].Kind; k == StepAll || k == StepFilter {
		return Sequence
	}
	return Scalar
}

func (n *PathNode) node() {}

// DocField 是文档字面量里的一个字段。
type DocField struct {
	Key   string
	Value Node
}

// DocumentNode 是一个文档字面量，比如 {a: 1, b: $.x}。
type DocumentNode struct {
	Fields []DocField
}

// Kind 返回 [KindDocument]。
func (n *DocumentNode) Kind() Kind { return KindDocument }

// Cardinality 恒为 [Scalar]。
func (n *DocumentNode) Cardinality() Cardinality { return Scalar }
func (n *DocumentNode) node()                    {}

// ArrayNode 是一个数组字面量。
type ArrayNode struct {
	Items []Node
}

// Kind 返回 [KindArray]。
func (n *ArrayNode) Kind() Kind { return KindArray }

// Cardinality 恒为 [Scalar]。
func (n *ArrayNode) Cardinality() Cardinality { return Scalar }
func (n *ArrayNode) node()                    {}

// ParameterNode 是一个具名参数，写作 @name，求值时从 [Ctx] 的 Params 里取。
type ParameterNode struct {
	Name string
}

// Kind 返回 [KindParameter]。
func (n *ParameterNode) Kind() Kind { return KindParameter }

// Cardinality 恒为 [Scalar]。
func (n *ParameterNode) Cardinality() Cardinality { return Scalar }
func (n *ParameterNode) node()                    {}

// ParenNode 是一对括号。留在树里是为了还原文本时保住原样。
type ParenNode struct {
	Inner Node
}

// Kind 返回 [KindParen]。
func (n *ParenNode) Kind() Kind { return KindParen }

// Cardinality 跟里面那个节点走。
func (n *ParenNode) Cardinality() Cardinality {
	if n == nil || n.Inner == nil {
		return Scalar
	}
	return n.Inner.Cardinality()
}

func (n *ParenNode) node() {}

// CallNode 是一次方法调用，比如 UPPER($.name)。
type CallNode struct {
	Name string
	Args []Node

	// PathItems 表示这次调用写在路径后面，第一个实参是被点的那个值。
	//
	// 比如 $.a.UPPER() 会解析成 UPPER($.a) 并把它置真，还原文本时才写得回原样。
	PathItems bool
}

// Kind 返回 [KindCall]。
func (n *CallNode) Kind() Kind { return KindCall }

// Cardinality 由方法名和实参个数决定，见 [CallNode.ReturnsSequence]。
func (n *CallNode) Cardinality() Cardinality {
	if n == nil {
		return Scalar
	}
	if n.ReturnsSequence() {
		return Sequence
	}
	return Scalar
}

func (n *CallNode) node() {}

// FuncName 是几个带 lambda 的内建函数。它们不在方法表里，语法上也自成一格。
type FuncName uint8

const (
	// FuncMap 对每一项算一遍 lambda，产出一串值。
	FuncMap FuncName = iota

	// FuncFilter 留下 lambda 为真的那些项。
	FuncFilter

	// FuncSort 按 lambda 的结果排序。
	FuncSort

	// FuncVectorSim 算两个向量的相似度，产出一个数。
	FuncVectorSim
)

// String 返回函数名；未知取值返回 ?。
func (f FuncName) String() string {
	switch f {
	case FuncMap:
		return "MAP"
	case FuncFilter:
		return "FILTER"
	case FuncSort:
		return "SORT"
	case FuncVectorSim:
		return "VECTOR_SIM"
	}
	return "?"
}

// FuncNode 是一次带 lambda 的内建函数调用。
type FuncNode struct {
	Name FuncName

	// Input 是被处理的那串值。
	Input Node

	// Lambda 是对每一项要算的表达式，里面用 @ 指当前项。
	Lambda Node

	// Args 是余下的实参，比如排序方向。
	Args []Node
}

// Kind 返回 [KindFunc]。
func (n *FuncNode) Kind() Kind { return KindFunc }

// Cardinality 只有 [FuncVectorSim] 是标量，其余都产出一串值。
func (n *FuncNode) Cardinality() Cardinality {
	if n == nil || n.Name == FuncVectorSim {
		return Scalar
	}
	return Sequence
}

func (n *FuncNode) node() {}

// Quantifier 是比较运算前面的 ANY/ALL：左边是一串值时，要求任意一项满足还是全部满足。
type Quantifier uint8

const (
	// QuantNone 没有量词。
	QuantNone Quantifier = iota

	// QuantAny 任意一项满足即为真。
	QuantAny

	// QuantAll 全部满足才为真。
	QuantAll
)

// String 返回 ANY、ALL，或空串。
func (q Quantifier) String() string {
	switch q {
	case QuantAny:
		return "ANY"
	case QuantAll:
		return "ALL"
	default:
		return ""
	}
}

// Operator 是二元运算符。
//
// 取值次序就是优先级次序：越靠前结合得越紧。带 ANY/ALL 的变体各占一个取值，
// 用 [Operator.Base] 取回它们的基础运算，[Operator.Quantifier] 取回量词。
type Operator uint8

const (
	OpMod Operator = iota
	OpDiv
	OpMul
	OpAdd
	OpSub
	OpVectorSim
	OpLike
	OpBetween
	OpIn
	OpGT
	OpGTE
	OpLT
	OpLTE
	OpNE
	OpEQ
	OpAnyLike
	OpAnyBetween
	OpAnyIn
	OpAnyGT
	OpAnyGTE
	OpAnyLT
	OpAnyLTE
	OpAnyNE
	OpAnyEQ
	OpAllLike
	OpAllBetween
	OpAllIn
	OpAllGT
	OpAllGTE
	OpAllLT
	OpAllLTE
	OpAllNE
	OpAllEQ
	OpAnd
	OpOr

	numOperators
)

// opInfo 是一个运算符的属性表项。
type opInfo struct {
	// key 是规范写法，也是 [operatorByKey] 反查用的键。
	key string

	// text 是还原成文本时用的写法，自带该有的空格。
	text string

	// base 是去掉量词之后的基础运算。
	base Operator

	// quant 是这个运算符自带的量词。
	quant Quantifier
}

// opTable 是运算符属性表，按 [Operator] 的取值索引。
var opTable = [numOperators]opInfo{
	OpMod:        {"%", "%", OpMod, QuantNone},
	OpDiv:        {"/", "/", OpDiv, QuantNone},
	OpMul:        {"*", "*", OpMul, QuantNone},
	OpAdd:        {"+", "+", OpAdd, QuantNone},
	OpSub:        {"-", "-", OpSub, QuantNone},
	OpVectorSim:  {"VECTOR_SIM", " VECTOR_SIM ", OpVectorSim, QuantNone},
	OpLike:       {"LIKE", " LIKE ", OpLike, QuantNone},
	OpBetween:    {"BETWEEN", " BETWEEN ", OpBetween, QuantNone},
	OpIn:         {"IN", " IN ", OpIn, QuantNone},
	OpGT:         {">", ">", OpGT, QuantNone},
	OpGTE:        {">=", ">=", OpGTE, QuantNone},
	OpLT:         {"<", "<", OpLT, QuantNone},
	OpLTE:        {"<=", "<=", OpLTE, QuantNone},
	OpNE:         {"!=", "!=", OpNE, QuantNone},
	OpEQ:         {"=", "=", OpEQ, QuantNone},
	OpAnyLike:    {"ANY LIKE", " ANY LIKE ", OpLike, QuantAny},
	OpAnyBetween: {"ANY BETWEEN", " ANY BETWEEN ", OpBetween, QuantAny},
	OpAnyIn:      {"ANY IN", " ANY IN ", OpIn, QuantAny},
	OpAnyGT:      {"ANY >", " ANY>", OpGT, QuantAny},
	OpAnyGTE:     {"ANY >=", " ANY>=", OpGTE, QuantAny},
	OpAnyLT:      {"ANY <", " ANY<", OpLT, QuantAny},
	OpAnyLTE:     {"ANY <=", " ANY<=", OpLTE, QuantAny},
	OpAnyNE:      {"ANY !=", " ANY!=", OpNE, QuantAny},
	OpAnyEQ:      {"ANY =", " ANY=", OpEQ, QuantAny},
	OpAllLike:    {"ALL LIKE", " ALL LIKE ", OpLike, QuantAll},
	OpAllBetween: {"ALL BETWEEN", " ALL BETWEEN ", OpBetween, QuantAll},
	OpAllIn:      {"ALL IN", " ALL IN ", OpIn, QuantAll},
	OpAllGT:      {"ALL >", " ALL>", OpGT, QuantAll},
	OpAllGTE:     {"ALL >=", " ALL>=", OpGTE, QuantAll},
	OpAllLT:      {"ALL <", " ALL<", OpLT, QuantAll},
	OpAllLTE:     {"ALL <=", " ALL<=", OpLTE, QuantAll},
	OpAllNE:      {"ALL !=", " ALL!=", OpNE, QuantAll},
	OpAllEQ:      {"ALL =", " ALL=", OpEQ, QuantAll},
	OpAnd:        {"AND", " AND ", OpAnd, QuantNone},
	OpOr:         {"OR", " OR ", OpOr, QuantNone},
}

// valid 判断取值是否在表内。
func (o Operator) valid() bool { return o < numOperators }

// String 返回规范写法；越界返回 ?。
func (o Operator) String() string {
	if !o.valid() {
		return "?"
	}
	return opTable[o].key
}

// Text 返回还原成文本时该写的样子，含两侧空格；越界返回 ?。
func (o Operator) Text() string {
	if !o.valid() {
		return "?"
	}
	return opTable[o].text
}

// Base 去掉量词，返回基础运算。没有量词的返回自己。
func (o Operator) Base() Operator {
	if !o.valid() {
		return o
	}
	return opTable[o].base
}

// Quantifier 返回该运算符自带的量词。
func (o Operator) Quantifier() Quantifier {
	if !o.valid() {
		return QuantNone
	}
	return opTable[o].quant
}

// IsPredicate 判断这是不是一个比较运算——也就是能拿去查索引的那一类。
func (o Operator) IsPredicate() bool {
	switch o.Base() {
	case OpEQ, OpNE, OpGT, OpGTE, OpLT, OpLTE, OpLike, OpBetween, OpIn:
		return true
	default:
		return false
	}
}

// operatorByKey 按规范写法反查运算符，线性扫过属性表。
func operatorByKey(key string) (Operator, bool) {
	for i := range int(numOperators) {
		if opTable[i].key == key {
			return Operator(i), true
		}
	}
	return 0, false
}

// BinaryNode 是一次二元运算。即便左边是一串值，结果也是一个值。
type BinaryNode struct {
	Op    Operator
	Left  Node
	Right Node
}

// Kind 返回 [KindBinary]。
func (n *BinaryNode) Kind() Kind { return KindBinary }

// Cardinality 恒为 [Scalar]。
func (n *BinaryNode) Cardinality() Cardinality { return Scalar }
func (n *BinaryNode) node()                    {}

// SourceNode 指数据源本身，也就是 SQL 里的 *。它产出一串值。
type SourceNode struct{}

// Kind 返回 [KindSource]。
func (n *SourceNode) Kind() Kind { return KindSource }

// Cardinality 恒为 [Sequence]。
func (n *SourceNode) Cardinality() Cardinality { return Sequence }
func (n *SourceNode) node()                    {}

// seqMethods 列出哪些方法产出一串值，值是允许的实参个数掩码。
//
// 第 n 位为 1 表示 n 个实参的那一版产出序列。比如 SPLIT 是 2|4，
// 即两个或三个实参时产出序列。
var seqMethods = map[string]uint8{
	"ITEMS":    1,
	"KEYS":     1,
	"VALUES":   1,
	"DISTINCT": 1,
	"TOP":      2,
	"SPLIT":    2 | 4,
	"UNION":    2,
	"EXCEPT":   2,
	"CONCAT":   2,
}

// ReturnsSequence 判断这次调用产出的是一串值还是一个值。
func (n *CallNode) ReturnsSequence() bool {
	if n == nil {
		return false
	}
	argc := len(n.Args)
	if argc < 1 || argc > 8 {
		return false
	}
	mask, ok := seqMethods[upperASCII(n.Name)]
	if !ok {
		return false
	}
	return mask&(1<<(argc-1)) != 0
}

// noChildren 是叶子节点共用的空遍历。
func noChildren(func(Node) bool) {}

// Children 没有子节点。
func (n *ConstNode) Children() iter.Seq[Node] { return noChildren }

// Children 没有子节点。
func (n *ParameterNode) Children() iter.Seq[Node] { return noChildren }

// Children 没有子节点。
func (n *SourceNode) Children() iter.Seq[Node] { return noChildren }

// Children 遍历各步里的下标表达式与过滤条件。
func (n *PathNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		for _, s := range n.Steps {
			if s.Expr != nil && !yield(s.Expr) {
				return
			}
		}
	}
}

// Children 按字段次序遍历各字段的值。
func (n *DocumentNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		for _, f := range n.Fields {
			if f.Value != nil && !yield(f.Value) {
				return
			}
		}
	}
}

// Children 按次序遍历各项。
func (n *ArrayNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		for _, it := range n.Items {
			if it != nil && !yield(it) {
				return
			}
		}
	}
}

// Children 只有括号里那一个。
func (n *ParenNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil || n.Inner == nil {
			return
		}
		yield(n.Inner)
	}
}

// Children 按次序遍历各实参。
func (n *CallNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		for _, a := range n.Args {
			if a != nil && !yield(a) {
				return
			}
		}
	}
}

// Children 依次遍历输入、lambda 和其余实参。
func (n *FuncNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		if n.Input != nil && !yield(n.Input) {
			return
		}
		if n.Lambda != nil && !yield(n.Lambda) {
			return
		}
		for _, a := range n.Args {
			if a != nil && !yield(a) {
				return
			}
		}
	}
}

// Children 先左后右。
func (n *BinaryNode) Children() iter.Seq[Node] {
	return func(yield func(Node) bool) {
		if n == nil {
			return
		}
		if n.Left != nil && !yield(n.Left) {
			return
		}
		if n.Right != nil {
			yield(n.Right)
		}
	}
}

// Walk 先序遍历整棵树，先给出节点自己再给出子树。
//
// 提前跳出会立刻停下，不会白走剩下的子树。
func Walk(n Node) iter.Seq[Node] {
	return func(yield func(Node) bool) {
		walk(n, yield)
	}
}

// walk 是 [Walk] 的递归体，返回是否该继续。
func walk(n Node, yield func(Node) bool) bool {
	if n == nil {
		return true
	}
	if !yield(n) {
		return false
	}
	for c := range n.Children() {
		if !walk(c, yield) {
			return false
		}
	}
	return true
}
