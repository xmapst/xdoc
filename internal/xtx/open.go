// Package xtx 是事务层：谁能看到哪一版页，以及改动怎么落盘。
//
// 写不直接改数据文件，而是把整页追加进日志；提交时在日志索引里把这个事务
// 标成已确认——**确认才是提交点**。攒够了就做一次检查点，把已确认的页搬回
// 数据文件再清空日志。崩溃后重开时重放日志，未确认的那些自然被丢掉。
//
// 隔离靠版本号：[Snapshot] 开出来时记下当时的日志版本，此后别人写的页它看不见。
// 锁分两层，一道全局共享／排他闸门加按集合名的可重入锁，见 [LockService]。
package xtx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xerr"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xwal"
)

// OpenOptions 是打开一份库的选项。
type OpenOptions struct {
	// ReadOnly 只读打开。文件不存在时报错，而不是新建。
	ReadOnly bool

	// SyncOnCommit 决定提交时要不要真的落盘。
	SyncOnCommit bool

	// CollationSet 表示调用方明确指定了排序规则，要与文件里的核对。
	CollationSet bool

	// Collation 是新建库时用的排序规则。
	Collation xcoll.Collation

	// CacheSize 是页缓存能存几页；不为正时用默认值。
	CacheSize int

	// Now 是建库时取当前时间的入口，为空时用 [time.Now]。
	Now func() time.Time

	// Password 非空时按加密文件打开。
	Password string

	// InitialSize 是建库时预先撑到多大，必须是整页的倍数。加密库不支持。
	InitialSize int64

	// IgnoreInvalidState 让打开跳过「上次没干净关闭」的检查，重建流程要用。
	IgnoreInvalidState bool
}

// DefaultOpenOptions 返回可读写、提交即落盘、默认排序规则的选项。
func DefaultOpenOptions() OpenOptions {
	return OpenOptions{SyncOnCommit: true, Collation: xcoll.Default}
}

// ErrNeedsRebuild 表示上次没干净关闭，文件被标成了需要重建。
var ErrNeedsRebuild = errors.New(
	"xtx: database was not closed cleanly; open it read-only to inspect it, or rebuild it before writing")

// Open 在给定的两份存储上打开一个实例。
//
// 数据文件为空就建一份新库：写出头页、落盘、按需预分配。已有文件则读出头页；
// 标着「上次没干净关闭」时，可写打开会被拒绝——只读打开和重建流程可以放行。
//
// 日志非空时先重放一遍：重放中若遇到确认过的头页，就用它顶替盘上那份，
// 并把它上面的事务标记清掉。
func (opt OpenOptions) Open(data, log xdisk.Storage) (*Core, error) {
	d, err := (xdisk.Options{
		ReadOnly:     opt.ReadOnly,
		SyncOnCommit: opt.SyncOnCommit,
	}).New(data, log)
	if err != nil {
		return nil, err
	}

	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	hbuf := make([]byte, xpage.PageSize)
	var header *xpage.HeaderPage

	n, err := d.DataPageCount()
	if err != nil {
		return nil, errors.Join(err, d.Close())
	}
	if n == 0 {
		if opt.ReadOnly {
			return nil, errors.Join(
				errors.New("xtx: cannot create a database in read-only mode"), d.Close())
		}
		if header, err = xpage.NewHeaderPage(hbuf, opt.Collation, now().UTC()); err != nil {
			return nil, errors.Join(err, d.Close())
		}
		if err = d.WriteDataPage(0, hbuf); err != nil {
			return nil, errors.Join(err, d.Close())
		}

		if err = d.SyncData(); err != nil {
			return nil, errors.Join(err, d.Close())
		}

		if err = opt.prealloc(d); err != nil {
			return nil, errors.Join(err, d.Close())
		}
	} else {
		if err = d.ReadDataPage(0, hbuf); err != nil {
			return nil, errors.Join(err, d.Close())
		}
		if header, err = xpage.LoadHeaderPage(hbuf); err != nil {
			return nil, errors.Join(err, d.Close())
		}

		if header.InvalidState() && !opt.IgnoreInvalidState && !opt.ReadOnly {
			return nil, errors.Join(ErrNeedsRebuild, d.Close())
		}
	}

	w := xwal.NewIndex()
	if d.LogPageCount() > 0 {
		onHeader := func(buf []byte) error {
			h, err := xpage.LoadHeaderPage(buf)
			if err != nil {
				return err
			}
			copy(hbuf, buf)
			h2, err := xpage.LoadHeaderPage(hbuf)
			if err != nil {
				return err
			}

			h2.SetTransactionID(xpage.EmptyPageID)
			h2.SetConfirmed(false)
			header = h2
			_ = h
			return nil
		}
		if err = w.Restore(logReader{d}, onHeader); err != nil {
			return nil, errors.Join(err, d.Close())
		}
	}

	coll := header.Collation()

	if opt.CollationSet && coll != opt.Collation {
		return nil, errors.Join(
			xerr.Unspecified.Newf("datafile collation is %s, not the %s given at open; use rebuild to change it", coll, opt.Collation),
			d.Close())
	}
	return NewCore(d, w, header, hbuf, opt.CacheSize, coll), nil
}

