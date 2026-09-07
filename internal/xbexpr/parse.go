package xbexpr

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbson"
)

// MaxDepth 是表达式的嵌套深度上限，解析和求值各自都按它设限。
const MaxDepth = 128

var (
	// ErrSyntax 表示表达式写法不对。
	ErrSyntax = fmt.Errorf("%w: syntax error", ErrExpr)

	// ErrTooDeep 表示嵌套超过了 [MaxDepth]。
	ErrTooDeep = fmt.Errorf("%w: expression nesting too deep", ErrExpr)

	// ErrNumberLiteral 表示数字字面量不合法或超出范围。
	ErrNumberLiteral = fmt.Errorf("%w: invalid number literal", ErrExpr)

	// ErrCardinality 表示该给一个值的地方给了一串值。
	ErrCardinality = fmt.Errorf("%w: scalar/sequence mismatch", ErrExpr)
)

// ParseError 带上出错的位置和附近的记号，好让人对着源文本找。
type ParseError struct {
	// Pos 是出错处在源文本里的字节下标。
	Pos int

	// Token 是出错处附近那个记号的文本，可能为空。
	Token string

	// Msg 是这次错误的具体说明。
	Msg string

	// err 是被包住的那个哨兵错误，供 [errors.Is] 判别。
	err error
}

// Error 把哨兵错误、说明、位置拼成一句话。
func (e *ParseError) Error() string {
	if e.Token == "" {
		return fmt.Sprintf("%s: %s at offset %d", e.err, e.Msg, e.Pos)
	}
	return fmt.Sprintf("%s: %s at offset %d, near %q", e.err, e.Msg, e.Pos, e.Token)
}

// Unwrap 返回底下的哨兵错误。
func (e *ParseError) Unwrap() error { return e.err }

// Parse 把整个串解析成一棵语法树，末尾有剩余就报错。
func Parse(src string) (Node, error) {
	p := &parser{lx: newLexer(src)}
	n, err := p.parseFull(ScopeRoot)
	if err != nil {
		return nil, err
	}
	if t := p.lx.lookAhead(true); t.Type != TokEOF {
		return nil, p.unexpected(t)
	}
	return n, nil
}

// ParsePrefix 只解析开头能构成表达式的那一段，返回树和停下来的位置。
//
// SQL 解析器靠它切出一个表达式，再接着读后面的子句。
func ParsePrefix(src string) (Node, int, error) {
	p := &parser{lx: newLexer(src)}
	n, err := p.parseFull(ScopeRoot)
	if err != nil {
		return nil, 0, err
	}
	t := p.lx.lookAhead(true)
	if t.Type == TokEOF {
		return n, len(src), nil
	}
	return n, t.Pos, nil
}

// parser 是一个递归下降的解析器，depth 用来卡住嵌套深度。
type parser struct {
	lx    *lexer
	depth int
}

// errAt 造一个带位置的解析错误。
func (p *parser) errAt(t Token, err error, format string, args ...any) error {
	return &ParseError{Pos: t.Pos, Token: t.Value, Msg: fmt.Sprintf(format, args...), err: err}
}

// unexpected 报「这里不该出现这个记号」。
func (p *parser) unexpected(t Token) error {
	return p.errAt(t, ErrSyntax, "unexpected %s", t.Type)
}

// expect 断言记号类别，不符就报错。
func (p *parser) expect(t Token, want TokenType) error {
	if t.Type == want {
		return nil
	}
	return p.errAt(t, ErrSyntax, "expected %s, got %s", want, t.Type)
}

