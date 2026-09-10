package xtx

import (
	"math/bits"
	"sync"
	"sync/atomic"

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

// Cache 是页缓存，容量有上限，按 CLOCK（二次机会）淘汰。
//
// 按键散列分成若干片，每片一把读写锁。命中只拿读锁、置一下原子引用位，
// 不挪动任何结构，并发读页因此不会互相排队；没命中往里放时才拿那一片的写锁。
// 各片容量加起来正好是总容量。
//
// 可并发使用。存的是页缓冲本身，不拷贝——取出来的切片与缓存里那份是同一块内存。
type Cache struct {
	shards   []cacheShard
	shift    uint
	capacity int
}

// cacheShard 是缓存的一片：一张映射加一个环，指针在环上转，找没被读过的项淘汰。
type cacheShard struct {
	mu       sync.RWMutex
	items    map[cacheKey]*cacheItem
	ring     []*cacheItem
	hand     int
	capacity int

	// ctr 指向 [Stats] 里这一片的命中计数。
	ctr *cacheCounter
}

// cacheItem 是缓存里的一项，键留在里面好从环反查回映射。
//
// 放进去之后 key 和 buf 不再改，只有 ref 会变；淘汰掉的项不复用。
type cacheItem struct {
	key cacheKey
	buf []byte
	ref atomic.Bool
}

// DefaultCacheCapacity 是没指定容量时能存几页。
const DefaultCacheCapacity = 5000

const (
	// maxCacheShards 是最多分几片。
	maxCacheShards = 16

	// minCacheShardPages 是每片至少几页；容量小时少分几片，免得片太小、淘汰得太早。
	minCacheShardPages = 256
)

// NewCache 开一个页缓存；容量不为正时用默认值。
func NewCache(capacity int) *Cache { return newCache(capacity, new(Stats)) }

// newCache 同 [NewCache]，命中与未命中记进 st。
func newCache(capacity int, st *Stats) *Cache {
	if capacity <= 0 {
		capacity = DefaultCacheCapacity
	}
	n := 1
	for n < maxCacheShards && capacity/(n*2) >= minCacheShardPages {
		n *= 2
	}
	c := &Cache{
		shards:   make([]cacheShard, n),
		shift:    uint(64 - bits.TrailingZeros(uint(n))),
		capacity: capacity,
	}
	for i := range c.shards {
		s := &c.shards[i]
		s.capacity = capacity / n
		if i < capacity%n {
			s.capacity++
		}
		s.items = make(map[cacheKey]*cacheItem, s.capacity)
		s.ring = make([]*cacheItem, 0, s.capacity)
		s.ctr = &st.cache[i]
	}
	return c
}

// shard 找键落在哪一片。只分一片时 shift 是 64，移出来恒为零。
func (c *Cache) shard(k cacheKey) *cacheShard {
	h := (uint64(k.pos) ^ uint64(k.origin)<<56) * 0x9E3779B97F4A7C15
	return &c.shards[h>>c.shift]
}

// Get 取一页，命中时置上引用位。
//
// 引用位在读锁里置：放页的一方拿着写锁转指针时，不会有人同时把位再置回去。
func (c *Cache) Get(origin Origin, pos int64) ([]byte, bool) {
	k := cacheKey{origin, pos}
	s := c.shard(k)
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[k]
	if !ok {
		s.ctr.misses.Add(1)
		return nil, false
	}
	s.ctr.hits.Add(1)
	if !it.ref.Load() {
		it.ref.Store(true)
	}
	return it.buf, true
}

// Put 放一页进去，那一片满了就转指针淘汰一项。
//
// 已经在里面的**不覆盖**，只置引用位——同一个位置的内容是不变的。
func (c *Cache) Put(origin Origin, pos int64, buf []byte) {
	k := cacheKey{origin, pos}
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if it, ok := s.items[k]; ok {
		it.ref.Store(true)
		return
	}
	it := &cacheItem{key: k, buf: buf}
	s.items[k] = it
	if len(s.ring) < s.capacity {
		s.ring = append(s.ring, it)
		return
	}
	// 读过的清掉引用位放过去，停在第一个没读过的上面换掉；最多转两圈。
	for {
		old := s.ring[s.hand]
		if old.ref.Load() {
			old.ref.Store(false)
			s.hand = (s.hand + 1) % len(s.ring)
			continue
		}
		delete(s.items, old.key)
		s.ring[s.hand] = it
		s.hand = (s.hand + 1) % len(s.ring)
		return
	}
}

// Clear 清空缓存。
func (c *Cache) Clear() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		clear(s.items)
		clear(s.ring)
		s.ring = s.ring[:0]
		s.hand = 0
		s.mu.Unlock()
	}
}

// DropLogAndData 丢掉所有日志页，以及 moved 里那些页号对应的数据页，返回剩下几项。
//
// 检查点之后调用：日志已经清空，缓存里的日志页全都指向不存在的位置；
// 被搬回数据文件的那些页号，缓存里的旧内容也过期了。
func (c *Cache) DropLogAndData(moved map[uint32]struct{}) int {
	n := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		kept := s.ring[:0]
		for _, it := range s.ring {
			drop := it.key.origin == OriginLog
			if !drop {
				_, drop = moved[uint32(it.key.pos/xpage.PageSize)]
			}
			if drop {
				delete(s.items, it.key)
				continue
			}
			kept = append(kept, it)
		}
		clear(s.ring[len(kept):])
		s.ring = kept
		if s.hand >= len(kept) {
			s.hand = 0
		}
		n += len(kept)
		s.mu.Unlock()
	}
	return n
}

// Len 返回当前缓存了几页。
func (c *Cache) Len() int {
	n := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		n += len(s.ring)
		s.mu.RUnlock()
	}
	return n
}

// Capacity 返回容量。
func (c *Cache) Capacity() int { return c.capacity }
