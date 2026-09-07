package xdoc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xerr"
	"github.com/xmapst/xdoc/internal/xpage"
)

// RebuildErrorsCollection 是重建报告落进新库的集合名。
//
// 一次搬不干净的重建会把跳过的每一处写成这个集合里的一篇文档。名字是格式的
// 一部分：写出的库拿到别的工具那里，照样找得到这个集合。
const RebuildErrorsCollection = "_rebuild_errors"

// newBuildID 为一次重建生成标识，同一次的所有行共用它。
//
// 先按 v4 的规矩造一个随机 GUID（第 6、8 字节打上版本与变体位），
// 再从第 6 个字符起截掉开头——报告里这一列只用来归堆，不用来做全局唯一。
func newBuildID() string {
	var b [16]byte

	_, _ = rand.Read(b[:])

	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	guid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	return guid[6:]
}

// errorDoc 把一处跳过整理成报告文档。
//
// 有三个字段在这里没有对应物，一律写 Null 或固定值而不是编一个：
// hresult 是宿主运行时的 32 位错误号，Go 的 error 没有这个东西；
// stacktrace 要求错误里带着调用栈，本实现的错误不带；
// origin 用来区分数据文件与日志，而这里搬运读的是两者合并之后的视图，
// 一页来自哪个文件在那一层已经看不见，所以恒为 Data。
//
// 报告里也没有「出问题的集合名」这一项，那个字段在格式里就是缺的。
func (s SalvageSkip) errorDoc(buildID string, created *Value) *Document {
	exc := xbson.NewDocument()
	exc.Set("code", xbson.Int32(errorCodeOf(s.Err)))
	exc.Set("hresult", xbson.Null)
	exc.Set("type", xbson.String(fmt.Sprintf("%T", s.Err)))
	exc.Set("inner", innerMessage(s.Err))
	exc.Set("stacktrace", xbson.Null)

	d := xbson.NewDocument()
	d.Set("buildId", xbson.String(buildID))
	d.Set("created", created)
	d.Set("pageID", xbson.Int32(int32(s.PageID)))
	d.Set("positionID", xbson.Int64(int64(s.PageID)*xpage.PageSize))
	d.Set("origin", xbson.String("Data"))
	d.Set("pageType", xbson.String(s.pageTypeName()))
	d.Set("message", xbson.String(s.Err.Error()))
	d.Set("exception", exc.Value())
	return d
}

// pageTypeName 把槽号折成页类型名：整页读不出来时记 Empty，否则记 Data。
func (s SalvageSkip) pageTypeName() string {
	if s.Slot >= 0 {
		return "Data"
	}
	return "Empty"
}

// errorCodeOf 取出错误链上的错误码，链上没有码时返回 -1。
//
// 两种形态都认：带上下文的错误，以及裸的码值本身。
func errorCodeOf(err error) int32 {
	if e, ok := errors.AsType[*xerr.Error](err); ok {
		return int32(e.Code)
	}
	if c, ok := errors.AsType[xerr.Code](err); ok {
		return int32(c)
	}
	return -1
}

// innerMessage 取底层原因的文案，没有底层原因时给 Null。
//
// 先认带上下文的那种错误、再退回通用的 Unwrap：前者的 Unwrap 同时暴露码与
// 底层原因两条线，直接走通用分支会把码当成"底层原因"取出来。
func innerMessage(err error) *Value {
	switch u := err.(type) {
	case *xerr.Error:
		if u.Err != nil {
			return xbson.String(u.Err.Error())
		}
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			return xbson.String(inner.Error())
		}
	}
	return xbson.Null
}

// writeRebuildErrors 把这一批跳过写进新库的报告集合。
//
// created 由调用方统一给：同一次重建的所有行必须是同一个时刻，各行自己去取
// 会差出几毫秒，按 buildId 归堆之后还要再猜哪几行是一批的。
//
// 主键用集合内自增整数，报告本身不需要按时间排序的标识。
func (db *DB) writeRebuildErrors(ctx context.Context, skips []SalvageSkip, at time.Time) error {
	created, err := xbson.DateTime(at)
	if err != nil {
		created = xbson.Null
	}
	buildID := newBuildID()
	docs := make([]*Document, 0, len(skips))
	for _, s := range skips {
		docs = append(docs, s.errorDoc(buildID, created))
	}

	c := db.Collection(RebuildErrorsCollection).WithAutoID(AutoIDInt32)
	if _, err := c.Insert(ctx, docs...); err != nil {
		return err
	}
	return nil
}
