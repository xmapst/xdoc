package xquery

import (
	"fmt"
	"iter"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
)

// row 是一条待处理的结果：文档本身，加上它的地址。
type row struct {
	doc  *xbson.Document
	addr xpage.Address
}

// lookup 负责把索引命中变成一篇文档。
type lookup interface {
	load(h hit) (row, error)
	loadAddr(a xpage.Address) (*xbson.Document, error)
}

// docLookup 顺着数据块地址读出整篇文档，复用一块读缓冲。
type docLookup struct {
	p xstore.Pages

	loc *time.Location
	buf []byte
}

// load 读出命中项指向的文档。索引节点没有数据块时报损坏。
func (l *docLookup) load(h hit) (row, error) {
	if h.dataBlock.IsEmpty() {
		return row{}, fmt.Errorf("%w: index node %s has no data block", xpage.ErrCorrupt, h.addr)
	}
	doc, err := l.loadAddr(h.dataBlock)
	if err != nil {
		return row{}, err
	}
	return row{doc: doc, addr: h.dataBlock}, nil
}

// loadAddr 按数据块地址读出一篇文档。
func (l *docLookup) loadAddr(a xpage.Address) (*xbson.Document, error) {
	buf, err := xstore.New(l.p).ReadDocument(a, l.buf)
	if err != nil {
		return nil, err
	}

	l.buf = buf
	return xbson.DecodeIn(buf, l.loc)
}

// keyLookup 只用索引键拼一篇单字段文档，不去读数据块。
//
// 查询只要索引键本身时走这条路，能省掉整篇文档的读取与解码。
type keyLookup struct {
	p     xstore.Pages
	field string

	loc *time.Location
}

// load 把命中项的索引键包成一篇单字段文档，时间按库设的时区落定。
func (l *keyLookup) load(h hit) (row, error) {
	d := xbson.NewDocument()

	k := h.key
	d.Set(l.field, k.In(l.loc))
	return row{doc: d, addr: h.addr}, nil
}

// loadAddr 按索引节点地址取出键并包成文档。
func (l *keyLookup) loadAddr(a xpage.Address) (*xbson.Document, error) {
	n, err := xstore.New(l.p).NodeAt(a)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fmt.Errorf("%w: index node %s is gone", xpage.ErrCorrupt, a)
	}
	k, err := n.Key()
	if err != nil {
		return nil, err
	}
	d := xbson.NewDocument()
	d.Set(l.field, k.In(l.loc))
	return d, nil
}

// Source 是一串现成的文档，虚拟集合就是这么喂进来的。
type Source = iter.Seq2[*xbson.Document, error]

// virtualSource 包住一个现成的文档序列。
//
// retain 为真时把流过的文档留一份，好让后面按地址回查——排序之后
// 拿到的是地址而不是文档本身。
type virtualSource struct {
	seq    Source
	retain bool
	docs   []*xbson.Document
}

// opVirtual 是虚拟数据源的索引访问方式：从头到尾走一遍，谈不上什么索引。
type opVirtual struct {
	src *virtualSource
}

// indexName 没有索引名。
func (o *opVirtual) indexName() string { return "" }

// order 恒为升序。
func (o *opVirtual) order() Order { return Ascending }

// setOrder 无事可做：虚拟源的次序由它自己定。
func (o *opVirtual) setOrder(Order) {}

// keyOrdered 恒为假：虚拟源不保证按任何键有序。
func (o *opVirtual) keyOrdered() bool { return false }

// cost 恒为零：没有别的路可选。
func (o *opVirtual) cost(*xpage.CollectionIndex) uint32 { return 0 }

// mode 返回访问方式的描述。
func (o *opVirtual) mode() string { return "FULL COLLECTION SCAN" }

// execute 把文档序列变成一串命中项。
//
// **地址是现编的**：按流过的次序从 1 起编号，好让后面能按序号回查。
func (o *opVirtual) execute(xstore.Pages, *xpage.CollectionIndex, xcoll.Collation) iter.Seq2[hit, error] {
	return func(yield func(hit, error) bool) {
		i := 0
		for d, err := range o.src.seq {
			if err != nil {
				yield(hit{}, err)
				return
			}
			if o.src.retain {
				o.src.docs = append(o.src.docs, d)
			}
			i++
			h := hit{
				key:       *d.Value(),
				dataBlock: xpage.EmptyAddress,
				addr:      xpage.Address{PageID: uint32(i)},
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}

// isVirtual 判断这份计划走的是不是虚拟数据源。
func (p *Plan) isVirtual() bool {
	_, ok := p.index.(*opVirtual)
	return ok
}

// virtualLookup 从虚拟源取文档：命中项里带着文档本身，不必再读盘。
type virtualLookup struct{ src *virtualSource }

// load 从命中项里取出文档。
func (l *virtualLookup) load(h hit) (row, error) {
	d, ok := h.key.AsDocument()
	if !ok {
		return row{}, fmt.Errorf("%w: virtual source produced a non-document", ErrInternal)
	}
	return row{doc: d, addr: h.addr}, nil
}

// loadAddr 按现编的序号回查文档，要求源开了留存。
func (l *virtualLookup) loadAddr(a xpage.Address) (*xbson.Document, error) {
	i := int(a.PageID) - 1
	if i < 0 || i >= len(l.src.docs) {
		return nil, fmt.Errorf("%w: virtual document %s is out of range", ErrInternal, a)
	}
	return l.src.docs[i], nil
}