// parseFull 解析一个完整的表达式，含各级二元运算。
//
// 先把操作数和运算符平铺成两串，最后交给 [parser.fold] 按优先级合并——
// 这样不必为每一级优先级各写一个递归函数。
//
// BETWEEN 在这里特殊处理：它后面必须跟 AND 和第二个界，两个界打包成一个
// 数组节点当右操作数。
func (p *parser) parseFull(scope Scope) (Node, error) {
	first, err := p.parseSingle(scope)
	if err != nil {
		return nil, err
	}
	values := []Node{first}
	var ops []Operator

	for {
		op, ok, err := p.readOperator()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}

		right, err := p.parseSingle(scope)
		if err != nil {
			return nil, err
		}

		if op.Base() == OpBetween {
			t := p.lx.readToken(true)
			if !t.is("AND") {
				return nil, p.errAt(t, ErrSyntax, "expected AND after BETWEEN")
			}
			hi, err := p.parseSingle(scope)
			if err != nil {
				return nil, err
			}
			if right.Cardinality() != Scalar {
				return nil, p.cardErr("lower bound of BETWEEN", right)
			}
			if hi.Cardinality() != Scalar {
				return nil, p.cardErr("upper bound of BETWEEN", hi)
			}
			right = &ArrayNode{Items: []Node{right, hi}}
		}

		values = append(values, right)
		ops = append(ops, op)
	}

	return p.fold(values, ops)
}

// cardErr 报「这里要一个值，给的却是一串」，并提示可以加 ANY/ALL 或者用 ARRAY() 收起来。
func (p *parser) cardErr(where string, n Node) error {
	return fmt.Errorf("%w: %s `%s` returns a sequence, use ANY or ALL, or wrap it with ARRAY()",
		ErrCardinality, where, Print(n))
}

// readOperator 试着读一个二元运算符，第二个返回值说明读到没有。
//
// ANY 和 ALL 要连着后面的比较符一起认，两个词拼成一个键去查运算符表。
func (p *parser) readOperator() (Operator, bool, error) {
	t := p.lx.lookAhead(true)

	if t.isOperand() {
		p.lx.readToken(true)
		op, ok := operatorByKey(upperASCII(t.Value))
		if !ok {
			return 0, false, p.errAt(t, ErrSyntax, "unknown operator")
		}
		return op, true, nil
	}

	if t.is("ANY") || t.is("ALL") {
		quant := upperASCII(t.Value)
		p.lx.readToken(true)
		t2 := p.lx.readToken(true)
		if !t2.isOperand() {
			return 0, false, p.errAt(t2, ErrSyntax, "expected a comparison operator after %s", quant)
		}
		key := quant + " " + upperASCII(t2.Value)
		op, ok := operatorByKey(key)
		if !ok {
			return 0, false, p.errAt(t2, ErrSyntax, "operator `%s` does not exist", key)
		}
		return op, true, nil
	}

	return 0, false, nil
}

// fold 按优先级把平铺的操作数与运算符合并成一棵树。
//
// 从 0 号运算符开始，每一级把所有出现的位置从左往右合并掉，再看下一级——
// [Operator] 的取值次序就是优先级次序。
//
// 合并时顺带查基数：带量词的运算允许左边是一串值，标量的左边会自动套上
// ITEMS() 变成序列；不带量词的两边都必须是一个值。
func (p *parser) fold(values []Node, ops []Operator) (Node, error) {
	order := 0
	for len(values) >= 2 {
		if order >= int(numOperators) {
			return nil, fmt.Errorf("%w: unresolved operator sequence", ErrSyntax)
		}
		op := Operator(order)
		i := slices.Index(ops, op)
		if i < 0 {
			order++
			continue
		}

		left, right := values[i], values[i+1]

		if op.Quantifier() != QuantNone {
			if left.Cardinality() == Scalar {
				left = wrapItems(left)
			}
		} else if left.Cardinality() != Scalar {
			return nil, p.cardErr("left operand of `"+op.String()+"`", left)
		}
		if right.Cardinality() != Scalar {
			return nil, p.cardErr("right operand of `"+op.String()+"`", right)
		}

		values[i] = &BinaryNode{Op: op, Left: left, Right: right}
		values = slices.Delete(values, i+1, i+2)
		ops = slices.Delete(ops, i, i+1)
	}
	return values[0], nil
}

// wrapItems 给一个标量套上 ITEMS()，把它当成序列看。
func wrapItems(n Node) Node {
	return &CallNode{Name: "ITEMS", Args: []Node{n}, PathItems: printsAsPath(n)}
}

// printsAsPath 判断一个节点还原成文本时是不是一条路径，决定套上的调用要不要写成点号形式。
func printsAsPath(n Node) bool {
	switch t := n.(type) {
	case *PathNode:
		return true
	case *ParenNode:
		return t != nil && printsAsPath(t.Inner)
	}
	return false
}

