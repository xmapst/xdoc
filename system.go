package xdoc

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xjson"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xtx"
)

// sysCollection 是一个 $ 打头的虚拟集合。
//
// 它不占页、不进集合表，读写都是当场算出来的——查询层把它当普通集合看待，
// 所以 WHERE、ORDER BY、投影这些照常能用在上面。
type sysCollection struct {
	// name 是带 $ 前缀的集合名。
	name string

	// input 产出这个集合的内容；为 nil 表示只写不读。
	input func(db *DB, ctx context.Context, tx *Tx, opts sysOpts) iter.Seq2[*Document, error]

	// output 接收写进这个集合的文档；为 nil 表示只读不写。
	output func(db *DB, ctx context.Context, tx *Tx, opts sysOpts, docs iter.Seq2[*Document, error]) (int, error)
}

// sysOrder 记住注册顺序，让 $cols 与错误提示里的列举有个稳定次序。
var sysOrder []string

// sysRegistry 按小写名索引，查找因此不区分大小写。
var sysRegistry = map[string]*sysCollection{}

// registerSystem 把一个虚拟集合登记进去。
//
// 名字不带 $ 直接 panic：这是初始化期的编码错误，放过去就会得到一个
// 永远查不到、也没人报错的集合。
func (c *sysCollection) registerSystem() {
	if !strings.HasPrefix(c.name, "$") {
		panic("xdoc: system collection name must start with $: " + c.name)
	}
	key := strings.ToLower(c.name)
	if _, ok := sysRegistry[key]; !ok {
		sysOrder = append(sysOrder, c.name)
	}
	sysRegistry[key] = c
}

// SystemCollectionNames 按注册顺序返回全部虚拟集合名。返回的是副本。
func SystemCollectionNames() []string { return append([]string(nil), sysOrder...) }

// isSystemName 判断一个集合名是不是虚拟集合。
func isSystemName(name string) bool { return strings.HasPrefix(name, "$") }

// parseSystemName 把 `$file({...})` 这种带参写法拆成名字与参数。
//
// 括号里是一段 JSON，用严格模式解析：多一个逗号、少一个引号都当错，
// 而不是解出半个文档再让后面某个取值处报一个看不懂的错。
func parseSystemName(s string) (name string, opts *Value, err error) {
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return s, nil, nil
	}
	if !strings.HasSuffix(s, ")") {
		return "", nil, fmt.Errorf("xdoc: system collection %q: missing closing parenthesis", s)
	}
	name = s[:i]
	body := strings.TrimSpace(s[i+1 : len(s)-1])
	if body == "" {
		return name, nil, nil
	}

	v, err := xjson.UnmarshalExact([]byte(body))
	if err != nil {
		return "", nil, fmt.Errorf("xdoc: system collection %q: %w", s, err)
	}
	return name, v, nil
}

// lookupSystem 按规格串找到虚拟集合并解出它的参数。
func lookupSystem(spec string) (*sysCollection, sysOpts, error) {
	name, v, err := parseSystemName(spec)
	if err != nil {
		return nil, sysOpts{}, err
	}
	c, ok := sysRegistry[strings.ToLower(name)]
	if !ok {
		return nil, sysOpts{}, fmt.Errorf("xdoc: %q is not a registered system collection, known ones are %s",
			name, strings.Join(sysOrder, ", "))
	}

	return c, sysOpts{v: v}, nil
}

// sysSource 打开一个虚拟集合作为查询的数据源。
//
// 在事务里时先取一份读快照：虚拟集合的内容多半是从库的当前状态算出来的，
// 不取快照的话，同一个事务里前后两次读会看到不同的东西。
func (db *DB) sysSource(ctx context.Context, spec string, tx *Tx) (iter.Seq2[*Document, error], error) {
	c, opts, err := lookupSystem(spec)
	if err != nil {
		return nil, err
	}
	if c.input == nil {
		return nil, fmt.Errorf("xdoc: system collection %q is write-only", c.name)
	}

	if tx != nil {
		if _, err := tx.tx.Snapshot(ctx, c.name, xtx.ModeRead, false); err != nil {
			return nil, err
		}
	}
	return c.input(db, ctx, tx, opts), nil
}