// prealloc 按 InitialSize 预先撑大数据文件。加密库不支持，长度也必须是整页的倍数。
func (opt OpenOptions) prealloc(d *xdisk.Disk) error {
	if opt.InitialSize <= 0 {
		return nil
	}
	if opt.Password != "" {
		return xerr.InitialSizeCryptoNotSupport.New("initial size is not supported on an encrypted database")
	}
	if opt.InitialSize%xpage.PageSize != 0 {
		return xerr.InvalidInitialSize.Newf("initial size must be a multiple of the %d byte page size, got %d", xpage.PageSize, opt.InitialSize)
	}
	return d.Prealloc(opt.InitialSize)
}

// OpenFile 按路径打开一份库，连同它旁边的日志文件。
//
// 没给口令却发现文件是加密的，直接报错而不是解出一堆乱码。可写打开会先在
// 进程内占住这个路径，同一个文件不能被打开两次。只读打开时日志文件不存在
// 不算错，拿一份空存储顶上。
func (opt OpenOptions) OpenFile(path string) (*Core, error) {
	if opt.Password == "" {
		if enc, err := looksEncrypted(path); err != nil {
			return nil, err
		} else if enc {
			return nil, fmt.Errorf("%w: %q", ErrPasswordRequired, path)
		}
	}

	var pathKey string
	if !opt.ReadOnly {
		var kerr error
		if pathKey, kerr = openPaths.claim(path); kerr != nil {
			return nil, kerr
		}
	}
	release := func() {
		if pathKey != "" {
			openPaths.release(pathKey)
		}
	}

	open := func(p string) (xdisk.Storage, error) {
		if opt.Password != "" {
			return xdisk.OpenEncryptedFile(p, opt.Password, opt.ReadOnly)
		}
		return xdisk.OpenFile(p, opt.ReadOnly)
	}
	data, err := open(path)
	if err != nil {
		release()
		return nil, err
	}
	log, err := open(xdisk.LogPath(path))
	if err != nil && opt.ReadOnly && errors.Is(err, fs.ErrNotExist) {
		log, err = xdisk.EmptyStorage{}, nil
	}
	if err != nil {
		release()
		return nil, errors.Join(err, data.Close())
	}
	core, err := opt.Open(data, log)
	if err != nil {
		release()
		return nil, err
	}
	core.pathKey = pathKey
	core.readOnly = opt.ReadOnly
	return core, nil
}

// pathClaims 记着本进程里已经可写打开了哪些文件。
type pathClaims struct {
	open sync.Map
}

// openPaths 是全进程共用的那一份。
var openPaths pathClaims

