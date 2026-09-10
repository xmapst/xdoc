// Package xengine 是文档层：一篇文档怎么落到页上，以及索引怎么跟着走。
//
// 写入的路子是固定的：先把文档编成字节存进数据块，拿到首块地址，再把各个索引
// 的节点建起来指向它。同一篇文档在各索引里的节点串成一条链（见 xstore 的
// DocNodes），删除时顺着链摘干净即可。任何一步失败都会把已经做的撤回去。
//
// 表达式求值不在这里——索引键和向量靠 [KeyFunc]、[ScalarFunc] 从外面注入，
// 本包只管调用它们。
package xengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

// AutoID 说明插入时主键缺失该怎么补。
type AutoID uint8

const (
	// AutoIDNone 不补，文档没写主键就报错。
	AutoIDNone AutoID = iota

	// AutoIDObjectID 生成一个 ObjectId。
	AutoIDObjectID

	// AutoIDGUID 生成一个随机 GUID。
	AutoIDGUID

	// AutoIDInt32 用自增序列，存成 32 位整数。
	AutoIDInt32

	// AutoIDInt64 用自增序列，存成 64 位整数。
	AutoIDInt64
)

// IDField 是主键在文档里的字段名。
const IDField = "_id"

// PrimaryIndexName 是主键索引的名字。
const PrimaryIndexName = "_id"

// PrimaryIndexExpression 是主键索引的表达式文本。主键索引另有快路径，不真的走表达式求值。
const PrimaryIndexExpression = "$._id"

// KeyFunc 按索引表达式算出一篇文档该占的索引键，可能不止一个。
//
// 由上层注入，本包不依赖表达式实现。
type KeyFunc func(expr string, doc *xbson.Value, coll xcoll.Collation) ([]*xbson.Value, error)

// ScalarFunc 按表达式算出一篇文档的一个值，向量索引取向量时用。
type ScalarFunc func(expr string, doc *xbson.Value, coll xcoll.Collation) (*xbson.Value, error)

var (
	// ErrInvalidID 表示主键缺失或者用了不许当主键的类型。
	ErrInvalidID = errors.New("xengine: invalid document id")

	// ErrInvalidDocument 表示传进来的文档是空的。
	ErrInvalidDocument = errors.New("xengine: invalid document")

	// ErrUnsupportedIndex 表示集合上有本实现维护不了的索引类型，此时不许写。
	ErrUnsupportedIndex = errors.New("xengine: collection has an index this implementation cannot maintain")

	// ErrInvalidCollectionName 表示集合名不合法。
	ErrInvalidCollectionName = errors.New("xengine: invalid collection name")

	// ErrCollectionNotFound 表示集合不存在。
	ErrCollectionNotFound = errors.New("xengine: collection not found")

	// ErrSequenceNotNumeric 表示集合里最大的主键不是数，自增无从算起。
	ErrSequenceNotNumeric = errors.New("xengine: collection has non-numeric ids, cannot auto-increment")
)

// Engine 是文档层：增删改查、建索引、维护自增序列。
//
// 自增序列缓存在内存里，自带锁；其余状态都在事务层。
type Engine struct {
	core *xtx.Core
	coll xcoll.Collation
	keys KeyFunc

	// scalar 求单值，向量索引用。
	scalar ScalarFunc

	// seqMu 保护 seq。
	seqMu sync.Mutex

	// seq 是各集合当前的自增序列值，只在内存里。
	//
	// 进程重启后第一次要用时从索引里现查最大主键，见 [Engine.lastID]。
	seq map[string]int64

	// obs 旁听写入与自开事务的结局，见 [Engine.SetObserver]。
	obs Observer
}

// New 造一个文档层引擎。
func New(core *xtx.Core, coll xcoll.Collation, keys KeyFunc, scalar ScalarFunc) *Engine {
	return &Engine{
		core: core, coll: coll, keys: keys, scalar: scalar,
		seq: map[string]int64{},
	}
}

// DateLocation 返回时间该按哪个时区解读。
func (e *Engine) DateLocation() *time.Location { return e.core.DateLocation() }

// decode 把一段字节解成文档，时间按库设的时区落定。
func (e *Engine) decode(b []byte) (*xbson.Document, error) {
	return xbson.DecodeIn(b, e.core.DateLocation())
}

// Core 返回底下的事务层实例。
func (e *Engine) Core() *xtx.Core { return e.core }

// Collation 返回排序规则。
func (e *Engine) Collation() xcoll.Collation { return e.coll }

// ValidateCollectionName 校验集合名。
//
// 不能为空，不能以美元号开头（那是虚拟集合的前缀），首字符是字母或下划线，
// 后续再加数字和美元号。
func ValidateCollectionName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidCollectionName)
	}
	if strings.HasPrefix(name, "$") {
		return fmt.Errorf("%w: %q starts with $, which is reserved", ErrInvalidCollectionName, name)
	}
	if r, ok := badNameChar(name); !ok {
		return fmt.Errorf("%w: %q cannot contain %q at that position; a name starts with a letter or _ "+
			"and continues with letters, digits, _ or $", ErrInvalidCollectionName, name, r)
	}
	return nil
}

