// Package xdoc 是一个嵌入式文档数据库：整个库就是一份文件，没有服务端进程。
//
// 存的是文档——有序的键值对，值可以嵌套。文档按集合归拢，集合不必预先建，
// 第一次写入时自动出现，也不要求同一集合里的文档字段一致。
//
// 起手是 [Open]（或 [OpenMemory]），拿到 [DB] 之后从 [DB.Collection] 取集合句柄：
//
//	db, err := xdoc.Open("data.db")
//	defer db.Close()
//	c := db.Collection("users")
//	_, err = c.Insert(ctx, xdoc.Doc("name", "ann", "age", 30))
//
// # 三套写法
//
// 同一件事有三种表达，可以混着用：
//
//   - 文档式：[Collection] 上的方法直接收发 [Document]。
//   - 类型式：[DB.Typed] 绑定一个 Go 结构体，收发的就是那个类型。
//   - SQL：[DB.Execute] 收一条语句。
//
// 三者走的是同一套执行路径，行为一致。
//
// # 表达式
//
// 查询条件、投影、索引取键、排序键都是同一种表达式语言，`$` 指当前文档，
// `@name` 引用参数。**不要把外部输入拼进表达式串**——用参数：
// [QueryBuilder.Param] 或 [DB.Execute] 的可变参数。
//
// # 事务
//
// [DB.Transaction] 把一段操作包进一个事务，回调返回 error 就整体回滚。
// 单独的一次插入或更新自带事务，不必额外包。
//
// # 并发
//
// [DB] 可以被多个 goroutine 同时使用。同一份文件要被多个**进程**打开，
// 开的时候带上 [WithConnection] 选 [ConnectionShared]。
package xdoc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xlock"
	"github.com/xmapst/xdoc/internal/xmap"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xquery"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

type (
	// Value 是一个文档值：可以是数字、字符串、数组、文档等等。
	Value = xbson.Value

	// Document 是一篇有序的键值文档，键的先后次序会存进文件。
	Document = xbson.Document

	// Array 是一列值。
	Array = xbson.Array

	// ObjectID 是 12 字节的主键，前 4 字节是时间戳，所以按值排序大致等于按生成时间排序。
	ObjectID = xbson.ObjectID

	// Type 是值的类型标记。
	Type = xbson.Type

	// Mapper 负责 Go 值与文档值之间的互转。
	Mapper = xmap.Mapper
)

// AutoID 说明插入时没带主键的文档该发一个什么样的主键。
type AutoID = xengine.AutoID

const (
	// AutoIDNone 不发：没带主键的文档会被拒绝。
	AutoIDNone = xengine.AutoIDNone

	// AutoIDObjectID 发一个 [ObjectID]。
	AutoIDObjectID = xengine.AutoIDObjectID

	// AutoIDGUID 发一个随机 GUID。
	AutoIDGUID = xengine.AutoIDGUID

	// AutoIDInt32 发一个 int32 自增值。
	AutoIDInt32 = xengine.AutoIDInt32

	// AutoIDInt64 发一个 int64 自增值。
	AutoIDInt64 = xengine.AutoIDInt64
)

// ErrNotFound 表示按主键没找到文档。
var ErrNotFound = errors.New("xdoc: not found")

// ErrAlreadyOpen 表示这份文件已经被别的进程独占着。
var ErrAlreadyOpen = xtx.ErrAlreadyOpen

// ErrBroken 表示这个库句柄已经不能再用了。
//
// 发生过一次写到一半的失败之后，内存里的状态与文件对不上，
// 继续用下去只会写出更多不一致。重新打开即可。
var ErrBroken = xtx.ErrBroken

// ErrClosed 表示这个库句柄已经关了。
var ErrClosed = xtx.ErrClosed

// ErrCollectionTableFull 表示集合表满了，建不了新集合。
//
// 集合表是头页里固定大小的一块，装得下多少个集合取决于名字长短。
var ErrCollectionTableFull = xpage.ErrCollectionTableFull