// sysOpts 是集合名括号里带进来的参数，可能是任意一个值。
type sysOpts struct {
	v *Value
}

// option 从参数文档里取一个具名选项，没有就用 def。
//
// def 非 nil 时它同时兼作类型样板：取到的值类型对不上就报错。
// 参数整个不是文档时，def 为 nil 的选项拿到的是参数本身——
// 这让 `$query("...")` 这种单参写法与 `$file({...})` 走同一套取值。
func (so sysOpts) option(key string, def *Value) (*Value, error) {
	if so.v != nil && so.v.Type() == xbson.TypeDocument {
		d, _ := so.v.AsDocument()

		if !d.Has(key) {
			return def, nil
		}
		v := d.Get(key)
		if def != nil && v.Type() != def.Type() {
			return nil, fmt.Errorf("Parameter `%s` expect %s value type", key, def.Type())
		}
		return v, nil
	}
	if def == nil {
		return so.v, nil
	}
	return def, nil
}

// init 登记全部虚拟集合，顺序决定 $cols 里的排列。
func init() {
	(&sysCollection{name: "$database", input: (*DB).sysDatabase}).registerSystem()
	(&sysCollection{name: "$cols", input: (*DB).sysCols}).registerSystem()
	(&sysCollection{name: "$indexes", input: (*DB).sysIndexes}).registerSystem()
	(&sysCollection{name: "$sequences", input: (*DB).sysSequences}).registerSystem()
	(&sysCollection{name: "$transactions", input: (*DB).sysTransactions}).registerSystem()
	(&sysCollection{name: "$snapshots", input: (*DB).sysSnapshots}).registerSystem()
	(&sysCollection{name: "$open_cursors", input: (*DB).sysOpenCursors}).registerSystem()
	(&sysCollection{name: "$file", input: (*DB).sysFileInput, output: (*DB).sysFileOutput}).registerSystem()
	(&sysCollection{name: "$dump", input: (*DB).sysDump}).registerSystem()
	(&sysCollection{name: "$page_list", input: (*DB).sysPageList}).registerSystem()
	(&sysCollection{name: "$query", input: (*DB).sysQuery}).registerSystem()
}

// sysCols 列出全部集合，用户集合在前、虚拟集合在后。
func (db *DB) sysCols(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	var out []*Document
	for _, n := range db.CollectionNames() {
		out = append(out, Doc("name", n, "type", "user"))
	}
	for _, n := range sysOrder {
		out = append(out, Doc("name", n, "type", "system"))
	}
	return seqOf(out)
}

// sysIndexes 列出全部集合上的索引。
//
// 在事务里时走事务那条路，好让本事务里刚建的索引也看得见。
func (db *DB) sysIndexes(ctx context.Context, tx *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	return func(yield func(*Document, error) bool) {
		for _, coll := range db.CollectionNames() {
			var (
				ixs []xengine.IndexInfo
				err error
			)
			if tx != nil {
				ixs, err = db.engine.IndexesIn(ctx, tx.tx, coll)
			} else {
				ixs, err = db.engine.Indexes(ctx, coll)
			}
			if err != nil {
				yield(nil, err)
				return
			}
			for _, ix := range ixs {
				if !yield(Doc(
					"collection", coll,
					"name", ix.Name,
					"expression", ix.Expression,
					"unique", ix.Unique,
				), nil) {
					return
				}
			}
		}
	}
}

// sysSequences 列出各集合的自增序列当前值，按集合名排序。
func (db *DB) sysSequences(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	seq := db.engine.Sequences()
	names := slices.Sorted(maps.Keys(seq))
	out := make([]*Document, 0, len(names))
	for _, n := range names {
		out = append(out, Doc("collection", n, "value", seq[n]))
	}
	return seqOf(out)
}

// sysDatabase 产出唯一一篇描述本库整体状态的文档。
func (db *DB) sysDatabase(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	d, err := db.databaseInfo()
	if err != nil {
		return seqErr[*Document](err)
	}
	return seqOf([]*Document{d})
}

