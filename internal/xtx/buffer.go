package xtx

import (
	"container/list"
	"sync"

	"github.com/xmapst/xdoc/internal/xpage"
)

// Origin 说明一页是从哪份文件读来的。同一个页号在两份文件里都可能有，缓存因此要连它一起当键。
type Origin uint8

const (
	// OriginNone 表示还没落盘——事务内新改的页就是这样。
	OriginNone Origin = iota

	// OriginData 表示来自数据文件。
	OriginData

	// OriginLog 表示来自日志文件。
	OriginLog
)

// String 返回来源名。
func (o Origin) String() string {
	switch o {
	case OriginNone:
		return "None"
	case OriginData:
		return "Data"
	case OriginLog:
		return "Log"
	}
	return "Unknown"
}

// bufPool 是整页缓冲的池子。
var bufPool = sync.Pool{New: func() any { b := make([]byte, xpage.PageSize); return &b }}

// getBuf 取一块清好零的整页缓冲。
func getBuf() []byte {
	b := *bufPool.Get().(*[]byte)
	clear(b)
	return b
}

// putBuf 把整页缓冲还回池里；长度不对的直接丢掉。
func putBuf(b []byte) {
	if len(b) == xpage.PageSize {
		bufPool.Put(&b)
	}
}

// cacheKey 是缓存的键：哪份文件的哪个字节偏移。
type cacheKey struct {
	origin Origin
	pos    int64
}

// Cache 是页缓存，按最近最少使用淘汰。
//
// 自带互斥锁，可并发使用。存的是页缓冲本身，不拷贝——取出来的切片
// 与缓存里那份是同一块内存。
type Cache struct {
	mu       sync.Mutex
	items    map[cacheKey]*list.Element
	order    *list.List
	capacity int
}

// cacheItem 是缓存里的一项，键留在里面好从链表反查回映射。
type cacheItem struct {
	key cacheKey
	buf []byte
}

// DefaultCacheCapacity 是没指定容量时能存几页。
const DefaultCacheCapacity = 5000

// NewCache 开一个页缓存；容量不为正时用默认值。
func NewCache(capacity int) *Cache {
	if capacity <= 0 {
		capacity = DefaultCacheCapacity
	}
	return &Cache{
		items:    make(map[cacheKey]*list.Element, capacity),
		order:    list.New(),
		capacity: capacity,
	}
}

// Get 取一页，命中时把它挪到链表最前。
func (c *Cache) Get(origin Origin, pos int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[cacheKey{origin, pos}]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheItem).buf, true
}

// Put 放一页进去，超出容量就淘汰链表末尾那些。
//
// 已经在里面的**不覆盖**，只挪到最前——同一个位置的内容是不变的。
func (c *Cache) Put(origin Origin, pos int64, buf []byte) {
	k := cacheKey{origin, pos}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&cacheItem{key: k, buf: buf})
	for c.order.Len() > c.capacity {
		back := c.order.Back()
		c.order.Remove(back)
		delete(c.items, back.Value.(*cacheItem).key)
	}
}

// Clear 清空缓存。
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.items)
	c.order.Init()
}

// DropLogAndData 丢掉所有日志页，以及 moved 里那些页号对应的数据页，返回剩下几项。
//
// 检查点之后调用：日志已经清空，缓存里的日志页全都指向不存在的位置；
// 被搬回数据文件的那些页号，缓存里的旧内容也过期了。
func (c *Cache) DropLogAndData(moved map[uint32]struct{}) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.order.Front(); e != nil; {
		next := e.Next()
		it := e.Value.(*cacheItem)
		drop := it.key.origin == OriginLog
		if !drop {
			if _, ok := moved[uint32(it.key.pos/xpage.PageSize)]; ok {
				drop = true
			}
		}
		if drop {
			c.order.Remove(e)
			delete(c.items, it.key)
		}
		e = next
	}
	return c.order.Len()
}

// Len 返回当前缓存了几页。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Capacity 返回容量。
func (c *Cache) Capacity() int { return c.capacity }