// ErrNeedsRebuild 表示这份文件要先重建才能打开。
//
// 用 [Rebuild]，或者开的时候带上 [WithAutoRebuild]。
var ErrNeedsRebuild = xtx.ErrNeedsRebuild

// ErrDuplicateKey 表示违反了唯一索引。
var ErrDuplicateKey = xstore.ErrDuplicateKey

// IsCorrupt 判断一个错误是不是文件损坏。
//
// 损坏与"用错了"是两回事：前者要重建或从备份恢复，重试没有意义。
func IsCorrupt(err error) bool {
	return errors.Is(err, xpage.ErrCorrupt) || errors.Is(err, xbson.ErrCorrupt)
}

// IsExprError 判断一个错误出在表达式或查询的解析、求值上。
func IsExprError(err error) bool {
	return errors.Is(err, xbexpr.ErrExpr) || errors.Is(err, xquery.ErrQuery)
}

// parsedExpr 是一次表达式解析的结果，成败都记下来。
//
// 失败的也缓存：一个写错的索引表达式每篇文档都会碰一次，
// 不缓存就等于每篇文档重新解析一遍同一个错误。
type parsedExpr struct {
	node xbexpr.Node
	err  error
}

// DB 是一份打开着的库。
//
// 它可以被多个 goroutine 同时使用。
type DB struct {
	core   *xtx.Core
	engine *xengine.Engine
	mapper *Mapper
	auto   AutoID

	// 查询执行器按需建一次：只做插删改的用法不必付它的代价。
	execOnce sync.Once
	exec     *xquery.Executor

	// dataPath 是数据文件路径，内存库为空。
	dataPath string

	password string

	// sqlTx 是 SQL 的 BEGIN 开出来的那个事务，由 sqlMu 守着。
	sqlMu sync.Mutex
	sqlTx *Tx

	// cursors 记着当前正在遍历的查询，供 $open_cursors 查看。
	cursors cursorSet

	opts options

	// shared 非空表示这个句柄走共享连接：底层文件按需开关，跨进程锁挡着别人。
	shared *sharedState
}

// executor 返回查询执行器，第一次用到时才建。
//
// 返回 error 是为了让调用点统一，实际上它从不失败。
func (db *DB) executor() (*xquery.Executor, error) {
	db.execOnce.Do(func() {
		db.exec = xquery.New(db.core, db.core.Collation(), db.dataPath, db.password)
	})
	return db.exec, nil
}

// options 是打开一份库时的全部可调项。
type options struct {
	readOnly     bool
	syncOnCommit bool
	cacheSize    int
	mapper       *Mapper
	auto         AutoID
	now          func() time.Time
	password     string
	initialSize  int64
	autoRebuild  bool
	conn         ConnectionType
	coll         xcoll.Collation
	collSet      bool
	collErr      error
}

// Option 调整打开库时的行为。
type Option func(*options)

