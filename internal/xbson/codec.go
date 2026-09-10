package xbson

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/xmapst/xdoc/internal/xbin"
)

// Encode 把一篇文档编成字节。
//
// 先算出长度再一次分配：文档编码的开头就是总长度，不先算的话
// 要么反填、要么让缓冲反复扩容。
//
// 算出来的长度与实际写出的对不上时报错——那是编码器自身的缺陷，
// 放过去会写出一份长度字段错误、谁也读不回来的文档。
func (d *Document) Encode() ([]byte, error) {
	n, err := d.encodedSize(0)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, n)
	buf, err = d.appendTo(buf)
	if err != nil {
		return nil, err
	}
	if len(buf) != n {
		return nil, fmt.Errorf("xbson: size mismatch: predicted %d, wrote %d", n, len(buf))
	}
	return buf, nil
}

// encodedSize 算出文档编码后的字节数，并记进缓存。
//
// 5 是固定开销：4 字节长度加 1 字节结束标记。depth 是外面已经套了几层。
func (d *Document) encodedSize(depth int) (int, error) {
	if d == nil {
		return 5, nil
	}
	if err := checkNestingDepth(depth); err != nil {
		return 0, err
	}
	total := 5
	for k, v := range d.Elements() {
		n, err := v.elementSize(k, depth+1)
		if err != nil {
			return 0, err
		}
		total += n
	}
	d.length = total
	return total, nil
}

// encodedSize 算出数组编码后的字节数，并记进缓存。
func (a *Array) encodedSize(depth int) (int, error) {
	if a == nil {
		return 5, nil
	}
	if err := checkNestingDepth(depth); err != nil {
		return 0, err
	}
	total := 5
	for i, v := range a.items {
		n, err := v.elementSize(arrayKey(i), depth+1)
		if err != nil {
			return 0, err
		}
		total += n
	}
	a.length = total
	return total, nil
}

// checkNestingDepth 在写侧卡住嵌套深度，上限与读侧同为 [maxNestingDepth]。
//
// 超了就报错而不是照写：照写出去的文档读不回来。
func checkNestingDepth(depth int) error {
	if depth >= maxNestingDepth {
		return fmt.Errorf("xbson: document nested deeper than %d levels", maxNestingDepth)
	}
	return nil
}

// elementSize 算出「类型标记 + 键名 + 0 + 载荷」的字节数。
//
// 键名在这里查一次合法性：它以 0 结尾存放，串里再有一个 0
// 就会在读的时候提前截断。
func (v *Value) elementSize(key string, depth int) (int, error) {
	if err := xbin.ValidateCString(key); err != nil {
		return 0, fmt.Errorf("xbson: invalid element name: %w", err)
	}
	n, err := v.payloadSize(depth)
	if err != nil {
		return 0, err
	}
	return 1 + len(key) + 1 + n, nil
}

// payloadSize 算出一个值的载荷字节数。
//
// 字符串在这里查合法性——不合法的 UTF-8 写进去，读回来会变成
// 替换字符，那时原始内容已经找不回来了。
func (v *Value) payloadSize(depth int) (int, error) {
	switch v.t {
	case TypeMinValue, TypeNull, TypeMaxValue:
		return 0, nil
	case TypeBoolean:
		return 1, nil
	case TypeInt32:
		return 4, nil
	case TypeInt64, TypeDouble, TypeDateTime:
		return 8, nil
	case TypeObjectID:
		return 12, nil
	case TypeDecimal:
		return 16, nil
	case TypeString:
		if err := xbin.ValidateString(v.str); err != nil {
			return 0, err
		}

		return 4 + len(v.str) + 1, nil
	case TypeBinary:
		b, _ := v.AsBinary()

		return 4 + 1 + len(b), nil
	case TypeGUID:
		return 4 + 1 + 16, nil
	case TypeVector:
		f, _ := v.AsVector()

		return 2 + 4*len(f), nil
	case TypeDocument:
		d, _ := v.AsDocument()
		return d.encodedSize(depth)
	case TypeArray:
		a, _ := v.AsArray()
		return a.encodedSize(depth)
	default:
		return 0, fmt.Errorf("xbson: cannot encode type %s", v.t)
	}
}

// appendTo 把文档追加进 dst。
//
// 优先用缓存好的长度；文档改过则重算。
func (d *Document) appendTo(dst []byte) ([]byte, error) {
	n := d.length
	if n == 0 {
		var err error
		if n, err = d.encodedSize(0); err != nil {
			return nil, err
		}
	}
	dst = appendUint32(dst, uint32(n))
	var err error
	for k, v := range d.Elements() {
		if dst, err = v.appendElement(dst, k); err != nil {
			return nil, err
		}
	}
	return append(dst, 0), nil
}

// appendTo 把数组追加进 dst，数组在文件里就是键为下标串的文档。
func (a *Array) appendTo(dst []byte) ([]byte, error) {
	n := a.length
	if n == 0 {
		var err error
		if n, err = a.encodedSize(0); err != nil {
			return nil, err
		}
	}
	dst = appendUint32(dst, uint32(n))
	var err error
	for i, v := range a.items {
		if dst, err = v.appendElement(dst, arrayKey(i)); err != nil {
			return nil, err
		}
	}
	return append(dst, 0), nil
}

