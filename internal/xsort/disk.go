// Package xsort 是外部排序：内存放不下的结果集分段落盘，再多路归并出来。
//
// 输入切成若干段，每段在内存里排好序、编成字节；只有一段时就留在内存里，
// 多于一段才落到数据文件旁边的临时文件。取结果时每段各开一个读取器，
// 用一个小顶堆挑最小的那条——内存占用与总记录数无关。
//
// 库带口令时临时文件也加密：排序过程中文档内容会落到那里。
package xsort

import (
	"fmt"
	"os"
	"sync"

	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xpage"
)

// RunSize 是默认的每段大小，也就是排序时最多占多少内存。
//
// 排序把输入切成若干段，每段在内存里排好再落盘，最后归并。
// 段越大占内存越多、要归并的段越少。
const RunSize = 100 * xpage.PageSize

// Disk 管理外部排序用的临时空间：按段分配、回收、读写。
//
// 段大小固定，所以回收之后可以直接重用，不必考虑碎片。
type Disk struct {
	// st 是底层存储，path 非空时表示它是本包建的临时文件，关闭时要删掉。
	st      xdisk.Storage
	path    string
	runSize int

	// next 是下一段的分配位置，free 是回收待重用的段。
	mu   sync.Mutex
	next int64
	free []int64
}

// NewDisk 在一个已有的存储上建临时空间，段大小必须是页大小的正整数倍。
func NewDisk(st xdisk.Storage, runSize int) (*Disk, error) {
	if runSize <= 0 || runSize%xpage.PageSize != 0 {
		return nil, fmt.Errorf("xsort: run size %d must be a positive multiple of %d", runSize, xpage.PageSize)
	}
	return &Disk{st: st, runSize: runSize}, nil
}

// OpenTempDisk 在数据文件旁边开一个临时文件当排序空间。
//
// 库带口令时临时文件也加密：排序过程中文档内容会落到这里，
// 不加密等于把数据明文摊在磁盘上。
//
// 打开之后先截空：上一次运行留下的同名文件里可能还有内容。
func OpenTempDisk(dataPath, password string, runSize int) (*Disk, error) {
	path := xdisk.TempPath(dataPath)

	plain, err := xdisk.OpenFile(path, false)
	if err != nil {
		return nil, err
	}
	var f xdisk.Storage = plain
	if password != "" {
		if f, err = newTempCipher(plain, password); err != nil {
			_ = plain.Close()
			return nil, err
		}
	}
	d, err := NewDisk(f, runSize)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}

	if err = f.Truncate(0); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("xsort: reset temp file: %w", err)
	}
	d.path = path
	return d, nil
}

// RunSize 返回每段的大小。
func (d *Disk) RunSize() int { return d.runSize }

// alloc 分配一段，优先重用回收的。
func (d *Disk) alloc() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.free); n > 0 {
		pos := d.free[n-1]
		d.free = d.free[:n-1]
		return pos
	}
	pos := d.next
	d.next += int64(d.runSize)
	return pos
}

// release 回收一段。
func (d *Disk) release(pos int64) {
	d.mu.Lock()
	d.free = append(d.free, pos)
	d.mu.Unlock()
}

// write 写一段。
func (d *Disk) write(pos int64, p []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.st.WriteAt(p, pos); err != nil {
		return fmt.Errorf("xsort: write run at %d: %w", pos, err)
	}
	return nil
}

// read 读满 p，读不满就报错。
//
// 短读在这里判成错误：段的内容是定长记录，读少了后面的解析
// 会把半条记录当成完整的。
func (d *Disk) read(p []byte, off int64) error {
	n, err := d.st.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("xsort: short read: got %d of %d bytes at %d", n, len(p), off)
	}
	return fmt.Errorf("xsort: read run at %d: %w", off, err)
}

// Close 关掉存储，本包建的临时文件一并删掉。
func (d *Disk) Close() error {
	err := d.st.Close()
	if d.path != "" {
		if rmErr := os.Remove(d.path); rmErr != nil && err == nil && !os.IsNotExist(rmErr) {
			err = fmt.Errorf("xsort: remove temp file: %w", rmErr)
		}
	}
	return err
}