// newOptions 依次应用各选项。
//
// 默认：提交时落盘、用默认映射器、主键发 ObjectId、二进制排序。
// nil 选项直接跳过，让调用方能写出条件性的选项列表。
func newOptions(opts []Option) options {
	o := options{syncOnCommit: true, mapper: xmap.Default, auto: AutoIDObjectID, coll: xcoll.Default}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// ReadOnly 以只读方式打开，任何写入都会被拒绝。
func ReadOnly() Option { return func(o *options) { o.readOnly = true } }

// WithoutSyncOnCommit 提交时不等落盘。
//
// **进程崩溃只丢最近的提交，操作系统崩溃或断电则可能丢更多**：
// 写出去的字节还在系统缓存里。批量导入这类可以重来的场合值得用它。
func WithoutSyncOnCommit() Option { return func(o *options) { o.syncOnCommit = false } }

// WithCacheSize 设置页缓存能放多少页。
func WithCacheSize(pages int) Option { return func(o *options) { o.cacheSize = pages } }

// WithMapper 换掉 Go 值与文档值互转的映射器。
//
// nil 被忽略，保留默认那个。
func WithMapper(m *Mapper) Option {
	return func(o *options) {
		if m != nil {
			o.mapper = m
		}
	}
}

// WithAutoID 设置库级默认的主键生成方式。
func WithAutoID(a AutoID) Option { return func(o *options) { o.auto = a } }

// WithPassword 用口令加密这份文件。
//
// 新建时定下加密，之后每次打开都要给同一个口令。改口令走 [Rebuild]。
func WithPassword(pw string) Option { return func(o *options) { o.password = pw } }

// WithAutoRebuild 让打开时遇到"要先重建"的文件就自动重建一次再开。
//
// 重建会把原文件改名成备份，所以这个选项会动磁盘上的东西。
func WithAutoRebuild() Option { return func(o *options) { o.autoRebuild = true } }

// WithInitialSize 新建文件时先占下这么多字节。
//
// 一次占够比写着写着一点点长省事，也让文件在磁盘上更连续。
func WithInitialSize(size int64) Option { return func(o *options) { o.initialSize = size } }

// WithCollation 设置字符串的比较与排序规则，只在新建时起作用。
//
// 解析失败不在这里报，留到 [Open] 时——选项函数没有 error 出口。
func WithCollation(s string) Option {
	return func(o *options) {
		c, err := xcoll.Parse(s)
		if err != nil {
			o.collErr = err
			return
		}
		o.coll, o.collSet = c, true
	}
}

// withCollation 直接设排序规则，重建时用来沿用源库那个。
func withCollation(c xcoll.Collation) Option {
	return func(o *options) { o.coll, o.collSet = c, true }
}

// withNow 换掉取当前时间的方式，测试用。
func withNow(fn func() time.Time) Option { return func(o *options) { o.now = fn } }

// nowOrDefault 取当前时间。
func (o options) nowOrDefault() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// Open 打开或新建一份库文件。
//
// 带 [WithAutoRebuild] 时，遇到"要先重建"会重建一次再开；重建也失败的话，
// 两个错误一起返回，好看出到底是哪一步卡住的。
//
// 用完要 Close。
func Open(path string, opts ...Option) (*DB, error) {
	o := newOptions(opts)
	if o.collErr != nil {
		return nil, o.collErr
	}

	if o.conn == ConnectionShared {
		return o.newSharedDB(path)
	}
	core, err := o.openOptions().OpenFile(path)
	if err != nil && o.autoRebuild && errors.Is(err, xtx.ErrNeedsRebuild) {
		if _, rerr := o.rebuildFile(path, opts); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		core, err = o.openOptions().OpenFile(path)
	}
	if err != nil {
		return nil, err
	}
	db := o.newDB(core)
	db.dataPath = path
	return db, nil
}

// OpenMemory 开一份只在内存里的库，关掉即消失。
//
// 页格式与文件库完全一样，所以行为也一样。
func OpenMemory(opts ...Option) (*DB, error) {
	o := newOptions(opts)
	if o.collErr != nil {
		return nil, o.collErr
	}

	if o.conn == ConnectionShared {
		return o.newSharedDB("")
	}
	core, err := o.openOptions().OpenMemory()
	if err != nil {
		return nil, err
	}
	return o.newDB(core), nil
}

// newSharedDB 建一个共享连接的库句柄。
//
// 这时候还不开文件：共享连接是用到才开、闲下来就关，好让别的进程能插进来。
func (o options) newSharedDB(path string) (*DB, error) {
	lockPath := path
	if lockPath == "" {
		lockPath = ":memory:"
	}
	db := &DB{mapper: o.mapper, auto: o.auto, password: o.password, opts: o}
	db.dataPath = path
	lk, err := xlock.StrategyDefault.Open(lockPath, db)
	if err != nil {
		return nil, err
	}
	db.shared = &sharedState{lock: lk}
	return db, nil
}

// openOptions 把公开选项翻成底层的打开参数。
func (o options) openOptions() xtx.OpenOptions {
	return xtx.OpenOptions{
		ReadOnly:     o.readOnly,
		SyncOnCommit: o.syncOnCommit,

		Collation:    o.coll,
		CollationSet: o.collSet,
		CacheSize:    o.cacheSize,
		Now:          o.now,
		Password:     o.password,
		InitialSize:  o.initialSize,
	}
}

// newDB 在一个已经打开的底层之上装配出库句柄。
//
// 索引表达式的解析结果按表达式串缓存：同一个索引对每篇文档都要算一次取键，
// 每次重新解析的代价会随文档数放大。
//
// 文档为 nil 时先解析、再返回空结果——这样一个写错的索引表达式在
// 建索引的那一刻就报出来，而不是等到插第一篇文档。
func (o options) newDB(core *xtx.Core) *DB {
	var cache sync.Map

	compile := func(expr string) (parsedExpr, error) {
		if v, ok := cache.Load(expr); ok {
			pe := v.(parsedExpr)
			return pe, pe.err
		}
		var pe parsedExpr
		pe.node, pe.err = xbexpr.Parse(expr)
		if pe.err != nil {
			pe.err = fmt.Errorf("xdoc: index expression %q: %w", expr, pe.err)
		}

		cache.Store(expr, pe)
		return pe, pe.err
	}
	keys := func(expr string, doc *Value, coll xcoll.Collation) ([]*Value, error) {
		pe, err := compile(expr)
		if err != nil {
			return nil, err
		}

		if doc == nil {
			return nil, nil
		}

		return xbexpr.IndexKeys(pe.node, doc, coll, core.DateLocation())
	}

	scalar := func(expr string, doc *Value, coll xcoll.Collation) (*Value, error) {
		pe, err := compile(expr)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			return nil, nil
		}
		return xbexpr.ExecuteScalar(pe.node, doc, nil, coll)
	}
	return &DB{
		core:     core,
		engine:   xengine.New(core, core.Collation(), keys, scalar),
		mapper:   o.mapper,
		auto:     o.auto,
		password: o.password,
		opts:     o,
	}
}

