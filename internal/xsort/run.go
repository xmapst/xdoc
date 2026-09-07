package xsort

import (
	"fmt"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
)

// run 是排好序的一段。
//
// data 非空表示它还在内存里（只有一段时不必落盘），否则落在临时文件的
// pos 位置上。
type run struct {
	count int
	size  int
	data  []byte
	pos   int64
}

// runReader 顺序读出一段里的记录。
//
// 落盘的段按页读进一个滑动窗口，不整段读进内存。
type runReader struct {
	// win 是滑动窗口，off 是窗口里的读取位置，read 是已从盘上读了多少，left 是还剩几条。
	d    *Disk
	r    *run
	win  []byte
	off  int
	read int
	left int

	// key 与 addr 是当前这一条记录。
	key  *xbson.Value
	addr xpage.Address
}

// newRunReader 给一段建读取器。
//
// 窗口留出一页加一条最大记录的空间：这样窗口里总能凑齐完整的一条，
// 不必处理跨窗口的半条记录。
func (d *Disk) newRunReader(r *run) *runReader {
	rr := &runReader{d: d, r: r, left: r.count}
	if r.data != nil {
		rr.win, rr.read = r.data, r.size
		return rr
	}

	rr.win = make([]byte, 0, xpage.PageSize+maxRecord)
	return rr
}

// fill 把窗口补到至少放得下一条完整记录。
//
// 补之前先把已读部分丢掉、把剩下的挪到窗口开头。段还在内存里时什么都不做。
func (rr *runReader) fill() error {
	if rr.r.data != nil {
		return nil
	}
	for len(rr.win)-rr.off < maxRecord && rr.read < rr.r.size {
		if rr.off > 0 {
			rr.win = rr.win[:copy(rr.win, rr.win[rr.off:])]
			rr.off = 0
		}
		n := min(xpage.PageSize, rr.r.size-rr.read)
		base := len(rr.win)
		rr.win = rr.win[:base+n]
		if err := rr.d.read(rr.win[base:], rr.r.pos+int64(rr.read)); err != nil {
			return err
		}
		rr.read += n
	}
	return nil
}

// next 读出下一条记录，读完返回 false。
func (rr *runReader) next() (bool, error) {
	if rr.left == 0 {
		return false, nil
	}
	if err := rr.fill(); err != nil {
		return false, err
	}
	key, n, err := xbson.ReadIndexKey(rr.win[rr.off:])
	if err != nil {
		return false, fmt.Errorf("xsort: decode sort key: %w", err)
	}
	rr.off += n
	if len(rr.win)-rr.off < xpage.AddressSize {
		return false, fmt.Errorf("xsort: run record truncated: need %d bytes for address, have %d",
			xpage.AddressSize, len(rr.win)-rr.off)
	}
	rr.key = key
	rr.addr = xpage.ReadAddress(rr.win[rr.off:])
	rr.off += xpage.AddressSize
	rr.left--
	return true, nil
}
