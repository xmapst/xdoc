// Package xdisk 管数据文件与日志文件这一对，对上只提供按页读写。
//
// [Storage] 抽掉了「文件」这件事：真文件、内存、加密文件都能塞进来。
// [Disk] 在其上加了页对齐、页号自检和日志追加：数据文件按页号随机写，
// 日志文件只往后追加，[Disk.ClearLog] 在数据落盘之后把日志截掉。
//
// 页的内容与格式由 xpage 定义，这里只认页号和 [xpage.PageSize]。
package xdisk

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/xmapst/xdoc/internal/xpage"
)

// ErrMisplacedPage 表示页里自称的页号与它所在的偏移对不上。
//
// 读到它说明文件错位或损坏；写到它说明调用方把页放错了槽，属于程序错误。
var ErrMisplacedPage = errors.New("xdisk: page id does not match its offset")

// Disk 是数据文件加日志文件这一对的按页读写门面。
//
// 数据文件按页号定位，日志文件只往后追加。写入用互斥锁串起来，读取不加锁——
// 底层的 ReadAt 本身就是无状态的。
type Disk struct {
	// logRun 是批量追加日志时拼接多页用的缓冲，只增不减。
	logRun []byte
	data   file
	log    file

	// mu 保护 logEnd 以及所有写入，让并发提交不会交错落到同一段偏移。
	mu sync.Mutex

	// logEnd 是日志文件下一次追加的偏移，打开时按已有长度算好。
	logEnd int64

	syncOnCommit bool

	readOnly bool
}

// Options 是打开 [Disk] 的选项。
type Options struct {
	// ReadOnly 为真时一切写入、截断、清日志都报错。
	ReadOnly bool

	// SyncOnCommit 决定 [Disk.CommitLog] 是否真的落盘。
	//
	// 关掉能显著加快提交，代价是断电后已提交的事务可能丢失。
	SyncOnCommit bool
}

// DefaultOptions 返回可读写、提交即落盘的选项。
func DefaultOptions() Options { return Options{SyncOnCommit: true} }

// New 用给定的数据与日志存储造一个 [Disk]。
//
// 两份文件都先按页对齐：可写时把尾部不足一页的残留截掉，只读时当它不存在。
func (opt Options) New(data, log Storage) (*Disk, error) {
	d := &Disk{data: file{data}, log: file{log}, syncOnCommit: opt.SyncOnCommit, readOnly: opt.ReadOnly}
	if _, err := d.alignedSize(data, "data"); err != nil {
		return nil, err
	}
	n, err := d.alignedSize(log, "log")
	if err != nil {
		return nil, err
	}
	d.logEnd = n
	return d, nil
}

// alignedSize 返回向下取整到整页的长度；可写时顺手把残留截掉并落盘。
func (d *Disk) alignedSize(s Storage, what string) (int64, error) {
	n, err := s.Size()
	if err != nil {
		return 0, err
	}
	rem := n % xpage.PageSize
	if rem == 0 {
		return n, nil
	}
	aligned := n - rem
	if d.readOnly {
		return aligned, nil
	}
	if err := s.Truncate(aligned); err != nil {
		return 0, fmt.Errorf("xdisk: trim %d partial bytes from %s file: %w", rem, what, err)
	}
	if err := s.Sync(); err != nil {
		return 0, err
	}
	return aligned, nil
}

// DataPageCount 返回数据文件当前有几页。
func (d *Disk) DataPageCount() (uint32, error) {
	n, err := d.data.Size()
	if err != nil {
		return 0, err
	}
	return uint32(n / xpage.PageSize), nil
}

// LogPageCount 返回日志文件当前有几页。
func (d *Disk) LogPageCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logEnd / xpage.PageSize
}