// Close 关掉这份库。
//
// 执行器与底层都要关，两边的错误合并返回：先关执行器失败也不能不关文件。
func (db *DB) Close() error {
	if db.shared != nil {
		return db.closeShared()
	}
	var eerr error
	if db.exec != nil {
		eerr = db.exec.Close()
	}
	return errors.Join(eerr, db.core.Close())
}

// Collection 返回一个集合句柄。
//
// 不检查集合在不在，也不会建它：集合是第一次写入时才建的。
func (db *DB) Collection(name string) *Collection {
	return &Collection{db: db, name: name, auto: db.auto}
}

// CollectionNames 返回全部用户集合名，不含 $ 打头的虚拟集合。
func (db *DB) CollectionNames() []string {
	var names []string
	db.withCore(func() { names = db.engine.CollectionNames() })
	return names
}

// DropCollection 删掉一个集合，本来就不存在时返回 false 而不报错。
func (db *DB) DropCollection(ctx context.Context, name string) (bool, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return db.engine.DropCollection(ctx, name)
}

// RenameCollection 给集合改名，源集合不存在时返回 false 而不报错。
func (db *DB) RenameCollection(ctx context.Context, old, name string) (bool, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return db.engine.RenameCollection(ctx, old, name)
}

// Checkpoint 把日志里的页搬回数据文件，返回搬了几页。
//
// 平时到了阈值会自动做；手工调用是为了在关库前或备份前把日志清空。
func (db *DB) Checkpoint(ctx context.Context) (int, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return db.core.Checkpoint(ctx)
}

