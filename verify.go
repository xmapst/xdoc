package xdoc

import (
	"context"
	"fmt"
	"strings"

	"github.com/xmapst/xdoc/internal/xtx"
)

// 下面这组常量是 [VerifyIssue.Kind] 的取值，一个对应一类问题。
const (
	// VerifyKindPage 表示页读不出来、页号与位置对不上、页类型不认识，或者空页里还记着项。
	VerifyKindPage = "page"

	// VerifyKindFreeList 表示空闲链（空页链、数据页分档链、索引页链）成环、指到了分配范围之外，
	// 或者链上挂着类型、所属集合、档次不对的页。
	VerifyKindFreeList = "free-list"

	// VerifyKindPageOwner 表示同一页同时被两个属主占着，比如既在空页链上又装着数据。
	VerifyKindPageOwner = "page-owner"

	// VerifyKindCollection 表示集合表登记的集合页读不出来，或者索引表里有认不出的索引种类。
	VerifyKindCollection = "collection"

	// VerifyKindDocument 表示文档的块链断了、串到了别处，或者拼出来的字节解不开。
	VerifyKindDocument = "document"

	// VerifyKindIndexLink 表示跳表或向量图的节点读不出来、链断在半路或成环、前后指针对不上，
	// 或者高层链跳到了第 0 层上没有、或排在前面的节点。
	VerifyKindIndexLink = "index-link"

	// VerifyKindIndexOrder 表示相邻两个键不按库的排序规则递增，或者唯一索引里有相等的键。
	//
	// 这一类不妨碍读出任何一篇文档，却会让按索引的查找从错误的分支下探，**静默漏记录**。
	VerifyKindIndexOrder = "index-order"

	// VerifyKindIndexCount 表示索引的节点数与文档应有的键数不等。
	//
	// 主键索引与单键索引每篇文档一个节点；多键索引按写入时同一套求键与判重规则逐篇算，
	// 对不上时还逐条指出缺了哪篇文档的哪个键、多了哪个节点。
	VerifyKindIndexCount = "index-count"

	// VerifyKindDangling 表示索引节点（跳表或向量）指向的位置上没有文档。
	VerifyKindDangling = "dangling"

	// VerifyKindOrphanPage 表示数据页属于一个集合表里没有登记的集合。
	VerifyKindOrphanPage = "orphan-page"
)

// VerifyReport 是一次只读完整性校验的结果，见 [Verify]。
type VerifyReport struct {
	// Issues 是查出来的问题，按发现的先后排列。
	Issues []VerifyIssue

	// Pages 是已分配的页数（含头页），Collections 是集合表里登记的集合数，
	// Documents 是各集合数据页上找到的文档篇数，解不开的也算在内。
	Pages, Collections, Documents int

	// Omitted 是超出上限、没有逐条列进 Issues 的问题数。
	//
	// 同一集合、同一索引、同一类问题最多列 100 条：排序规则对不上的库几乎每一对
	// 相邻键都会报一次，全列出来只是更长。
	Omitted int
}

// OK 报告这次校验是不是一个问题都没查出来。
func (r *VerifyReport) OK() bool { return r != nil && len(r.Issues) == 0 && r.Omitted == 0 }

// VerifyIssue 是校验查出的一处问题。
type VerifyIssue struct {
	// PageID 是问题所在的页；不落在某一页上的（比如索引节点数与文档数对不上）记集合页。
	PageID uint32

	// Collection、Index 说明问题属于哪个集合、哪条索引，与之无关时为空。
	Collection, Index string

	// Kind 是问题的类别，取 VerifyKind 打头的那组常量之一。
	Kind string

	// Message 是给人看的说明。
	Message string
}

// String 给出一行可读的说明，形如 `page 12 users.name [index-order] …`。
func (i VerifyIssue) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "page %d", i.PageID)
	if i.Collection != "" {
		b.WriteString(" " + i.Collection)
		if i.Index != "" {
			b.WriteString("." + i.Index)
		}
	}
	fmt.Fprintf(&b, " [%s] %s", i.Kind, i.Message)
	return b.String()
}

// Verify 以只读方式打开 path，核对它的内部结构，**不写文件的任何一个字节**。
//
// 它回答的是「要不要重建」：报告里有问题，就说明有读不出来的内容，或者按索引查找
// 会漏记录，该用 [Rebuild] 了。文件坏了照样返回 nil error，问题列在报告里；
// error 只表示校验本身没能做下去——打不开、口令不对、不是库文件。
//
// 选项与 [Open] 相同；[ReadOnly] 总是生效，[WithAutoRebuild] 与共享连接被忽略。
func Verify(path string, opts ...Option) (*VerifyReport, error) {
	o := newOptions(opts)
	if o.collErr != nil {
		return nil, o.collErr
	}
	o.readOnly, o.autoRebuild, o.conn = true, false, ConnectionDirect
	core, err := o.openOptions().OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("xdoc: verify %q: %w", path, err)
	}
	db := o.newDB(core)
	db.dataPath = path
	rep, err := db.Verify(context.Background())
	if cerr := db.Close(); err == nil && cerr != nil {
		return nil, fmt.Errorf("xdoc: verify %q: close: %w", path, cerr)
	}
	return rep, err
}

// Verify 核对这份打开着的库，检查项与包级的 [Verify] 相同，只读、不改任何东西。
//
// 看的是调用那一刻已经提交的状态：校验期间别的写入不算进来，也不会被误报成损坏。
// 它全程占着一个读事务，检查点要等它做完。ctx 取消时中途停下，返回取消错误。
func (db *DB) Verify(ctx context.Context) (*VerifyReport, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return nil, fmt.Errorf("xdoc: verify: %w", err)
	}
	defer rel()
	tx, err := db.core.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("xdoc: verify: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	s, err := tx.Snapshot(ctx, verifyScanName, xtx.ModeRead, false)
	if err != nil {
		return nil, fmt.Errorf("xdoc: verify: %w", err)
	}
	rep, err := newVerifier(ctx, db.engine, s).run()
	if err != nil {
		return nil, fmt.Errorf("xdoc: verify: %w", err)
	}
	return rep, nil
}