// wrapArray 给一串值套上 ARRAY()，收成一个数组值。
func wrapArray(n Node) Node { return &CallNode{Name: "ARRAY", Args: []Node{n}} }

// parseSingle 解析一个不含二元运算的操作数。
//
// 按记号类别分派：数、关键字常量、字符串、*、{}、[]、@参数、括号、
// 函数调用、方法调用，最后才是路径。负号只有紧跟着数字时才并进字面量，
// 否则它是减法运算符。
func (p *parser) parseSingle(scope Scope) (Node, error) {
	if p.depth >= MaxDepth {
		return nil, fmt.Errorf("%w: limit is %d", ErrTooDeep, MaxDepth)
	}
	p.depth++
	defer func() { p.depth-- }()

	t := p.lx.readToken(true)

	if t.Type == TokDouble {
		return p.numberLiteral(t, t.Value, true)
	}
	if t.Type == TokInt {
		return p.numberLiteral(t, t.Value, false)
	}
	if t.Type == TokMinus {
		if ahead := p.lx.lookAhead(false); ahead.Type == TokDouble || ahead.Type == TokInt {
			num := p.lx.readToken(false)
			return p.numberLiteral(t, "-"+num.Value, ahead.Type == TokDouble)
		}
	}

	if t.Type == TokWord {
		switch {
		case t.is("true"):
			return &ConstNode{Value: xbson.True}, nil
		case t.is("false"):
			return &ConstNode{Value: xbson.False}, nil
		case t.is("null"):
			return &ConstNode{Value: xbson.Null}, nil
		}
	}

	if t.Type == TokString {
		return &ConstNode{Value: xbson.String(t.Value)}, nil
	}

	if t.Type == TokAsterisk {
		return p.parseSource()
	}
	if t.Type == TokOpenBrace {
		return p.parseDocument(scope)
	}
	if t.Type == TokOpenBracket {
		return p.parseArray(scope)
	}
	if t.Type == TokAt {
		if ahead := p.lx.lookAhead(false); ahead.Type == TokWord || ahead.Type == TokInt {
			name := p.lx.readToken(false)
			return &ParameterNode{Name: name.Value}, nil
		}
	}
	if t.Type == TokOpenParen {
		inner, err := p.parseFull(scope)
		if err != nil {
			return nil, err
		}
		if err := p.expect(p.lx.readToken(true), TokCloseParen); err != nil {
			return nil, err
		}
		return &ParenNode{Inner: inner}, nil
	}

	if t.Type == TokWord && p.lx.lookAhead(true).Type == TokOpenParen {
		switch {
		case foldEqual(t.Value, "MAP"):
			return p.parseFunc(FuncMap, scope)
		case foldEqual(t.Value, "FILTER"):
			return p.parseFunc(FuncFilter, scope)
		case foldEqual(t.Value, "SORT"):
			return p.parseFunc(FuncSort, scope)
		case foldEqual(t.Value, "VECTOR_SIM"):
			return p.parseFunc(FuncVectorSim, scope)
		}
		return p.parseCall(t, scope)
	}

	if t.Type == TokAt || t.Type == TokDollar || t.Type == TokWord {
		return p.parsePath(t, scope)
	}

	return nil, p.unexpected(t)
}

// numberLiteral 把数字文本转成常量节点。
//
// 整数先试 32 位，装不下再用 64 位——小整数在文档里更省地方。
// 浮点数溢出到无穷或者算出 NaN 都报错，但十进制精度不足导致的舍入不算错。
func (p *parser) numberLiteral(t Token, text string, dbl bool) (Node, error) {
	if dbl {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, p.errAt(t, ErrNumberLiteral, "cannot parse %q as a double", text)
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, p.errAt(t, ErrNumberLiteral, "%q is out of range for a double", text)
		}
		return &ConstNode{Value: xbson.Double(f)}, nil
	}
	if i32, err := strconv.ParseInt(text, 10, 32); err == nil {
		return &ConstNode{Value: xbson.Int32(int32(i32))}, nil
	}
	i64, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, p.errAt(t, ErrNumberLiteral, "%q is out of range for a 64-bit integer", text)
	}
	return &ConstNode{Value: xbson.Int64(i64)}, nil
}

