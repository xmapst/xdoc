package xbson

import (
	"iter"
	"maps"
	"slices"
	"strings"
)

// idKey 是主键字段名。
const idKey = "_id"

// Document 是一篇有序的键值文档。
//
// **键的先后次序会写进文件**，读回来还是这个次序，所以不能用 map 存。
//
// 键名比较**不区分大小写**（只折 ASCII 字母）：同一个键用不同大小写写两遍，
// 后一次是覆盖而不是新增。
type Document struct {
	// keys 与 vals 一一对应，次序就是文档里的次序。
	keys []string
	vals []*Value

	// idx 是键名（已折成大写）到下标的索引，只有键多到一定数量才建。
	//
	// 键少的时候线性扫比查表快，也省掉一次分配。
	idx map[string]int

	// length 缓存编码后的字节数，0 表示还没算过。
	//
	// 文档一改就作废，见 [Document.invalidate]。
	length int
}

// linearScanMax 是超过多少个键就改用索引查找。
const linearScanMax = 16

// NewDocument 建一篇空文档。
func NewDocument() *Document { return &Document{} }

// DocumentOf 用交替的键、值造一篇文档。
//
// 键不是字符串的整对跳过；值不是 [Value] 的写成空值。
// 这是给内部构造用的，调用方保证参数形状。
func DocumentOf(kv ...any) *Document {
	d := NewDocument()
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		v, ok := kv[i+1].(*Value)
		if !ok {
			v = Null
		}
		d.Set(k, v)
	}
	return d
}