// appendElement 追加一个「类型标记 + 键名 + 0 + 载荷」。
func (v *Value) appendElement(dst []byte, key string) ([]byte, error) {
	tag, err := v.wireTag()
	if err != nil {
		return nil, err
	}
	dst = append(dst, tag)
	dst = append(dst, key...)
	dst = append(dst, 0)
	return v.appendPayload(dst)
}

// wireTag 返回写进文件的类型标记。
//
// 标识没有自己的标记，它写成带 UUID 子类型的二进制，
// 读的时候靠子类型区分。
func (v *Value) wireTag() (byte, error) {
	switch v.t {
	case TypeDouble:
		return tagDouble, nil
	case TypeString:
		return tagString, nil
	case TypeDocument:
		return tagDocument, nil
	case TypeArray:
		return tagArray, nil
	case TypeBinary, TypeGUID:
		return tagBinary, nil
	case TypeObjectID:
		return tagObjectID, nil
	case TypeBoolean:
		return tagBoolean, nil
	case TypeDateTime:
		return tagDateTime, nil
	case TypeNull:
		return tagNull, nil
	case TypeInt32:
		return tagInt32, nil
	case TypeInt64:
		return tagInt64, nil
	case TypeDecimal:
		return tagDecimal, nil
	case TypeVector:
		return tagVector, nil
	case TypeMaxValue:
		return tagMaxValue, nil
	case TypeMinValue:
		return tagMinValue, nil
	default:
		return 0, fmt.Errorf("xbson: cannot encode type %s", v.t)
	}
}

// appendPayload 追加一个值的载荷。
//
// 字符串的长度字段**含末尾那个 0**，二进制的不含——两者的约定不同，
// 是格式定下的。
//
// 向量的维数用两个字节存，所以最多 65535 维。
func (v *Value) appendPayload(dst []byte) ([]byte, error) {
	switch v.t {
	case TypeMinValue, TypeNull, TypeMaxValue:
		return dst, nil
	case TypeBoolean:
		if v.num != 0 {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case TypeInt32:
		return appendUint32(dst, uint32(int32(v.num))), nil
	case TypeInt64:
		return appendUint64(dst, uint64(v.num)), nil
	case TypeDouble:
		return appendUint64(dst, math.Float64bits(v.flt)), nil
	case TypeDecimal:
		d, _ := v.AsDecimal()
		b := d.Bytes()
		return append(dst, b[:]...), nil
	case TypeString:
		dst = appendUint32(dst, uint32(len(v.str)+1))
		dst = append(dst, v.str...)
		return append(dst, 0), nil
	case TypeBinary:
		b, _ := v.AsBinary()
		dst = appendUint32(dst, uint32(len(b)))
		dst = append(dst, subtypeGeneric)
		return append(dst, b...), nil
	case TypeGUID:
		g, _ := v.AsGUID()
		dst = appendUint32(dst, 16)
		dst = append(dst, subtypeUUID)
		b := g.Bytes()
		return append(dst, b[:]...), nil
	case TypeObjectID:
		id, _ := v.AsObjectID()
		return append(dst, id[:]...), nil
	case TypeDateTime:
		ms, err := ticksToWireMillis(v.num)
		if err != nil {
			return nil, err
		}
		return appendUint64(dst, uint64(ms)), nil
	case TypeVector:
		f, _ := v.AsVector()

		if len(f) > math.MaxUint16 {
			return nil, fmt.Errorf("xbson: vector has %d dimensions, the format holds at most %d",
				len(f), math.MaxUint16)
		}
		dst = binary.LittleEndian.AppendUint16(dst, uint16(len(f)))
		for _, x := range f {
			dst = appendUint32(dst, math.Float32bits(x))
		}
		return dst, nil
	case TypeDocument:
		d, _ := v.AsDocument()
		return d.appendTo(dst)
	case TypeArray:
		a, _ := v.AsArray()
		return a.appendTo(dst)
	default:
		return nil, fmt.Errorf("xbson: cannot encode type %s", v.t)
	}
}

// ticksToWireMillis 把计时单位转成写进文件的 Unix 毫秒。
//
// 两个端点特判：毫秒精度装不下它们，用边界值代表，
// 读回来时再特判回去。
func ticksToWireMillis(ticks int64) (int64, error) {
	switch ticks {
	case xbin.MinTicks:
		return xbin.MinUnixMillis, nil
	case xbin.MaxTicks:
		return xbin.MaxUnixMillis, nil
	}
	t, err := xbin.TicksToTime(ticks)
	if err != nil {
		return 0, err
	}
	return xbin.TimeToUnixMillis(t)
}

// 文件里的多字节整数一律小端。
func appendUint32(dst []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(dst, v) }
func appendUint64(dst []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(dst, v) }