// ReadDataPage 读一页数据页到 buf，buf 必须正好一页大。
//
// 读完核对页内自带的页号：对不上返回 [ErrMisplacedPage]，而不是把错位的
// 内容交给上层。
func (d *Disk) ReadDataPage(id uint32, buf []byte) error {
	if len(buf) != xpage.PageSize {
		return fmt.Errorf("xdisk: buffer is %d bytes, want %d", len(buf), xpage.PageSize)
	}
	if err := d.data.readFull(buf, int64(id)*xpage.PageSize); err != nil {
		return fmt.Errorf("xdisk: read data page %d: %w", id, err)
	}

	if got := xpage.PeekPageID(buf); got != id {
		return fmt.Errorf("%w: offset says %d, page says %d", ErrMisplacedPage, id, got)
	}
	return nil
}

// ReadLogPage 按字节偏移读一页日志。日志页不核对页号——同一个页号在日志里可以出现多次。
func (d *Disk) ReadLogPage(off int64, buf []byte) error {
	if len(buf) != xpage.PageSize {
		return fmt.Errorf("xdisk: buffer is %d bytes, want %d", len(buf), xpage.PageSize)
	}
	if off%xpage.PageSize != 0 {
		return fmt.Errorf("xdisk: log offset %d is not page aligned", off)
	}
	if err := d.log.readFull(buf, off); err != nil {
		return fmt.Errorf("xdisk: read log page at %d: %w", off, err)
	}
	return nil
}

// ReadLogRun 一次读连续的若干页日志，buf 的大小决定读几页。
func (d *Disk) ReadLogRun(off int64, buf []byte) error {
	if len(buf) == 0 || len(buf)%xpage.PageSize != 0 {
		return fmt.Errorf("xdisk: buffer is %d bytes, want a multiple of %d", len(buf), xpage.PageSize)
	}
	if off%xpage.PageSize != 0 {
		return fmt.Errorf("xdisk: log offset %d is not page aligned", off)
	}
	if err := d.log.readFull(buf, off); err != nil {
		return fmt.Errorf("xdisk: read log run at %d: %w", off, err)
	}
	return nil
}

// DataPageWrite 是一次批量写里的一页：写到哪个槽，写什么内容。
type DataPageWrite struct {
	ID  uint32
	Buf []byte
}