// foldKey 把键名里的 ASCII 小写字母折成大写，供索引用。
//
// 只折 ASCII：非 ASCII 的大小写折叠依赖语言，而键名比较必须在
// 任何环境下给出同样的结果。
//
// 没有小写字母时原样返回，省掉一次分配。
func foldKey(k string) string {
	need := false
	for i := 0; i < len(k); i++ {
		if c := k[i]; c >= 'a' && c <= 'z' {
			need = true
			break
		}
	}
	if !need {
		return k
	}
	b := []byte(k)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// foldEqual 按折叠后的形式比较两个键名，不实际分配新串。
//
// 线性扫的路径上每个键都要比一次，先折再比会分配一堆临时串。
func foldEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if x >= 'a' && x <= 'z' {
			x -= 'a' - 'A'
		}
		if y >= 'a' && y <= 'z' {
			y -= 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// find 返回键的下标，没有给 -1。
func (d *Document) find(key string) int {
	if d.idx == nil {
		for i, k := range d.keys {
			if foldEqual(k, key) {
				return i
			}
		}
		return -1
	}
	if i, ok := d.idx[foldKey(key)]; ok {
		return i
	}
	return -1
}

// buildIndex 建起键名索引。
func (d *Document) buildIndex() {
	d.idx = make(map[string]int, len(d.keys)*2)
	for i, k := range d.keys {
		d.idx[foldKey(k)] = i
	}
}

// Len 返回键的个数。
func (d *Document) Len() int { return len(d.keys) }

// Get 取一个键的值，**没有这个键时返回 [Null]**，不是 nil。
//
// 这样取值链上不必每一步都判 nil。要区分「没有」与「值是空」，
// 用 [Document.Has]。
func (d *Document) Get(key string) *Value {
	if i := d.find(key); i >= 0 {
		return d.vals[i]
	}
	return Null
}

// Has 报告有没有这个键。
func (d *Document) Has(key string) bool { return d.find(key) >= 0 }

// Set 设一个键。已有则覆盖值、**保持原来的位置**；没有则追加到末尾。
//
// nil 当作 [Null]。
func (d *Document) Set(key string, v *Value) {
	if v == nil {
		v = Null
	}
	d.invalidate()
	if i := d.find(key); i >= 0 {
		d.vals[i] = v
		return
	}
	if d.idx != nil {
		d.idx[foldKey(key)] = len(d.keys)
	}
	d.keys = append(d.keys, key)
	d.vals = append(d.vals, v)
	if d.idx == nil && len(d.keys) > linearScanMax {
		d.buildIndex()
	}
}

// Delete 删一个键，没有则返回 false。
//
// 删完要把索引里所有更靠后的下标减一——这是个线性操作，
// 但删键本身也要挪切片，两者同量级。
func (d *Document) Delete(key string) bool {
	i := d.find(key)
	if i < 0 {
		return false
	}
	d.invalidate()
	d.keys = slices.Delete(d.keys, i, i+1)
	d.vals = slices.Delete(d.vals, i, i+1)
	if d.idx == nil {
		return true
	}
	delete(d.idx, foldKey(key))

	for k, j := range d.idx {
		if j > i {
			d.idx[k] = j - 1
		}
	}
	return true
}

// Keys 按文档次序返回全部键名，返回的是副本。
func (d *Document) Keys() []string { return slices.Clone(d.keys) }

// Elements 遍历全部键值，**主键排在最前**，其余保持文档次序。
//
// 主键提前是格式的一部分：编码时按这个次序写，读回来主键就在开头。
func (d *Document) Elements() iter.Seq2[string, *Value] {
	return func(yield func(string, *Value) bool) {
		idIdx := d.find(idKey)
		if idIdx >= 0 {
			i := idIdx

			if !yield(idKey, d.vals[i]) {
				return
			}
		}
		for i, k := range d.keys {
			if i == idIdx {
				continue
			}
			if !yield(k, d.vals[i]) {
				return
			}
		}
	}
}

// Clone 深拷一篇文档，嵌套的文档、数组、二进制都各拷一份。
func (d *Document) Clone() *Document {
	n := &Document{
		keys: slices.Clone(d.keys),
		vals: make([]*Value, len(d.vals)),
	}
	for i, v := range d.vals {
		n.vals[i] = v.Clone()
	}
	if d.idx != nil {
		n.idx = make(map[string]int, len(d.idx))
		maps.Copy(n.idx, d.idx)
	}
	return n
}

// invalidate 作废缓存的编码长度。
func (d *Document) invalidate() { d.length = 0 }

// Array 是一列值。
type Array struct {
	// items 是元素，length 缓存编码后的字节数。
	items []*Value

	length int
}

// NewArray 建一个数组，nil 元素写成 [Null]。
func NewArray(items ...*Value) *Array {
	a := &Array{items: make([]*Value, 0, len(items))}
	for _, v := range items {
		a.Append(v)
	}
	return a
}

// Len 返回元素个数。
func (a *Array) Len() int { return len(a.items) }

// At 取第 i 个元素，**下标越界返回 [Null]** 而不是 panic。
func (a *Array) At(i int) *Value {
	if i < 0 || i >= len(a.items) {
		return Null
	}
	return a.items[i]
}

// Append 追加一个元素，nil 当作 [Null]。
func (a *Array) Append(v *Value) {
	if v == nil {
		v = Null
	}
	a.length = 0
	a.items = append(a.items, v)
}

// Set 改一个元素，下标越界时什么也不做。
func (a *Array) Set(i int, v *Value) {
	if i < 0 || i >= len(a.items) {
		return
	}
	if v == nil {
		v = Null
	}
	a.length = 0
	a.items[i] = v
}

// Items 返回内部切片，别改它。
func (a *Array) Items() []*Value { return a.items }

// Clone 深拷一个数组。
func (a *Array) Clone() *Array {
	n := &Array{items: make([]*Value, len(a.items))}
	for i, v := range a.items {
		n.items[i] = v.Clone()
	}
	return n
}

// Clone 深拷一个值。
//
// 只有可变的那几种真的拷贝；其余类型的值是只读的，直接共享。
func (v *Value) Clone() *Value {
	switch v.t {
	case TypeDocument:
		d, _ := v.AsDocument()
		return d.Clone().Value()
	case TypeArray:
		a, _ := v.AsArray()
		return a.Clone().Value()
	case TypeBinary:
		b, _ := v.AsBinary()
		return Binary(slices.Clone(b))
	case TypeVector:
		f, _ := v.AsVector()
		return Vector(slices.Clone(f))
	default:
		return v
	}
}

// arrayKey 返回数组下标对应的键名。
//
// 数组在文件里存成键为 "0"、"1"…… 的文档，所以每个下标都要一个串。
// 小下标直接查表，省掉分配。
func arrayKey(i int) string {
	if i < len(smallInts) {
		return smallInts[i]
	}
	return itoa(i)
}

// smallInts 是前几十个下标的键名，避开重复分配。
var smallInts = [...]string{
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	"10", "11", "12", "13", "14", "15", "16", "17", "18", "19",
	"20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31",
}

// itoa 把整数转成串。
//
// 自己写一个是为了不引 strconv：这个包在编解码的热路径上，
// 少一个依赖也少一层间接。
func itoa(i int) string {
	var b [20]byte
	p := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			break
		}
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// maxStringDepth 是可读形式最多展开几层嵌套。
const maxStringDepth = 8

// String 给出一个可读形式，供错误消息与调试用。
//
// 嵌套太深的部分用省略号收住：这里的用途是让人看一眼，
// 不是完整导出。
func (d *Document) String() string { return d.docString(maxStringDepth) }

// String 给出一个可读形式，嵌套太深的部分用省略号收住。
func (a *Array) String() string { return a.arrayString(maxStringDepth) }

// docString 按剩余深度排出文档的可读形式。
func (d *Document) docString(depth int) string {
	if d == nil {
		return "<nil>"
	}
	if depth <= 0 {
		return "{…}"
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range d.Elements() {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v.valueString(depth - 1))
	}
	b.WriteByte('}')
	return b.String()
}

// arrayString 按剩余深度排出数组的可读形式。
func (a *Array) arrayString(depth int) string {
	if a == nil {
		return "<nil>"
	}
	if depth <= 0 {
		return "[…]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range a.items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(v.valueString(depth - 1))
	}
	b.WriteByte(']')
	return b.String()
}

// valueString 按剩余深度排出一个值的可读形式。
func (v *Value) valueString(depth int) string {
	if v == nil {
		return "<nil>"
	}
	switch v.t {
	case TypeDocument:
		d, _ := v.AsDocument()
		return d.docString(depth)
	case TypeArray:
		a, _ := v.AsArray()
		return a.arrayString(depth)
	default:
		return v.String()
	}
}
