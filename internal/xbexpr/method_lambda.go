package xbexpr

import (
	"slices"
	"strings"

	"github.com/xmapst/xdoc/internal/xbson"
)

// Lambda 是一段能对某一项反复求值的表达式，由求值器实现（见 lambdaBody）。
type Lambda interface {
	Eval(ctx *Ctx, current *xbson.Value) ([]*xbson.Value, error)
	EvalScalar(ctx *Ctx, current *xbson.Value) (*xbson.Value, error)
}

// LambdaFunc 是一个带 lambda 的方法的实现。
type LambdaFunc func(ctx *Ctx, input []*xbson.Value, body Lambda, extra []*xbson.Value) (*xbson.Value, error)

// lambdaDef 是 lambda 方法表里的一项。
type lambdaDef struct {
	fn   LambdaFunc
	info Info
}

// regLambda 登记一个带 lambda 的方法。
func regLambda(name string, argc int, fn LambdaFunc, info Info) {
	lambdaRegistry[key(name, argc)] = lambdaDef{fn: fn, info: info}
}

// init 登记带 lambda 的方法。
//
// SORT 登记了两版：只带 lambda 的，和再带一个排序方向的。
func init() {
	seqIn := Info{Params: []ParamKind{ParamSeq, ParamScalar}, ReturnsSeq: true}
	regLambda("MAP", 2, (*Ctx).lMAP, seqIn)
	regLambda("FILTER", 2, (*Ctx).lFILTER, seqIn)
	regLambda("SORT", 2, (*Ctx).lSORT, seqIn)
	regLambda("SORT", 3, (*Ctx).lSORT, Info{Params: []ParamKind{ParamSeq, ParamScalar, ParamScalar}, ReturnsSeq: true})
}

// LookupLambda 按名字和实参个数找带 lambda 的方法，找不到返回 nil。
//
// 返回的是一层包装：没给 lambda 体时报错，ctx 为空时给一个最小的默认上下文。
func LookupLambda(name string, argc int) LambdaFunc {
	d, ok := lambdaRegistry[key(name, argc)]
	if !ok {
		return nil
	}
	fn := d.fn
	return func(ctx *Ctx, input []*xbson.Value, body Lambda, extra []*xbson.Value) (*xbson.Value, error) {
		if body == nil {
			return nil, errf("%s requires a `input => expression` body", strings.ToUpper(name))
		}
		if ctx == nil {
			ctx = &Ctx{Root: xbson.Null}
		}
		return fn(ctx, input, body, extra)
	}
}

// LookupLambdaInfo 取带 lambda 的方法的签名。
func LookupLambdaInfo(name string, argc int) (Info, bool) {
	d, ok := lambdaRegistry[key(name, argc)]
	if !ok {
		return Info{}, false
	}
	return d.info, true
}

// lMAP 对每一项算一遍 lambda，结果摊平接在一起。
func (ctx *Ctx) lMAP(input []*xbson.Value, body Lambda, _ []*xbson.Value) (*xbson.Value, error) {
	var out []*xbson.Value
	for _, item := range input {
		vals, err := body.Eval(ctx, item)
		if err != nil {
			return nil, err
		}
		out = append(out, vals...)
	}
	return seqOf(out), nil
}

// lFILTER 留下 lambda 算出真的那些项。
//
// 产出的是原项，不是 lambda 的结果；算出来不是布尔值的项一律丢掉。
func (ctx *Ctx) lFILTER(input []*xbson.Value, body Lambda, _ []*xbson.Value) (*xbson.Value, error) {
	var out []*xbson.Value
	for _, item := range input {
		v, err := body.EvalScalar(ctx, item)
		if err != nil {
			return nil, err
		}
		if b, ok := v.AsBoolean(); ok && b {
			out = append(out, item)
		}
	}
	return seqOf(out), nil
}

// lSORT 按 lambda 算出的键排序，键只算一遍。
//
// 排序是稳定的，键相等的项保持原来的先后。方向由第二个实参定，
// 默认升序。
func (ctx *Ctx) lSORT(input []*xbson.Value, body Lambda, extra []*xbson.Value) (*xbson.Value, error) {
	type pair struct {
		item *xbson.Value
		key  *xbson.Value
	}
	pairs := make([]pair, 0, len(input))
	for _, item := range input {
		k, err := body.EvalScalar(ctx, item)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, pair{item: item, key: k})
	}
	asc := true
	if len(extra) > 0 {
		asc = isAscending(extra[0])
	}
	slices.SortStableFunc(pairs, func(a, b pair) int {
		c := a.key.Compare(b.key, ctx.Collation)
		if !asc {
			return -c
		}
		return c
	})
	out := make([]*xbson.Value, len(pairs))
	for i, p := range pairs {
		out[i] = p.item
	}
	return seqOf(out), nil
}

// isAscending 解读排序方向：正整数或字符串 asc 算升序。
//
// 其余一切——负数、零、别的字符串、非数非串——都算降序。
func isAscending(v *xbson.Value) bool {
	if n, ok := v.AsInt32(); ok {
		return n > 0
	}
	if s, ok := v.AsString(); ok {
		return strings.EqualFold(s, "asc")
	}
	return false
}