// databaseInfo 汇总库的整体状态：文件、头页、缓存、事务、配置项。
//
// 内存库的 name 是 :memory:。
//
// 缓存与事务那两节里有几个字段是固定值：这里的页缓存不分段、也不区分可读页与
// 可写页，但字段留着，让按这个格式写的读取方不至于取不到键。
func (db *DB) databaseInfo() (*Document, error) {
	d := xbson.NewDocument()

	dataName, _ := db.core.Disk().Names()
	if dataName == "" {
		dataName = memoryDatabaseName
	}
	d.Set("name", xbson.String(dataName))
	d.Set("encrypted", xbson.Boolean(db.password != ""))
	d.Set("readOnly", xbson.Boolean(db.core.ReadOnly()))

	var (
		lastPage  uint32
		freeEmpty uint32
		created   *Value
	)
	if err := db.core.WithHeader(func(h *xpage.HeaderPage) error {
		lastPage, freeEmpty = h.LastPageID(), h.FreeEmptyPageList()
		t, err := h.CreationTime()
		if err != nil {
			return err
		}
		created, err = xbson.DateTime(t)
		return err
	}); err != nil {
		return nil, err
	}
	d.Set("lastPageID", xbson.Int32(int32(lastPage)))

	d.Set("freeEmptyPageID", xbson.Int32(int32(freeEmpty)))
	d.Set("creationTime", created)

	dataSize, logSize, err := db.core.Disk().Sizes()
	if err != nil {
		return nil, err
	}
	d.Set("dataFileSize", xbson.Int32(int32(dataSize)))
	d.Set("logFileSize", xbson.Int32(int32(logSize)))
	d.Set("currentReadVersion", xbson.Int32(int32(db.core.WAL().ReadVersion())))
	d.Set("lastTransactionID", xbson.Int32(int32(db.core.WAL().LastTransactionID())))
	d.Set("engine", xbson.String(engineName))

	pr := xbson.NewDocument()
	for _, n := range []string{"USER_VERSION", "COLLATION", "TIMEOUT", "LIMIT_SIZE", "UTC_DATE", "CHECKPOINT"} {
		v, err := db.Pragma(n)
		if err != nil {
			return nil, err
		}
		pr.Set(n, v)
	}
	d.Set("pragmas", pr.Value())

	cache := xbson.NewDocument()
	cap, n := db.core.Cache().Capacity(), db.core.Cache().Len()
	cache.Set("extendSegments", xbson.Int32(1))
	cache.Set("extendPages", xbson.Int32(int32(cap)))
	cache.Set("freePages", xbson.Int32(int32(cap-n)))
	cache.Set("readablePages", xbson.Int32(int32(n)))
	cache.Set("writablePages", xbson.Int32(0))
	cache.Set("pagesInUse", xbson.Int32(0))
	d.Set("cache", cache.Value())

	tx := xbson.NewDocument()
	tx.Set("open", xbson.Int32(int32(db.core.OpenTransactions())))
	tx.Set("maxOpenTransactions", xbson.Int32(xtx.MaxOpenTransactions))
	tx.Set("initialTransactionSize", xbson.Int32(xtx.MaxTransactionPages/xtx.MaxOpenTransactions))
	tx.Set("availableSize", xbson.Int32(int32(db.core.Budget())))
	d.Set("transactions", tx.Value())

	return d, nil
}

// memoryDatabaseName 是内存库在 $database 里显示的名字。
const memoryDatabaseName = ":memory:"

// engineName 是 $database 里的引擎标识。
const engineName = "xdoc-v" + Version

// Version 是本库的版本号。
const Version = "1.0.0"

// sysQuery 把一条 SQL 的结果当成集合，这样就能在它外面再套一层查询。
//
// 结果里非文档的值包一层 {"expr": ...}：外层查询要的是文档流。
func (db *DB) sysQuery(ctx context.Context, _ *Tx, opts sysOpts) iter.Seq2[*Document, error] {
	sql, ok := opts.v.AsString()
	if !ok {
		return seqErr[*Document](fmt.Errorf("xdoc: $query(sql) requires a string parameter"))
	}
	return func(yield func(*Document, error) bool) {
		for v, err := range db.Execute(ctx, sql) {
			if err != nil {
				yield(nil, err)
				return
			}
			d, ok := v.AsDocument()
			if !ok {
				d = Doc("expr", v)
			}
			if !yield(d, nil) {
				return
			}
		}
	}
}
