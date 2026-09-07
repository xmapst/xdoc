package xstore

import (
	"errors"
	"fmt"

	"github.com/xmapst/xdoc/internal/xpage"
)

// ErrDocumentTooLarge 表示文档字节数超过了 [xpage.MaxDocumentSize]。
var ErrDocumentTooLarge = errors.New("xstore: document exceeds the maximum size")

// ErrBlockChain 表示文档的块链不对劲：太长、成环，或者起点根本不是首块。
var ErrBlockChain = errors.New("xstore: broken document block chain")

// InsertDocument 把一篇文档写成一条数据块链，返回首块地址。
//
// 每块尽量装满一页，装不下就再要一页接上去。空文档也会占一个块——
// 首块地址是文档的身份，不能是空地址。
func (s Store) InsertDocument(doc []byte) (xpage.Address, error) {
	if len(doc) > xpage.MaxDocumentSize {
		return xpage.EmptyAddress, fmt.Errorf("%w: %d bytes, limit is %d",
			ErrDocumentTooLarge, len(doc), xpage.MaxDocumentSize)
	}
	var first, last xpage.Address
	var lastBlk xpage.DataBlock
	first = xpage.EmptyAddress

	for off := 0; off < len(doc) || off == 0; {
		n := min(len(doc)-off, xpage.MaxDataBytesPerPage)
		page, err := s.getFreeDataPage(n + xpage.DataBlockHeaderSize)
		if err != nil {
			return xpage.EmptyAddress, err
		}
		blk, idx, err := page.InsertDataBlock(n, off > 0)
		if err != nil {
			return xpage.EmptyAddress, err
		}
		addr := xpage.Address{PageID: page.ID(), Index: idx}
		copy(blk.Data(), doc[off:off+n])
		if err := s.syncDataFreeList(page); err != nil {
			return xpage.EmptyAddress, err
		}
		if first.IsEmpty() {
			first = addr
		} else {
			lastBlk.SetNextBlock(addr)
		}
		lastBlk, last = blk, addr
		off += n
		if off >= len(doc) {
			break
		}
	}
	_ = last
	return first, nil
}

// ReadDocument 沿块链读回整篇文档，追加到 dst 后返回。
//
// dst 会先被清空再复用，传 nil 也可以。首块若标着「续块」说明给错了地址，
// 按链损坏处理；跳的步数超过上界同样报损坏。
func (s Store) ReadDocument(first xpage.Address, dst []byte) ([]byte, error) {
	dst = dst[:0]
	addr := first

	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops > maxBlocksPerDocument {
			return nil, fmt.Errorf("%w: more than %d blocks starting at %s",
				ErrBlockChain, maxBlocksPerDocument, first)
		}
		page, err := s.GetPage(addr.PageID)
		if err != nil {
			return nil, err
		}
		blk, err := page.GetDataBlock(addr.Index)
		if err != nil {
			return nil, err
		}
		if hops == 0 && blk.Extend() {
			return nil, fmt.Errorf("%w: %s is a continuation block, not a document head",
				ErrBlockChain, first)
		}
		dst = append(dst, blk.Data()...)
		addr = blk.NextBlock()
	}
	return dst, nil
}

// maxBlocksPerDocument 是一篇文档最多能有几块。
//
// 加二留出余量：整除时的边界，以及空文档那一块。
const maxBlocksPerDocument = xpage.MaxDocumentSize/xpage.MaxDataBytesPerPage + 2

// UpdateDocument 就地改写一篇文档，首块地址保持不变。
//
// 沿着原来的块链走，能塞多少塞多少——每块可用的空间是它自己的长度加上
// 所在页的剩余空间，所以块可以就地变大。原链走完还有剩余内容就另开新块接上；
// 反过来，内容写完而链还有尾巴，就把尾巴截断并整段删掉。
func (s Store) UpdateDocument(first xpage.Address, doc []byte) error {
	if len(doc) > xpage.MaxDocumentSize {
		return fmt.Errorf("%w: %d bytes, limit is %d",
			ErrDocumentTooLarge, len(doc), xpage.MaxDocumentSize)
	}
	off := 0
	addr := first
	var lastBlk xpage.DataBlock
	var haveLast bool

	for off < len(doc) || off == 0 {
		var blk xpage.DataBlock
		var cur xpage.Address
		if !addr.IsEmpty() {
			page, err := s.GetPage(addr.PageID)
			if err != nil {
				return err
			}
			old, err := page.GetDataBlock(addr.Index)
			if err != nil {
				return err
			}
			next := old.NextBlock()

			room := page.FreeBytes() + len(old.Data())
			n := min(len(doc)-off, room)
			seg, err := page.Update(addr.Index, n+xpage.DataBlockHeaderSize)
			if err != nil {
				return err
			}
			if blk, err = xpage.AsDataBlock(seg); err != nil {
				return err
			}
			blk.SetExtend(off > 0)
			blk.SetNextBlock(next)
			copy(blk.Data(), doc[off:off+n])
			if err := s.syncDataFreeList(page); err != nil {
				return err
			}
			cur, addr, off = addr, next, off+n
		} else {
			n := min(len(doc)-off, xpage.MaxDataBytesPerPage)
			page, err := s.getFreeDataPage(n + xpage.DataBlockHeaderSize)
			if err != nil {
				return err
			}
			nb, idx, err := page.InsertDataBlock(n, true)
			if err != nil {
				return err
			}
			copy(nb.Data(), doc[off:off+n])
			if err := s.syncDataFreeList(page); err != nil {
				return err
			}
			cur = xpage.Address{PageID: page.ID(), Index: idx}
			if haveLast {
				lastBlk.SetNextBlock(cur)
			}
			blk, off = nb, off+n
		}
		lastBlk, haveLast = blk, true
		_ = cur
		if off >= len(doc) {
			break
		}
	}

	if haveLast {
		if tail := lastBlk.NextBlock(); !tail.IsEmpty() {
			lastBlk.SetNextBlock(xpage.EmptyAddress)
			return s.DeleteDocument(tail)
		}
	}
	return nil
}

// DeleteDocument 沿块链把一篇文档的所有块删掉，每删一块就更新该页的空闲链档次。
func (s Store) DeleteDocument(first xpage.Address) error {
	addr := first
	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops > maxBlocksPerDocument {
			return fmt.Errorf("%w: more than %d blocks starting at %s",
				ErrBlockChain, maxBlocksPerDocument, first)
		}
		page, err := s.GetPage(addr.PageID)
		if err != nil {
			return err
		}
		blk, err := page.GetDataBlock(addr.Index)
		if err != nil {
			return err
		}
		next := blk.NextBlock()
		if err := page.Delete(addr.Index); err != nil {
			return err
		}
		if err := s.syncDataFreeList(page); err != nil {
			return err
		}
		addr = next
	}
	return nil
}
