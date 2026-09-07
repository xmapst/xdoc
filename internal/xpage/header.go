package xpage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

const (
	// 头页里各字段的偏移。布局是格式的一部分，不能改。
	//
	// 集合表从 192 字节起，一直到页尾。
	offMagic             = 32
	offFileVersion       = 59
	offFreeEmptyPageList = 60
	offLastPageID        = 64
	offCreationTime      = 68
	offUserVersion       = 76
	offCollationLCID     = 80
	offCollationOptions  = 84
	offTimeout           = 88
	offUTCDate           = 96
	offCheckpoint        = 97
	offLimitSize         = 101
	offInvalidState      = 191
	offCollections       = 192

	collectionsSize = PageSize - offCollections
)

const (
	// 格式标识在头页里的位置与长度，以及版本号字段的位置。
	//
	// 导出来是给需要在打开之前先看一眼文件的地方用。
	MagicOffset = offMagic

	MagicSize = offFileVersion - offMagic

	VersionOffset = offFileVersion
)

// magicText 是文件开头的格式标识。
//
// 打开时它是**逐字节比对**的（见 [LoadHeaderPage]），所以这一行就是
// 「这份文件属于谁」的唯一控制点：改了它，已有的文件就打不开了，
// 而别的实现写出的同构文件在这里也过不去。
const magicText = "** This is a XDoc file **"

// 编译期断言：标识文本不能超过为它留的字节数。
//
// 超了的话这个数组长度为负，编译不过。
var _ [MagicSize - len(magicText)]struct{}

// Magic 是补零到定长的格式标识，写入与比对都用它。
var Magic = append([]byte(magicText), make([]byte, MagicSize-len(magicText))...)

// FileVersion 是这个实现读写的文件版本。
//
// 打开时严格相等才放行：版本不同意味着布局可能变过，
// 按当前布局去读会得到一堆看似合法的垃圾。
const FileVersion uint8 = 8

const (
	// 新建文件时几个配置项的初值。
	DefaultTimeoutSeconds = 60
	DefaultCheckpoint     = 1000
)

// collectionTable 是集合名到集合页页号的对照表。
type collectionTable map[string]uint32

// HeaderPage 是文件的第 0 页：格式标识、版本、全库配置与集合表。
//
// 集合表解出来放在内存里，改动先落在 map 上，[HeaderPage.Flush]
// 才写回页里——每加一个集合就重编一次整张表太浪费。
type HeaderPage struct {
	*Page

	// collections 是解出来的集合表，collectionsDirty 表示它与页里的字节不一致。
	collections collectionTable

	collectionsDirty bool
}

// NewHeaderPage 初始化一份新文件的头页。
func NewHeaderPage(buf []byte, coll xcoll.Collation, now time.Time) (*HeaderPage, error) {
	p, err := PageHeader.NewPage(buf, 0)
	if err != nil {
		return nil, err
	}
	h := &HeaderPage{Page: p, collections: collectionTable{}}
	copy(buf[offMagic:], Magic)
	buf[offFileVersion] = FileVersion
	h.putU32(offFreeEmptyPageList, EmptyPageID)
	h.putU32(offLastPageID, 0)
	ticks, err := xbin.TimeToTicks(now)
	if err != nil {
		return nil, fmt.Errorf("xpage: creation time out of range: %w", err)
	}
	h.putI64(offCreationTime, ticks)
	h.putI32(offUserVersion, 0)
	h.putI32(offCollationLCID, int32(coll.LCID()))
	h.putI32(offCollationOptions, int32(coll.Options()))
	h.putI32(offTimeout, DefaultTimeoutSeconds)
	buf[offUTCDate] = 0
	h.putI32(offCheckpoint, DefaultCheckpoint)
	h.SetLimitSize(0)
	buf[offInvalidState] = 0

	return h, nil
}