// WriteDataPages 批量写数据页。
//
// 先把整批都校验一遍再动手：任何一页不合格都不会有部分写入落盘。
// 校验通过后逐页写，中途出错则前面几页已经写下去了。
func (d *Disk) WriteDataPages(pages []DataPageWrite) error {
	if len(pages) == 0 {
		return nil
	}
	for _, w := range pages {
		if err := d.checkWritable(len(w.Buf)); err != nil {
			return err
		}
		if got := xpage.PeekPageID(w.Buf); got != w.ID {
			return fmt.Errorf("%w: writing page %d into slot %d", ErrMisplacedPage, got, w.ID)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, w := range pages {
		if _, err := d.data.WriteAt(w.Buf, int64(w.ID)*xpage.PageSize); err != nil {
			return fmt.Errorf("xdisk: write data page %d: %w", w.ID, err)
		}
	}
	return nil
}

// WriteDataPage 写一页数据页，页内自带的页号必须与 id 一致。
func (d *Disk) WriteDataPage(id uint32, buf []byte) error {
	if err := d.checkWritable(len(buf)); err != nil {
		return err
	}
	if got := xpage.PeekPageID(buf); got != id {
		return fmt.Errorf("%w: writing page %d into slot %d", ErrMisplacedPage, got, id)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.data.WriteAt(buf, int64(id)*xpage.PageSize); err != nil {
		return fmt.Errorf("xdisk: write data page %d: %w", id, err)
	}
	return nil
}

// AppendLogPages 把若干页追加到日志末尾，返回它们各自的字节偏移。
//
// 多于一页时先拼进一块连续缓冲再一次写下去，少一轮系统调用。
// 返回的偏移在写之前就算好了，因为追加位置由 logEnd 独占决定。
func (d *Disk) AppendLogPages(bufs ...[]byte) ([]int64, error) {
	for _, b := range bufs {
		if err := d.checkWritable(len(b)); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	start := d.logEnd
	offs := make([]int64, 0, len(bufs))
	for i := range bufs {
		offs = append(offs, start+int64(i)*xpage.PageSize)
	}

	if len(bufs) == 1 {
		if _, err := d.log.WriteAt(bufs[0], start); err != nil {
			return nil, fmt.Errorf("xdisk: append log page at %d: %w", start, err)
		}
	} else {
		need := len(bufs) * xpage.PageSize
		if cap(d.logRun) < need {
			d.logRun = make([]byte, need)
		}
		run := d.logRun[:need]
		for i, b := range bufs {
			copy(run[i*xpage.PageSize:], b)
		}
		if _, err := d.log.WriteAt(run, start); err != nil {
			return nil, fmt.Errorf("xdisk: append log pages at %d: %w", start, err)
		}
	}
	d.logEnd = start + int64(len(bufs))*xpage.PageSize
	return offs, nil
}

// CommitLog 把日志落盘，此后这些页才算真的提交。关掉 SyncOnCommit 时直接返回。
func (d *Disk) CommitLog() error {
	if d.readOnly {
		return errDBReadOnly
	}
	if !d.syncOnCommit {
		return nil
	}
	return d.log.Sync()
}

// ClearLog 清空日志，一般在日志内容已经全部落到数据文件之后调用。
//
// 顺序不能反：先把数据文件刷到设备，再截断日志。反过来的话，两步之间断电
// 就既没有数据也没有日志了。
func (d *Disk) ClearLog() error {
	if d.readOnly {
		return errDBReadOnly
	}
	if err := d.data.Sync(); err != nil {
		return fmt.Errorf("xdisk: sync data file before clearing log: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.log.Truncate(0); err != nil {
		return fmt.Errorf("xdisk: clear log: %w", err)
	}
	if err := d.log.Sync(); err != nil {
		return err
	}
	d.logEnd = 0
	return nil
}

// Prealloc 把数据文件预先撑到 n 字节；已经够大就什么也不做。
func (d *Disk) Prealloc(n int64) error {
	if d.readOnly {
		return errDBReadOnly
	}
	cur, err := d.data.Size()
	if err != nil {
		return err
	}
	if n <= cur {
		return nil
	}
	return d.data.Truncate(n)
}

// SyncData 把数据文件刷到设备；只读时什么也不做。
func (d *Disk) SyncData() error {
	if d.readOnly {
		return nil
	}
	return d.data.Sync()
}

// Close 关闭两份文件；日志为空时顺手把日志文件删掉。
//
// 先关再删，两个关闭的错误都收集起来一并返回。删除失败不算错——
// 留下一个空日志文件无非是下次打开时多一个空文件。
func (d *Disk) Close() error {
	dropLog := false
	if !d.readOnly {
		if n, err := d.log.Size(); err == nil && n == 0 {
			dropLog = true
		}
	}
	e1 := d.data.Close()
	e2 := d.log.Close()
	if dropLog {
		if name := d.log.name(); name != "" {
			_ = os.Remove(name)
		}
	}
	return errors.Join(e1, e2)
}

// errDBReadOnly 是只读库上所有写操作的统一错误。
var errDBReadOnly = errors.New("xdisk: database is open read-only")

// checkWritable 校验库可写且这一段正好是一页。
func (d *Disk) checkWritable(n int) error {
	if d.readOnly {
		return errDBReadOnly
	}
	if n != xpage.PageSize {
		return fmt.Errorf("xdisk: buffer is %d bytes, want %d", n, xpage.PageSize)
	}
	return nil
}

// Sizes 返回数据与日志两份文件的字节数。
func (d *Disk) Sizes() (data, log int64, err error) {
	if data, err = d.data.Size(); err != nil {
		return 0, 0, err
	}
	if log, err = d.log.Size(); err != nil {
		return 0, 0, err
	}
	return data, log, nil
}

// Names 返回数据与日志两份文件的路径；没有路径的存储返回空串。
func (d *Disk) Names() (data, log string) {
	return d.data.name(), d.log.name()
}