// badNameChar 找出第一个不合法的字符。按 UTF-16 码元逐个看，与字段名的口径一致。
func badNameChar(name string) (rune, bool) {
	for i, u := range utf16.Encode([]rune(name)) {
		r := rune(u)
		var ok bool
		if i == 0 {
			ok = r == '_' || unicode.IsLetter(r)
		} else {
			ok = r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
		}
		if !ok {
			return r, false
		}
	}
	return 0, true
}

// validateID 校验主键：不能是空的，也不能是 Null 或者两个哨兵值。
func validateID(v *xbson.Value) error {
	if v == nil {
		return fmt.Errorf("%w: %s is nil", ErrInvalidID, IDField)
	}
	switch v.Type() {
	case xbson.TypeNull, xbson.TypeMinValue, xbson.TypeMaxValue:
		return fmt.Errorf("%w: %s is not allowed as %s", ErrInvalidID, v.Type(), IDField)
	}
	return nil
}

// inTx 开一个事务跑 fn，成功就提交，出错就回滚。
//
// fn 返回之后还会再查一次上下文：中途被取消的话，即便 fn 自己没报错也要回滚。
func (e *Engine) inTx(ctx context.Context, fn func(tx *xtx.Transaction) error) error {
	tx, err := e.core.Begin(ctx)
	if err != nil {
		return err
	}

	done, committed := false, false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
		e.finished(ctx, tx, committed)
	}()
	if err := fn(tx); err != nil {
		done = true
		return errors.Join(err, tx.Rollback())
	}

	if err := ctx.Err(); err != nil {
		done = true
		return errors.Join(err, tx.Rollback())
	}
	done = true
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// oneShot 是 [Engine.inTx] 的带计数版本。事务失败时计数一律归零。
func (e *Engine) oneShot(ctx context.Context, fn func(*xtx.Transaction) (int, error)) (int, error) {
	var n int
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		var err error
		n, err = fn(tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// InTx 是导出的 [Engine.inTx]，给要把几步写入并进同一个事务的调用方用。
func (e *Engine) InTx(ctx context.Context, fn func(tx *xtx.Transaction) error) error {
	return e.inTx(ctx, fn)
}

// writeSnapshot 取一份可写快照，必要时把集合和主键索引一并建出来。
//
// **只在集合确实不存在时才校验名字**：已经建好的集合即便名字不合规也照样能用。
// 集合一个索引都没有时补建主键索引——那是所有读写的入口。
func (e *Engine) writeSnapshot(ctx context.Context, tx *xtx.Transaction, name string, create bool) (*xtx.Snapshot, error) {
	if create {
		exists := false
		if err := tx.InspectHeader(func(h *xpage.HeaderPage) error {
			_, exists = h.Collection(name)
			return nil
		}); err != nil {
			return nil, err
		}
		if !exists {
			if err := ValidateCollectionName(name); err != nil {
				return nil, err
			}
		}
	}
	s, err := tx.Snapshot(ctx, name, xtx.ModeWrite, create)
	if err != nil {
		return nil, err
	}
	if s.CollectionPage() == nil {
		if !create {
			return nil, fmt.Errorf("%w: %q", ErrCollectionNotFound, name)
		}
		return nil, fmt.Errorf("xengine: collection %q was not created", name)
	}

	if len(s.CollectionPage().Indexes()) == 0 {
		if _, err := xstore.New(s).CreateIndex(PrimaryIndexName, PrimaryIndexExpression, true); err != nil {
			return nil, fmt.Errorf("xengine: create primary index for %q: %w", name, err)
		}
	}
	return s, nil
}

// readSnapshot 取一份只读快照；集合不存在时返回 nil 而不是错误。
func (e *Engine) readSnapshot(ctx context.Context, tx *xtx.Transaction, name string) (*xtx.Snapshot, error) {
	s, err := tx.Snapshot(ctx, name, xtx.ModeRead, false)
	if err != nil {
		return nil, err
	}
	if s.CollectionPage() == nil {
		return nil, nil
	}
	return s, nil
}

// work 给快照添几个本包用的便利方法。
type work struct {
	*xtx.Snapshot
}

// primaryIndex 取主键索引；没有就报损坏——集合建起来时就该有它。
func (w work) primaryIndex() (*xpage.CollectionIndex, error) {
	ix, ok := w.CollectionPage().PrimaryIndex()
	if !ok {
		return nil, fmt.Errorf("%w: collection %q has no primary index",
			xpage.ErrCorrupt, w.Name())
	}
	return ix, nil
}

// indexErr 给索引操作的错误标上索引名。键重复那个例外，它本身已经说明白了，且调用方要按原样识别它。
func indexErr(name string, err error) error {
	if errors.Is(err, xstore.ErrDuplicateKey) {
		return err
	}
	return fmt.Errorf("index %q: %w", name, err)
}