// LoadHeaderPage 读出头页，并确认这确实是一份本实现能读的文件。
//
// 四道检查：页本身的账目、页类型、页号、格式标识、版本号。
// 标识对不上时报的是「不是数据库文件」，而且落在 [ErrCorrupt] 之下——
// 调用方据此知道重试没有意义。
func LoadHeaderPage(buf []byte) (*HeaderPage, error) {
	p, err := Load(buf)
	if err != nil {
		return nil, err
	}
	if p.Type() != PageHeader {
		return nil, fmt.Errorf("%w: page 0 has type %s, want %s", ErrCorrupt, p.Type(), PageHeader)
	}
	if p.ID() != 0 {
		return nil, fmt.Errorf("%w: header page claims page id %d", ErrCorrupt, p.ID())
	}
	if !bytes.Equal(buf[offMagic:offMagic+MagicSize], Magic) {
		return nil, fmt.Errorf("%w: not a database file (bad magic)", ErrCorrupt)
	}
	if v := buf[offFileVersion]; v != FileVersion {
		return nil, fmt.Errorf("%w: file version %d, this build reads %d", ErrCorrupt, v, FileVersion)
	}
	h := &HeaderPage{Page: p}
	if err := h.parseCollections(); err != nil {
		return nil, err
	}
	return h, nil
}

// parseCollections 从页尾那块字节里解出集合表。
//
// 开头四个字节为 0 表示表还没写过（新文件），当空表处理。
func (h *HeaderPage) parseCollections() error {
	b := h.buf[offCollections:]
	out := collectionTable{}
	if binary.LittleEndian.Uint32(b) == 0 {
		h.collections = out
		return nil
	}
	doc, _, err := xbson.DecodePrefix(b)
	if err != nil {
		return fmt.Errorf("%w: collection table: %w", ErrCorrupt, err)
	}
	for name, v := range doc.Elements() {
		id, ok := v.AsInt32()
		if !ok {
			return fmt.Errorf("%w: collection %q maps to %s, want a page id",
				ErrCorrupt, name, v.Type())
		}
		out[name] = uint32(id)
	}
	h.collections = out
	return nil
}

// Collections 按名字排序遍历全部集合。
func (h *HeaderPage) Collections() iter.Seq2[string, uint32] {
	names := slices.Sorted(maps.Keys(h.collections))
	return func(yield func(string, uint32) bool) {
		for _, n := range names {
			if !yield(n, h.collections[n]) {
				return
			}
		}
	}
}

// Collection 按名字查集合页页号，名字不区分大小写。
func (h *HeaderPage) Collection(name string) (uint32, bool) {
	_, id, ok := h.lookup(name)
	return id, ok
}

// lookup 查一个集合，返回它**存进去时的原名**。
//
// 先精确匹配，不中再折叠大小写逐个比。原名要返回出去：报重名错误时
// 说清楚是与哪一个撞了，删除与改名也要按原名去操作 map。
func (h *HeaderPage) lookup(name string) (string, uint32, bool) {
	if id, ok := h.collections[name]; ok {
		return name, id, true
	}
	folded := FoldName(name)
	for k, id := range h.collections {
		if FoldName(k) == folded {
			return k, id, true
		}
	}
	return "", 0, false
}

