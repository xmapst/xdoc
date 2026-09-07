package xbexpr

import (
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// init 登记聚合方法。它们都收一串值，产出一个值。
func init() {
	seq1 := []ParamKind{ParamSeq}
	reg("COUNT", (*Ctx).mCOUNT, Info{Params: seq1})
	reg("MIN", (*Ctx).mMIN, Info{Params: seq1})
	reg("MAX", (*Ctx).mMAX, Info{Params: seq1})
	reg("FIRST", (*Ctx).mFIRST, Info{Params: seq1})
	reg("LAST", (*Ctx).mLAST, Info{Params: seq1})
	reg("AVG", (*Ctx).mAVG, Info{Params: seq1})
	reg("SUM", (*Ctx).mSUM, Info{Params: seq1})
	reg("ANY", (*Ctx).mANY, Info{Params: seq1})
}

// mCOUNT 数一串值有几项。
func (*Ctx) mCOUNT(args []*xbson.Value) (*xbson.Value, error) {
	return xbson.Int32(int32(len(items(args[0])))), nil
}

// mMIN 求最小值，按二进制序比。
//
// 空序列返回 MinValue——那是比任何值都小的哨兵。
func (*Ctx) mMIN(args []*xbson.Value) (*xbson.Value, error) {
	minV := xbson.MaxValue
	for _, v := range items(args[0]) {
		if v.Compare(minV, xcoll.Binary) <= 0 {
			minV = v
		}
	}
	if minV.Type() == xbson.TypeMaxValue {
		return xbson.MinValue, nil
	}
	return minV, nil
}

// mMAX 求最大值，按二进制序比。
//
// 空序列返回 MaxValue——那是比任何值都大的哨兵。
func (*Ctx) mMAX(args []*xbson.Value) (*xbson.Value, error) {
	maxV := xbson.MinValue
	for _, v := range items(args[0]) {
		if v.Compare(maxV, xcoll.Binary) >= 0 {
			maxV = v
		}
	}
	if maxV.Type() == xbson.TypeMinValue {
		return xbson.MaxValue, nil
	}
	return maxV, nil
}

// mFIRST 取第一项，空序列返回 Null。
func (*Ctx) mFIRST(args []*xbson.Value) (*xbson.Value, error) {
	it := items(args[0])
	if len(it) == 0 {
		return xbson.Null, nil
	}
	return it[0], nil
}

// mLAST 取最后一项，空序列返回 Null。
func (*Ctx) mLAST(args []*xbson.Value) (*xbson.Value, error) {
	it := items(args[0])
	if len(it) == 0 {
		return xbson.Null, nil
	}
	return it[len(it)-1], nil
}

// mSUM 求和，非数的项跳过。
//
// 从 32 位整数零起加，类型随加法逐步升上去；一项都没加到就返回整数零。
func (*Ctx) mSUM(args []*xbson.Value) (*xbson.Value, error) {
	sum := xbson.Int32(0)
	for _, v := range items(args[0]) {
		if !isNumber(v) {
			continue
		}
		next, err := opKindAdd.numBinary(sum, v)
		if err != nil {
			return nil, err
		}
		sum = next
	}
	return sum, nil
}

// mAVG 求平均，非数的项既不计入和也不计入个数。一项都没有时返回整数零。
func (*Ctx) mAVG(args []*xbson.Value) (*xbson.Value, error) {
	sum := xbson.Int32(0)
	count := int32(0)
	for _, v := range items(args[0]) {
		if !isNumber(v) {
			continue
		}
		next, err := opKindAdd.numBinary(sum, v)
		if err != nil {
			return nil, err
		}
		sum = next
		count++
	}
	if count == 0 {
		return xbson.Int32(0), nil
	}
	return numDiv(sum, xbson.Int32(count))
}

// mANY 判断序列非空。
func (*Ctx) mANY(args []*xbson.Value) (*xbson.Value, error) {
	return xbson.Boolean(len(items(args[0])) > 0), nil
}
