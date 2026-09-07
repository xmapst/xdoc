package xbexpr

import (
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbson"
)

// init 登记通用方法。
//
// RANDOM 登记了不带参数和带上下界两版，都标成易变——拿它们建索引没有意义。
func init() {
	one := scalars(1)
	two := scalars(2)
	seq1 := []ParamKind{ParamSeq}
	reg("JSON", (*Ctx).mJSON, Info{Params: one})
	reg("EXTEND", (*Ctx).mEXTEND, Info{Params: two})
	reg("ITEMS", (*Ctx).mITEMS, Info{Params: one, ReturnsSeq: true})
	reg("CONCAT", (*Ctx).mCONCAT, Info{Params: []ParamKind{ParamSeq, ParamSeq}, ReturnsSeq: true})
	reg("KEYS", (*Ctx).mKEYS, Info{Params: one, ReturnsSeq: true})
	reg("VALUES", (*Ctx).mVALUES, Info{Params: one, ReturnsSeq: true})
	reg("OID_CREATIONTIME", (*Ctx).mOIDCREATIONTIME, Info{Params: one})
	reg("IIF", (*Ctx).mIIF, Info{Params: scalars(3)})
	reg("COALESCE", (*Ctx).mCOALESCE, Info{Params: two})
	reg("LENGTH", (*Ctx).mLENGTH, Info{Params: one})
	reg("TOP", (*Ctx).mTOP, Info{Params: []ParamKind{ParamSeq, ParamScalar}, ReturnsSeq: true})
	reg("UNION", (*Ctx).mUNION, Info{Params: []ParamKind{ParamSeq, ParamSeq}, ReturnsSeq: true})
	reg("EXCEPT", (*Ctx).mEXCEPT, Info{Params: []ParamKind{ParamSeq, ParamSeq}, ReturnsSeq: true})
	reg("DISTINCT", (*Ctx).mDISTINCT, Info{Params: seq1, ReturnsSeq: true})
	reg("RANDOM", (*Ctx).mRANDOM0, Info{Params: scalars(0), Volatile: true})
	reg("RANDOM", (*Ctx).mRANDOM2, Info{Params: two, Volatile: true})
}

// mJSON 把一段 JSON 文本解析成值。
//
// 不是字符串、或者解析不出来，都返回 Null；只有解析过程本身出了硬错误才报错。
func (*Ctx) mJSON(args []*xbson.Value) (*xbson.Value, error) {
	s, ok := str(args[0])
	if !ok {
		return xbson.Null, nil
	}
	v, fatal, ok := jsonParse(s)
	if fatal != nil {
		return nil, fatal
	}
	if !ok {
		return xbson.Null, nil
	}
	return v, nil
}

// Extend 把两篇文档并成一篇新的，同名字段以 ext 为准。两个入参都不改。
func Extend(src, ext *xbson.Document) *xbson.Document {
	out := xbson.NewDocument()
	for k, v := range src.Elements() {
		out.Set(k, v)
	}
	for k, v := range ext.Elements() {
		out.Set(k, v)
	}
	return out
}

// mEXTEND 合并两篇文档。有一边不是文档就返回另一边；两边都不是则返回空文档。
func (*Ctx) mEXTEND(args []*xbson.Value) (*xbson.Value, error) {
	src, srcOK := args[0].AsDocument()
	ext, extOK := args[1].AsDocument()
	switch {
	case srcOK && extOK:
		return Extend(src, ext).Value(), nil
	case srcOK:
		return args[0], nil
	case extOK:
		return args[1], nil
	default:
		return xbson.NewDocument().Value(), nil
	}
}

// mITEMS 把一个值摊成一串。
//
// 数组摊成各项，二进制摊成一串字节值，其余就是它自己一项。
func (*Ctx) mITEMS(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	switch v.Type() {
	case xbson.TypeArray:
		a, _ := v.AsArray()
		return seqOf(a.Items()), nil
	case xbson.TypeBinary:
		b, _ := v.AsBinary()
		out := make([]*xbson.Value, len(b))
		for i, x := range b {
			out[i] = xbson.Int32(int32(x))
		}
		return seqOf(out), nil
	default:
		return seqOf([]*xbson.Value{v}), nil
	}
}

