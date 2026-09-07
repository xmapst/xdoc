package xstore

import "sync"

// MaxLevel 是跳表节点的层数上限。
const MaxLevel = 32

// randomizer 是一个减法式滞后 Fibonacci 伪随机数发生器。
//
// 只给跳表和向量图掷层数用，不做密码学用途。自带互斥锁，可并发调用。
type randomizer struct {
	mu    sync.Mutex
	state [56]int32
	i, j  int
}

const (
	// randMax 是取值上界（不含），也是负数回绕时加回的模数。
	randMax = 2147483647
	// randSeed 是初始化用的常数种子。
	randSeed = 161803398
)

// newRandomizer 按给定种子铺开 55 个状态字，再空转四轮把它们搅匀。
//
// 种子取绝对值；int32 最小值取绝对值会溢出，所以单独挑出来当上界处理。
func newRandomizer(seed int32) *randomizer {
	r := &randomizer{}
	sub := int32(randMax)
	if seed != -2147483648 {
		sub = seed
		if sub < 0 {
			sub = -sub
		}
	}
	mj := int32(randSeed - sub)
	r.state[55] = mj
	mk := int32(1)
	for i := 1; i <= 54; i++ {
		ii := (21 * i) % 55
		r.state[ii] = mk
		mk = mj - mk
		if mk < 0 {
			mk += randMax
		}
		mj = r.state[ii]
	}
	for range 4 {
		for i := 1; i <= 55; i++ {
			r.state[i] -= r.state[1+(i+30)%55]
			if r.state[i] < 0 {
				r.state[i] += randMax
			}
		}
	}
	r.i, r.j = 0, 21
	return r
}

// next 产出下一个 [0, randMax) 的伪随机数。
func (r *randomizer) next() int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.i++
	if r.i == 56 {
		r.i = 1
	}
	r.j++
	if r.j == 56 {
		r.j = 1
	}
	v := r.state[r.i] - r.state[r.j]
	if v == randMax {
		v--
	}
	if v < 0 {
		v += randMax
	}
	r.state[r.i] = v
	return v
}

// flip 掷出一个跳表层数：连着掷到偶数为止，每掷出一个奇数就多一层。
//
// 于是有一半的节点只有一层，四分之一有两层，依此类推；最多 [MaxLevel] 层。
func (r *randomizer) flip() int {
	levels := 1
	for v := r.next(); v&1 == 1; v >>= 1 {
		levels++
		if levels == MaxLevel {
			break
		}
	}
	return levels
}

// flipTo 同 flip，但层数上限由调用方给定。上限不大于 1 时直接返回 1，不消耗随机数。
func (r *randomizer) flipTo(max int) int {
	levels := 1
	if max <= 1 {
		return 1
	}
	for v := r.next(); v&1 == 1; v >>= 1 {
		levels++
		if levels == max {
			break
		}
	}
	return levels
}

// defaultRandomizer 是全进程共用的那一个，种子固定，因此层数分布可复现。
var defaultRandomizer = newRandomizer(0)