// parseSource 解析 *，也就是数据源本身。
//
// 后面跟着点号时，把点号后面的表达式当成对每一项要算的东西，
// 整体变成一次 MAP。
func (p *parser) parseSource() (Node, error) {
	src := &SourceNode{}

	if p.lx.lookAhead(false).Type != TokPeriod {
		return src, nil
	}
	p.lx.readToken(false)
	rest, err := p.parseSingle(ScopeSource)
	if err != nil {
		return nil, err
	}
	return &FuncNode{Name: FuncMap, Input: src, Lambda: rest}, nil
}

// parseDocument 解析文档字面量。
//
// 字段可以只写键名不写值，那等价于从文档里取同名字段。
// 值算出来是一串时自动收成数组——文档的字段只能放一个值。
func (p *parser) parseDocument(scope Scope) (Node, error) {
	doc := &DocumentNode{}
	if p.lx.lookAhead(true).Type == TokCloseBrace {
		p.lx.readToken(true)
		return doc, nil
	}
	for {
		key, err := p.readKey()
		if err != nil {
			return nil, err
		}

		next := p.lx.readToken(true)
		var value Node
		if next.Type == TokColon {
			value, err = p.parseFull(scope)
			if err != nil {
				return nil, err
			}
			next = p.lx.readToken(true)
		} else {
			value = &PathNode{
				Root:  RootDocument,
				Steps: []PathStep{{Kind: StepField, Name: key}},
				Scope: ScopeRoot,
			}
		}

		if value.Cardinality() != Scalar {
			value = wrapArray(value)
		}
		doc.Fields = append(doc.Fields, DocField{Key: key, Value: value})

		if next.Type == TokComma {
			continue
		}
		if next.Type == TokCloseBrace {
			return doc, nil
		}
		return nil, p.errAt(next, ErrSyntax, "expected , or } in document literal")
	}
}

// readKey 读一个文档键：字符串、标识符或整数都行。
func (p *parser) readKey() (string, error) {
	t := p.lx.readToken(true)
	switch t.Type {
	case TokString, TokWord, TokInt:
		return t.Value, nil
	default:
	}
	return "", p.errAt(t, ErrSyntax, "expected a document key")
}

// parseArray 解析数组字面量。项算出来是一串时自动收成数组，成为嵌套的一项。
func (p *parser) parseArray(scope Scope) (Node, error) {
	arr := &ArrayNode{}
	if p.lx.lookAhead(true).Type == TokCloseBracket {
		p.lx.readToken(true)
		return arr, nil
	}
	for {
		item, err := p.parseFull(scope)
		if err != nil {
			return nil, err
		}
		if item.Cardinality() != Scalar {
			item = wrapArray(item)
		}
		arr.Items = append(arr.Items, item)

		next := p.lx.readToken(true)
		if next.Type == TokComma {
			continue
		}
		if next.Type == TokCloseBracket {
			return arr, nil
		}
		return nil, p.errAt(next, ErrSyntax, "expected , or ] in array literal")
	}
}

// parseCall 解析一次方法调用，读完实参后核对方法确实存在。
func (p *parser) parseCall(name Token, scope Scope) (Node, error) {
	p.lx.readToken(true)
	call := &CallNode{Name: upperASCII(name.Value)}

	if p.lx.lookAhead(true).Type == TokCloseParen {
		p.lx.readToken(true)
		return call, p.checkCall(name, call)
	}
	for {
		arg, err := p.parseFull(scope)
		if err != nil {
			return nil, err
		}

		call.Args = append(call.Args, arg)

		next := p.lx.readToken(true)
		if next.Type == TokComma {
			continue
		}
		if next.Type == TokCloseParen {
			return call, p.checkCall(name, call)
		}
		return nil, p.errAt(next, ErrSyntax, "expected , or ) in call to %s", call.Name)
	}
}

// checkCall 确认方法名与实参个数都对得上。
//
// 名字对但个数不对，与名字根本不存在，报的是同一句——方法表按「名字加个数」
// 索引，这两种情况在表里没有区别。
func (p *parser) checkCall(name Token, call *CallNode) error {
	if _, ok := call.MethodInfo(); ok {
		return nil
	}
	return p.errAt(name, ErrSyntax,
		"method %s does not exist or takes a different number of arguments", call.Name)
}