// mCONCAT 把两串值首尾接起来，不去重。
func (*Ctx) mCONCAT(args []*xbson.Value) (*xbson.Value, error) {
	a := items(args[0])
	b := items(args[1])
	out := make([]*xbson.Value, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return seqOf(out), nil
}

// mKEYS 产出文档的字段名，按插入次序。不是文档就产出空串。
func (*Ctx) mKEYS(args []*xbson.Value) (*xbson.Value, error) {
	d, ok := args[0].AsDocument()
	if !ok {
		return emptySeq(), nil
	}
	keys := d.Keys()
	out := make([]*xbson.Value, len(keys))
	for i, k := range keys {
		out[i] = xbson.String(k)
	}
	return seqOf(out), nil
}

// mVALUES 产出文档的字段值，次序同 [Ctx.mKEYS]。
func (*Ctx) mVALUES(args []*xbson.Value) (*xbson.Value, error) {
	d, ok := args[0].AsDocument()
	if !ok {
		return emptySeq(), nil
	}
	keys := d.Keys()
	out := make([]*xbson.Value, len(keys))
	for i, k := range keys {
		out[i] = d.Get(k)
	}
	return seqOf(out), nil
}

// mOIDCREATIONTIME 取出 ObjectId 里嵌的生成时间。不是 ObjectId 就返回 Null。
func (*Ctx) mOIDCREATIONTIME(args []*xbson.Value) (*xbson.Value, error) {
	id, ok := args[0].AsObjectID()
	if !ok {
		return xbson.Null, nil
	}
	return xbson.DateTime(id.Timestamp())
}

// mIIF 三目选择：条件为真取第二个实参，否则取第三个。
//
// 这里两支都已经求过值了。求值器另有一条短路的路子，见 [CallNode.condition]；
// 这个实现留给不走那条路的调用方。
func (*Ctx) mIIF(args []*xbson.Value) (*xbson.Value, error) {
	test, ok := args[0].AsBoolean()
	if !ok {
		return nil, errf("IIF: condition must be a boolean, got %s", args[0].Type())
	}
	if test {
		return args[1], nil
	}
	return args[2], nil
}

// mCOALESCE 第一个不是 Null 就取它，否则取第二个。
func (*Ctx) mCOALESCE(args []*xbson.Value) (*xbson.Value, error) {
	if args[0].IsNull() {
		return args[1], nil
	}
	return args[0], nil
}

// mLENGTH 求长度：字符串按 UTF-16 码元，二进制按字节，数组按项数，文档按字段数。
//
// Null 的长度是 0，其余类型返回 Null。
func (*Ctx) mLENGTH(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	switch v.Type() {
	case xbson.TypeString:
		s, _ := v.AsString()
		return xbson.Int32(int32(utf16Len(s))), nil
	case xbson.TypeBinary:
		b, _ := v.AsBinary()
		return xbson.Int32(int32(len(b))), nil
	case xbson.TypeArray:
		a, _ := v.AsArray()
		return xbson.Int32(int32(a.Len())), nil
	case xbson.TypeDocument:
		d, _ := v.AsDocument()
		return xbson.Int32(int32(d.Len())), nil
	case xbson.TypeNull:
		return xbson.Int32(0), nil
	default:
		return xbson.Null, nil
	}
}

// mTOP 取前 n 项。
//
// n 必须是整型：双精度和十进制都不认，一律产出空串。n 不为正也产出空串。
func (*Ctx) mTOP(args []*xbson.Value) (*xbson.Value, error) {
	n := args[1]
	if n.Type() != xbson.TypeInt32 && n.Type() != xbson.TypeInt64 {
		return emptySeq(), nil
	}

	c, err := int32Of(n)
	if err != nil {
		return nil, err
	}
	if c <= 0 {
		return emptySeq(), nil
	}
	it := items(args[0])
	if int(c) < len(it) {
		it = it[:c]
	}
	return seqOf(it), nil
}

// mUNION 求并集，保留首次出现的次序。
func (*Ctx) mUNION(args []*xbson.Value) (*xbson.Value, error) {
	s := newValueSet()
	out := make([]*xbson.Value, 0, len(items(args[0])))
	for _, v := range items(args[0]) {
		if s.add(v) {
			out = append(out, v)
		}
	}
	for _, v := range items(args[1]) {
		if s.add(v) {
			out = append(out, v)
		}
	}
	return seqOf(out), nil
}

// mEXCEPT 求差集：留下第一串里不在第二串中的项，且自身也去重。
func (*Ctx) mEXCEPT(args []*xbson.Value) (*xbson.Value, error) {
	s := newValueSet()
	for _, v := range items(args[1]) {
		s.add(v)
	}
	var out []*xbson.Value
	for _, v := range items(args[0]) {
		if s.add(v) {
			out = append(out, v)
		}
	}
	return seqOf(out), nil
}

// mDISTINCT 去重，保留首次出现的次序。
func (*Ctx) mDISTINCT(args []*xbson.Value) (*xbson.Value, error) {
	s := newValueSet()
	var out []*xbson.Value
	for _, v := range items(args[0]) {
		if s.add(v) {
			out = append(out, v)
		}
	}
	return seqOf(out), nil
}

// valueSet 是集合运算用的去重表。
//
// 能算出去重键的值按键比，算不出的按指针比——那类值（数组、文档、二进制等）
// 只有同一个实例才算重复。
type valueSet struct {
	byValue map[string]bool
	byIdent map[*xbson.Value]bool
}

// newValueSet 开一张空的去重表。
func newValueSet() *valueSet {
	return &valueSet{byValue: map[string]bool{}, byIdent: map[*xbson.Value]bool{}}
}

// add 把一个值放进表里，返回它是不是头一次出现。
func (s *valueSet) add(v *xbson.Value) bool {
	if k, ok := dedupKey(v); ok {
		if s.byValue[k] {
			return false
		}
		s.byValue[k] = true
		return true
	}
	if s.byIdent[v] {
		return false
	}
	s.byIdent[v] = true
	return true
}

// dedupKey 算一个值的去重键，第二个返回值说明这类值有没有键。
//
// 键带类型前缀，所以同样的数字在不同类型下不算重复。几处细节：
// 双精度的 NaN 都归成同一个键，负零归到正零；十进制先去掉末尾的零再编，
// 于是 1.0 和 1.00 算同一个。
func dedupKey(v *xbson.Value) (string, bool) {
	t := strconv.Itoa(int(v.Type())) + ":"
	switch v.Type() {
	case xbson.TypeMinValue, xbson.TypeNull, xbson.TypeMaxValue:
		return t, true
	case xbson.TypeBoolean:
		b, _ := v.AsBoolean()
		return t + strconv.FormatBool(b), true
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return t + strconv.FormatInt(int64(n), 10), true
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return t + strconv.FormatInt(n, 10), true
	case xbson.TypeDateTime:
		ticks, _ := v.Ticks()
		return t + strconv.FormatInt(ticks, 10), true
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		if math.IsNaN(f) {
			return t + "nan", true
		}
		if f == 0 {
			f = 0
		}
		return t + strconv.FormatFloat(f, 'b', -1, 64), true
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()

		w := decFromXbin(d)
		for w.scale > 0 && new(big.Int).Rem(w.m, bigTen).Sign() == 0 {
			w = dec{m: new(big.Int).Quo(w.m, bigTen), scale: w.scale - 1}
		}
		return t + w.m.String() + "e-" + strconv.Itoa(int(w.scale)), true
	case xbson.TypeString:
		s, _ := v.AsString()
		return t + s, true
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		return t + id.String(), true
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		return t + g.String(), true
	default:
		return "", false
	}
}

// mRANDOM0 取一个 [0, 2147483647) 的随机整数。
func (*Ctx) mRANDOM0(_ []*xbson.Value) (*xbson.Value, error) {
	return xbson.Int32(rand.Int32N(math.MaxInt32)), nil
}

// mRANDOM2 取一个 [lo, hi) 的随机整数。
//
// **上界取不到**，除非上下界相等——那时直接返回它。下界大于上界报错。
func (*Ctx) mRANDOM2(args []*xbson.Value) (*xbson.Value, error) {
	if !isNumber(args[0]) || !isNumber(args[1]) {
		return xbson.Null, nil
	}
	lo, err := int32Of(args[0])
	if err != nil {
		return nil, err
	}
	hi, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	if lo > hi {
		return nil, errf("RANDOM: min %d is greater than max %d", lo, hi)
	}
	if lo == hi {
		return xbson.Int32(lo), nil
	}

	return xbson.Int32(int32(int64(lo) + rand.Int64N(int64(hi)-int64(lo)))), nil
}