// CollectionExists 报告某个集合在不在，名字不区分大小写。
func (db *DB) CollectionExists(name string) bool {
	for _, n := range db.CollectionNames() {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// UserVersion 读出调用方自己维护的版本号，本库不解释它的含义。
func (db *DB) UserVersion() int32 {
	var v int32
	db.withCore(func() {
		v, _ = db.core.HeaderValue(func(h *xpage.HeaderPage) (int32, error) { return h.UserVersion(), nil })
	})
	return v
}

// SetUserVersion 写入调用方自己维护的版本号。
func (db *DB) SetUserVersion(ctx context.Context, v int32) error {
	_, err := db.SetPragma(ctx, "USER_VERSION", xbson.Int32(v))
	return err
}

// Timeout 是等锁的时限。
func (db *DB) Timeout() time.Duration {
	var d time.Duration
	db.withCore(func() {
		d, _ = db.core.HeaderValue(func(h *xpage.HeaderPage) (time.Duration, error) { return h.Timeout(), nil })
	})
	return d
}

// SetTimeout 设置等锁时限。
//
// 存的是整秒，不足一秒的部分被截掉。
func (db *DB) SetTimeout(ctx context.Context, d time.Duration) error {
	_, err := db.SetPragma(ctx, "TIMEOUT", xbson.Int32(int32(d/time.Second)))
	return err
}

// UTCDate 报告日期是按 UTC 还是按本地时区读出来。
func (db *DB) UTCDate() bool {
	var v bool
	db.withCore(func() {
		v, _ = db.core.HeaderValue(func(h *xpage.HeaderPage) (bool, error) { return h.UTCDate(), nil })
	})
	return v
}

// SetUTCDate 设置日期按 UTC 还是按本地时区读出来。
func (db *DB) SetUTCDate(ctx context.Context, v bool) error {
	_, err := db.SetPragma(ctx, "UTC_DATE", xbson.Boolean(v))
	return err
}

// LimitSize 是数据文件允许长到多大。
func (db *DB) LimitSize() int64 {
	var v int64
	db.withCore(func() {
		v, _ = db.core.HeaderValue(func(h *xpage.HeaderPage) (int64, error) { return h.LimitSize(), nil })
	})
	return v
}

// SetLimitSize 设置数据文件的大小上限，不能小于当前大小。
func (db *DB) SetLimitSize(ctx context.Context, v int64) error {
	_, err := db.SetPragma(ctx, "LIMIT_SIZE", xbson.Int64(v))
	return err
}

// CheckpointSize 是日志攒到多少页就自动搬回数据文件，0 表示不自动搬。
func (db *DB) CheckpointSize() int32 {
	var v int32
	db.withCore(func() {
		v, _ = db.core.HeaderValue(func(h *xpage.HeaderPage) (int32, error) { return h.Checkpoint(), nil })
	})
	return v
}

// SetCheckpointSize 设置自动搬运的阈值。
func (db *DB) SetCheckpointSize(ctx context.Context, v int32) error {
	_, err := db.SetPragma(ctx, "CHECKPOINT", xbson.Int32(v))
	return err
}

// Collation 返回这份文件的字符串比较与排序规则。
func (db *DB) Collation() xcoll.Collation {
	var c xcoll.Collation
	db.withCore(func() { c = db.core.Collation() })
	return c
}

// errNilDocument 表示要解的文档是 nil。
var errNilDocument = errors.New("xdoc: cannot decode a nil document")

// Marshal 用这个库的映射器把 Go 值转成文档值。
func (db *DB) Marshal(v any) (*Value, error) { return db.mapper.Marshal(v) }

// MarshalDocument 用这个库的映射器把 Go 值转成一篇文档。
func (db *DB) MarshalDocument(v any) (*Document, error) { return db.mapper.MarshalDocument(v) }

// Unmarshal 用这个库的映射器把一篇文档解进 out。
func (db *DB) Unmarshal(d *Document, out any) error {
	if d == nil {
		return errNilDocument
	}
	return db.mapper.Unmarshal(DocValue(d), out)
}

// xmapMarshal 用默认映射器编码，供不挂在具体库上的地方使用。
func xmapMarshal(v any) (*Value, error) { return xmap.Marshal(v) }