// FoldName 把集合名里的 ASCII 小写字母折成大写。
//
// 只折 ASCII：非 ASCII 的折叠依赖语言，而集合名比较必须在任何环境下
// 给出同样的结果。
func FoldName(s string) string {
	need := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// CollectionCount 返回集合个数。
func (h *HeaderPage) CollectionCount() int { return len(h.collections) }

// AddCollection 往集合表里加一个集合。
//
// 先试编码一次：编不下就撤回去。集合表的空间是固定的，
// 装不下时要在这里失败，而不是等到写回页时才发现。
func (h *HeaderPage) AddCollection(name string, pageID uint32) error {
	if existing, _, ok := h.lookup(name); ok {
		return fmt.Errorf("xpage: collection %q already exists (as %q)", name, existing)
	}
	h.collections[name] = pageID
	if _, err := h.collections.encode(); err != nil {
		delete(h.collections, name)
		return err
	}
	h.collectionsDirty = true
	h.dirty = true
	return nil
}

// DeleteCollection 从集合表里删掉一个集合。
func (h *HeaderPage) DeleteCollection(name string) bool {
	actual, _, ok := h.lookup(name)
	if !ok {
		return false
	}
	delete(h.collections, actual)
	h.collectionsDirty = true
	h.dirty = true
	return true
}

// RenameCollection 给集合改名。
//
// 新名字与原名折叠后相同时不算重名——那是改大小写。
//
// 同样先试编码：新名字更长时可能装不下，那时要把两边都还原回去。
func (h *HeaderPage) RenameCollection(old, name string) error {
	actual, id, ok := h.lookup(old)
	if !ok {
		return fmt.Errorf("xpage: collection %q not found", old)
	}

	if existing, _, ok := h.lookup(name); ok && existing != actual {
		return fmt.Errorf("xpage: collection %q already exists (as %q)", name, existing)
	}
	delete(h.collections, actual)
	h.collections[name] = id
	if _, err := h.collections.encode(); err != nil {
		delete(h.collections, name)
		h.collections[actual] = id
		return err
	}
	h.collectionsDirty = true
	h.dirty = true
	return nil
}

// encode 把集合表编成文档字节，按名字排序。
//
// 排序是为了让同一张表每次编出同样的字节：不排的话 map 的遍历顺序
// 每次都不同，会让本可以不写的页每次都变脏。
func (ct collectionTable) encode() ([]byte, error) {
	d := xbson.NewDocument()
	for _, name := range slices.Sorted(maps.Keys(ct)) {
		d.Set(name, xbson.Int32(int32(ct[name])))
	}
	b, err := d.Encode()
	if err != nil {
		return nil, err
	}
	if len(b) > collectionsSize {
		return nil, fmt.Errorf("%w: collection table needs %d bytes, only %d available",
			ErrCollectionTableFull, len(b), collectionsSize)
	}
	return b, nil
}

// ErrCollectionTableFull 表示集合表装不下了。
//
// 能装多少取决于名字长短，不是一个固定的集合个数。
var ErrCollectionTableFull = errors.New("xpage: collection table is full")

// CanAddCollection 预判能不能加这个集合，不改任何状态。
//
// 用最大页号当占位：真正的页号还没分配，而页号的大小不影响编码长度。
func (h *HeaderPage) CanAddCollection(name string) error {
	if existing, _, ok := h.lookup(name); ok {
		return fmt.Errorf("xpage: collection %q already exists (as %q)", name, existing)
	}
	next := maps.Clone(h.collections)
	if next == nil {
		next = make(collectionTable, 1)
	}

	next[name] = math.MaxUint32
	_, err := next.encode()
	return err
}

// CanRenameCollection 预判能不能这样改名，不改任何状态。
func (h *HeaderPage) CanRenameCollection(old, name string) error {
	actual, id, ok := h.lookup(old)
	if !ok {
		return fmt.Errorf("xpage: collection %q not found", old)
	}
	if existing, _, ok := h.lookup(name); ok && existing != actual {
		return fmt.Errorf("xpage: collection %q already exists (as %q)", name, existing)
	}
	next := maps.Clone(h.collections)
	delete(next, actual)
	next[name] = id
	_, err := next.encode()
	return err
}

// Flush 把内存里的集合表写回页尾。
//
// 先清空整块再写：新表比旧表短时，残留的旧字节会让下次解析
// 读到多余的内容。
func (h *HeaderPage) Flush() error {
	if !h.collectionsDirty {
		return nil
	}
	b, err := h.collections.encode()
	if err != nil {
		return err
	}
	clear(h.buf[offCollections:])
	copy(h.buf[offCollections:], b)
	h.collectionsDirty = false
	return nil
}

// FreeEmptyPageList 是回收待重用的空页链的链头。
func (h *HeaderPage) FreeEmptyPageList() uint32 { return h.u32(offFreeEmptyPageList) }

// SetFreeEmptyPageList 设置空页链的链头。
func (h *HeaderPage) SetFreeEmptyPageList(v uint32) {
	h.putU32(offFreeEmptyPageList, v)
	h.dirty = true
}

// LastPageID 是文件里已经分配到的最大页号。
func (h *HeaderPage) LastPageID() uint32 { return h.u32(offLastPageID) }

// SetLastPageID 设置已分配到的最大页号。
func (h *HeaderPage) SetLastPageID(v uint32) { h.putU32(offLastPageID, v); h.dirty = true }

// CreationTime 返回文件的创建时刻。
func (h *HeaderPage) CreationTime() (time.Time, error) {
	return xbin.TicksToTime(h.i64(offCreationTime))
}

// UserVersion 是调用方自己维护的版本号，本实现不解释它。
func (h *HeaderPage) UserVersion() int32 { return h.i32(offUserVersion) }

// SetUserVersion 设置调用方的版本号。
func (h *HeaderPage) SetUserVersion(v int32) { h.putI32(offUserVersion, v); h.dirty = true }

// Collation 返回这份文件的字符串排序规则。
//
// 用宽松构造：文件里记着的区域标识可能是这个构建认不出来的，
// 那时也要能把文件打开。
func (h *HeaderPage) Collation() xcoll.Collation {
	return xcoll.NewLenient(int(h.i32(offCollationLCID)), int(h.i32(offCollationOptions)))
}

// Timeout 是等锁的时限，文件里按整秒存。
func (h *HeaderPage) Timeout() time.Duration {
	return time.Duration(h.i32(offTimeout)) * time.Second
}

// SetTimeout 设置等锁时限，不足一秒的部分被截掉。
func (h *HeaderPage) SetTimeout(d time.Duration) {
	h.putI32(offTimeout, int32(d/time.Second))
	h.dirty = true
}

// UTCDate 报告日期该按 UTC 还是按本地时区读出来。
func (h *HeaderPage) UTCDate() bool { return h.buf[offUTCDate] != 0 }

// SetUTCDate 设置日期的读出方式。
func (h *HeaderPage) SetUTCDate(v bool) {
	h.buf[offUTCDate] = 0
	if v {
		h.buf[offUTCDate] = 1
	}
	h.dirty = true
}

// Checkpoint 是日志攒到多少页就自动搬回数据文件，0 表示不自动搬。
func (h *HeaderPage) Checkpoint() int32 { return h.i32(offCheckpoint) }

// SetCheckpoint 设置自动搬运的阈值。
func (h *HeaderPage) SetCheckpoint(v int32) { h.putI32(offCheckpoint, v); h.dirty = true }

// noLimit 是「不限大小」在内存里的表示，文件里存的是 0。
const noLimit int64 = 1<<63 - 1

// LimitSize 是数据文件的大小上限，文件里的 0 表示不限。
func (h *HeaderPage) LimitSize() int64 {
	if v := h.i64(offLimitSize); v != 0 {
		return v
	}
	return noLimit
}

// SetLimitSize 设置大小上限，非正数当作不限。
func (h *HeaderPage) SetLimitSize(v int64) {
	if v <= 0 {
		v = noLimit
	}
	h.putI64(offLimitSize, v)
	h.dirty = true
}

// InvalidState 表示这份文件上一次没有正常收尾。
//
// 打开时看到它就要求先重建：接着用下去会在一份已知不一致的文件上
// 继续写。
func (h *HeaderPage) InvalidState() bool { return h.buf[offInvalidState] != 0 }

// SetInvalidState 设置文件的异常标记。
func (h *HeaderPage) SetInvalidState(v bool) {
	h.buf[offInvalidState] = 0
	if v {
		h.buf[offInvalidState] = 1
	}
	h.dirty = true
}

// Savepoint 拷一份整页，供事务回滚时还原。
//
// 头页的改动（集合表、配置项）不走日志，所以要靠整页快照回退。
func (h *HeaderPage) Savepoint() []byte { return bytes.Clone(h.buf) }

// Restore 从快照还原整个头页，并重新解出集合表。
//
// 内存里的集合表也要跟着回退，否则它会与页里的字节对不上。
func (h *HeaderPage) Restore(sp []byte) error {
	if len(sp) != PageSize {
		return fmt.Errorf("xpage: savepoint is %d bytes, want %d", len(sp), PageSize)
	}
	copy(h.buf, sp)
	if err := h.parseCollections(); err != nil {
		return err
	}
	h.collectionsDirty = false
	h.dirty = true
	return nil
}

// 头页里有几个字段是有符号的，这几个访问器按有符号读写。
func (p *Page) i32(off int) int32       { return int32(p.u32(off)) }
func (p *Page) i64(off int) int64       { return int64(binary.LittleEndian.Uint64(p.buf[off:])) }
func (p *Page) putI32(off int, v int32) { p.putU32(off, uint32(v)) }
func (p *Page) putI64(off int, v int64) { binary.LittleEndian.PutUint64(p.buf[off:], uint64(v)) }