// parseFunc 解析一次带 lambda 的内建函数调用。
//
// 输入是标量时自动套上 ITEMS()（VECTOR_SIM 除外，它本来就收两个值）。
// lambda 写作 `=>`，它里面的作用域取决于输入：输入是数据源就还在数据源那一层，
// 否则是当前项。
func (p *parser) parseFunc(name FuncName, scope Scope) (Node, error) {
	open := p.lx.readToken(true)

	input, err := p.parseSingle(scope)
	if err != nil {
		return nil, err
	}

	if name != FuncVectorSim && input.Cardinality() == Scalar {
		input = wrapItems(input)
	}

	fn := &FuncNode{Name: name, Input: input}

	if p.lx.lookAhead(true).Type == TokEquals {
		p.lx.readToken(true)
		if err = p.expect(p.lx.readToken(true), TokGreater); err != nil {
			return nil, err
		}

		inner := ScopeCurrent
		if isSource(input) {
			inner = ScopeSource
		}
		fn.Lambda, err = p.parseFull(inner)
		if err != nil {
			return nil, err
		}
	}

	if p.lx.lookAhead(true).Type != TokCloseParen {
		if err := p.expect(p.lx.readToken(true), TokComma); err != nil {
			return nil, err
		}
		for {
			arg, err := p.parseFull(scope)
			if err != nil {
				return nil, err
			}
			fn.Args = append(fn.Args, arg)
			if p.lx.lookAhead(true).Type == TokComma {
				p.lx.readToken(true)
				continue
			}
			break
		}
	}
	if err := p.expect(p.lx.readToken(true), TokCloseParen); err != nil {
		return nil, err
	}

	if err := p.checkFuncShape(open, fn); err != nil {
		return nil, err
	}
	return fn, nil
}

// checkFuncShape 按函数各自的要求核对形状。
//
// MAP 和 FILTER 必须有 lambda 且没有额外实参；SORT 必须有 lambda，最多再带一个
// 排序方向；VECTOR_SIM 反过来，不收 lambda，正好两个值。
func (p *parser) checkFuncShape(at Token, fn *FuncNode) error {
	switch fn.Name {
	case FuncMap, FuncFilter:
		if fn.Lambda == nil {
			return p.errAt(at, ErrSyntax, "%s requires a `input => expression` lambda", fn.Name)
		}
		if len(fn.Args) != 0 {
			return p.errAt(at, ErrSyntax, "%s takes no extra arguments", fn.Name)
		}
	case FuncSort:
		if fn.Lambda == nil {
			return p.errAt(at, ErrSyntax, "SORT requires a `input => key` lambda")
		}
		if len(fn.Args) > 1 {
			return p.errAt(at, ErrSyntax, "SORT takes at most one sort-order argument")
		}
	case FuncVectorSim:
		if fn.Lambda != nil {
			return p.errAt(at, ErrSyntax, "VECTOR_SIM takes two values, not a lambda")
		}
		if len(fn.Args) != 1 {
			return p.errAt(at, ErrSyntax, "VECTOR_SIM takes exactly two arguments")
		}
	}
	return nil
}

// isSource 判断一个节点剥掉括号之后是不是数据源。
func isSource(n Node) bool {
	for {
		pr, ok := n.(*ParenNode)
		if !ok || pr == nil || pr.Inner == nil {
			break
		}
		n = pr.Inner
	}
	_, ok := n.(*SourceNode)
	return ok
}

// isParameter 判断一个节点剥掉括号之后是不是具名参数。
func isParameter(n Node) bool {
	for {
		pr, ok := n.(*ParenNode)
		if !ok || pr == nil || pr.Inner == nil {
			break
		}
		n = pr.Inner
	}
	_, ok := n.(*ParameterNode)
	return ok
}

