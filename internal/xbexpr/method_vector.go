package xbexpr

import (
	"math"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xvector"
)

// init 登记向量方法。
func init() {
	reg("VECTOR_SIM", (*Ctx).mVECTORSIM, Info{Params: scalars(2)})
}

// mVECTORSIM 求两个向量的余弦距离：1 减去余弦相似度，越小越像。
//
// 两边都可以是向量值或数字数组。维数不等、含 NaN、任一边模长为零，
// 都返回 Null——那些情况下距离没有意义。
func (*Ctx) mVECTORSIM(args []*xbson.Value) (*xbson.Value, error) {
	query, ok := vectorQuery(args[0])
	if !ok {
		return xbson.Null, nil
	}
	cand, ok := vectorCandidate(args[1])
	if !ok {
		return xbson.Null, nil
	}
	if len(query) != len(cand) {
		return xbson.Null, nil
	}

	var dot, magQ, magC float64
	for i, c32 := range cand {
		q, c := query[i], float64(c32)

		if math.IsNaN(c) {
			return xbson.Null, nil
		}
		dot += q * c
		magQ += q * q
		magC += c * c
	}
	if magQ == 0 || magC == 0 {
		return xbson.Null, nil
	}
	cos := 1.0 - dot/(math.Sqrt(magQ)*math.Sqrt(magC))
	if math.IsNaN(cos) {
		return xbson.Null, nil
	}
	return xbson.Double(cos), nil
}

// vectorQuery 把查询侧的值取成双精度数组；数组里有一项不是数就整体失败。
func vectorQuery(v *xbson.Value) ([]float64, bool) {
	switch v.Type() {
	case xbson.TypeVector:
		f, _ := v.AsVector()
		out := make([]float64, len(f))
		for i, x := range f {
			out[i] = float64(x)
		}
		return out, true
	case xbson.TypeArray:
		a, _ := v.AsArray()
		out := make([]float64, 0, a.Len())
		for _, it := range a.Items() {
			f, ok := xvector.ToDouble(it)
			if !ok {
				return nil, false
			}
			out = append(out, f)
		}
		return out, true
	default:
		return nil, false
	}
}

// vectorCandidate 把候选侧的值取成单精度数组。
//
// 与查询侧不同：数组里非数的项换成 NaN 而不是整体失败，
// 由调用方看到 NaN 后返回 Null。
func vectorCandidate(v *xbson.Value) ([]float32, bool) {
	switch v.Type() {
	case xbson.TypeVector:
		return mustVector(v), true
	case xbson.TypeArray:
		a, _ := v.AsArray()
		out := make([]float32, 0, a.Len())
		for _, it := range a.Items() {
			f, ok := xvector.ToDouble(it)
			if !ok {
				out = append(out, float32(math.NaN()))
				continue
			}
			out = append(out, float32(f))
		}
		return out, true
	default:
		return nil, false
	}
}

// mustVector 取向量值，调用方已经确认过类型。
func mustVector(v *xbson.Value) []float32 {
	f, _ := v.AsVector()
	return f
}