// claim 占住一个路径，返回用作键的规范路径。
//
// 先取绝对路径，能解软链接就解——同一个文件的两条不同路径必须认成一个。
// 已经被占住时报错。
func (c *pathClaims) claim(path string) (string, error) {
	key, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("xtx: resolve %q: %w", path, err)
	}

	if resolved, rerr := filepath.EvalSymlinks(key); rerr == nil {
		key = resolved
	}
	if _, loaded := c.open.LoadOrStore(key, struct{}{}); loaded {
		return "", fmt.Errorf("%w: %q", ErrAlreadyOpen, path)
	}
	return key, nil
}

// release 放开一个路径。
func (c *pathClaims) release(key string) { c.open.Delete(key) }

// ErrAlreadyOpen 表示同一个文件在本进程里已经可写打开了。
var ErrAlreadyOpen = errors.New("xtx: database is already open in this process")

// ErrPasswordRequired 表示文件是加密的但没给口令。
var ErrPasswordRequired = errors.New("xtx: database is encrypted; a password is required")

// looksEncrypted 看首字节判断文件是不是加密的。
//
// 加密文件的第 0 页是盐页，首字节是加密标志；普通库的第 0 页是头页，
// 那个位置放的是别的东西。文件不存在或读不出内容时一律当成没加密。
func looksEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("xtx: inspect %q: %w", path, err)
	}
	defer f.Close()
	var b [1]byte
	n, err := f.Read(b[:])
	if n == 0 || err != nil {
		return false, nil
	}
	return b[0] == 1, nil
}

// OpenMemory 在内存里开一份库，进程结束就没了。
func (opt OpenOptions) OpenMemory() (*Core, error) {
	return opt.Open(xdisk.NewMemStorage(), xdisk.NewMemStorage())
}

// Close 关闭实例。重复调用只有第一次起作用。
//
// 关之前顺手做一次检查点，拿不到排他闸门就算了。检查点失败时把文件标成
// 需要重建——那说明日志没能全部搬回数据文件。
func (c *Core) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	var cerr error

	if !c.readOnly && c.CheckpointPages() > 0 {
		if _, _, err := c.TryCheckpoint(); err != nil {
			cerr = fmt.Errorf("xtx: checkpoint on close: %w", err)
		}
	}
	c.MarkNeedsRebuild(cerr)
	c.writeInvalidState()
	return errors.Join(cerr, c.disk.Close(), c.releasePath())
}

// writeInvalidState 把「需要重建」的标记写进盘上的头页。
//
// 直接读改写第 0 页，不走内存里那份头页——那份可能已经不可信了。
// 中途任何一步失败都静静放弃：这本来就是尽力而为的一笔。
func (c *Core) writeInvalidState() {
	if c.readOnly || !c.needRebuild.Load() {
		return
	}
	buf := make([]byte, xpage.PageSize)
	if err := c.disk.ReadDataPage(0, buf); err != nil {
		return
	}
	h, err := xpage.LoadHeaderPage(buf)
	if err != nil {
		return
	}
	h.SetInvalidState(true)
	_ = c.disk.WriteDataPage(0, buf)
	_ = c.disk.SyncData()
}

// releasePath 放开占住的路径。
func (c *Core) releasePath() error {
	if c.pathKey != "" {
		openPaths.release(c.pathKey)
		c.pathKey = ""
	}
	return nil
}

// Header 返回内存里那份头页。调用方要自己拿 [Core.WithHeader] 的锁。
func (c *Core) Header() *xpage.HeaderPage { return c.header }

// logReader 把磁盘门面收窄成日志重放需要的那两个方法。
type logReader struct{ d *xdisk.Disk }

// ReadLogPage 读一页日志。
func (r logReader) ReadLogPage(off int64, buf []byte) error { return r.d.ReadLogPage(off, buf) }

// LogPageCount 返回日志有几页。
func (r logReader) LogPageCount() int64 { return r.d.LogPageCount() }

// CloseAbrupt 直接关掉文件，不做检查点也不写任何标记。
//
// 给「模拟进程崩溃」这类场景用：留在日志里的东西下次打开时靠重放捞回来。
func (c *Core) CloseAbrupt() error {
	if c.closed.Swap(true) {
		return nil
	}
	return errors.Join(c.disk.Close(), c.releasePath())
}