// parsePath 解析一条取值路径。
//
// 起点由记号定：$ 是整篇文档，@ 是当前项；都不写时看外层作用域——
// 在 lambda 里裸写字段名指的是当前项。
//
// 一路读字段与下标，直到 [StepAll] 或 [StepFilter]——那两个只能收尾。
// 路径因此产出一串值，后面若还跟着点号，余下的部分变成对每一项的 MAP。
func (p *parser) parsePath(t Token, scope Scope) (Node, error) {
	root := RootDocument
	if scope != ScopeRoot {
		root = RootCurrent
	}
	switch t.Type {
	case TokAt:
		root = RootCurrent
	case TokDollar:
		root = RootDocument
	default:
	}

	if t.Type == TokAt || t.Type == TokDollar {
		if p.lx.lookAhead(false).Type == TokPeriod {
			p.lx.readToken(true)
			p.lx.readToken(true)
		}
	}

	var steps []PathStep
	field, err := p.readField()
	if err != nil {
		return nil, err
	}
	if field != "" {
		steps = append(steps, PathStep{Kind: StepField, Name: field})
	}

	for {
		ahead := p.lx.lookAhead(false)

		if ahead.Type == TokPeriod {
			p.lx.readToken(true)

			p.lx.readToken(false)
			f, err := p.readField()
			if err != nil {
				return nil, err
			}
			if f != "" {
				steps = append(steps, PathStep{Kind: StepField, Name: f})
			}
			continue
		}

		if ahead.Type == TokOpenBracket {
			step, err := p.parseSubscript()
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)

			if step.Kind == StepAll || step.Kind == StepFilter {
				break
			}
			continue
		}

		break
	}

	path := &PathNode{Root: root, Steps: steps, Scope: scope}

	if path.Cardinality() == Sequence && p.lx.lookAhead(false).Type == TokPeriod {
		p.lx.readToken(false)
		rest, err := p.parseSingle(ScopeCurrent)
		if err != nil {
			return nil, err
		}
		return &FuncNode{Name: FuncMap, Input: path, Lambda: rest}, nil
	}
	return path, nil
}

// readField 读一步字段名：标识符，或者方括号里的字符串。
//
// 方括号里的字符串写法用来写那些不是合法标识符的字段名。
// 读不到字段名时返回空串而不报错——那说明这一步不是字段。
func (p *parser) readField() (string, error) {
	cur := p.lx.cur
	switch cur.Type {
	case TokOpenBracket:
		t := p.lx.readToken(true)
		if err := p.expect(t, TokString); err != nil {
			return "", err
		}
		if err := p.expect(p.lx.readToken(true), TokCloseBracket); err != nil {
			return "", err
		}
		return t.Value, nil
	case TokWord:
		return cur.Value, nil
	default:
	}
	return "", nil
}

// parseSubscript 解析方括号里的一步。
//
// 整数是定位下标，前面带减号就从末尾数起；星号是全展开；其余按表达式解析，
// 是具名参数就当动态下标，否则当过滤条件。
func (p *parser) parseSubscript() (PathStep, error) {
	p.lx.readToken(true)

	var step PathStep
	ahead := p.lx.lookAhead(true)
	switch ahead.Type {
	case TokInt:
		t := p.lx.readToken(true)
		idx, err := p.arrayIndex(t)
		if err != nil {
			return step, err
		}
		step = PathStep{Kind: StepIndex, Index: idx}

	case TokMinus:
		p.lx.readToken(true)
		t := p.lx.readToken(true)
		if err := p.expect(t, TokInt); err != nil {
			return step, err
		}
		idx, err := p.arrayIndex(t)
		if err != nil {
			return step, err
		}
		step = PathStep{Kind: StepIndex, Index: -idx}

	case TokAsterisk:
		p.lx.readToken(true)
		step = PathStep{Kind: StepAll}

	default:
		inner, err := p.parseFull(ScopeCurrent)
		if err != nil {
			return step, err
		}
		if isParameter(inner) {
			step = PathStep{Kind: StepParamIndex, Expr: inner}
		} else {
			step = PathStep{Kind: StepFilter, Expr: inner}
		}
	}

	if err := p.expect(p.lx.readToken(true), TokCloseBracket); err != nil {
		return step, err
	}
	return step, nil
}

// arrayIndex 把下标文本转成整数，超出 32 位就报错。
func (p *parser) arrayIndex(t Token) (int, error) {
	v, err := strconv.ParseInt(t.Value, 10, 32)
	if err != nil {
		return 0, p.errAt(t, ErrNumberLiteral, "%q is out of range for an array index", t.Value)
	}
	return int(v), nil
}
