package xbson

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
)

const (
	// MaxIndexKeyLength 是一个索引键最多占多少字节，含首字节。
	MaxIndexKeyLength = 1023

	// maxIndexKeyPayload 是字符串与二进制索引键的载荷上限，长度用 10 位存。
	maxIndexKeyPayload = 1023

	// typeMask 是首字节里存类型的低 6 位，lengthHighMask 是高 2 位。
	//
	// 字符串与二进制的长度要 10 位，低 8 位放第二个字节，高 2 位挤在首字节
	// 剩下的两位里——这样一个短字符串键只占两字节头。
	//
	// 代价是**类型编号必须小于 64**：向量的编号是 100，装不进这 6 位，
	// 所以向量写得出索引键却读不回来（读的时候会算出一个不存在的类型）。
	// 向量走的是专门的向量索引，不经过这里。
	typeMask = 0b0011_1111

	lengthHighMask = 0b1100_0000
)

// ErrIndexKeyTooLong 表示这个值太大，当不了索引键。
var ErrIndexKeyTooLong = errors.New("xbson: index key too long")

// ErrIndexKeyUnsupported 表示这个类型当不了索引键。
var ErrIndexKeyUnsupported = errors.New("xbson: value type cannot be an index key")

// IndexKeySize 算出这个值当索引键要占多少字节。
//
// 超长时报错而不是截断：截断过的键排序位置是错的，查找会漏。
func (v *Value) IndexKeySize() (int, error) {
	n, err := v.indexPayloadSize()
	if err != nil {
		return 0, err
	}
	total := 1 + n
	if total > MaxIndexKeyLength {
		return 0, fmt.Errorf("%w: %d bytes exceeds %d", ErrIndexKeyTooLong, total, MaxIndexKeyLength)
	}
	return total, nil
}

// indexPayloadSize 算出索引键的载荷字节数。
//
// 与文档编码的载荷不同：字符串与二进制这里只用 1 字节额外开销
// （长度的低 8 位），标识存成 16 字节裸值而不是带子类型的二进制。
func (v *Value) indexPayloadSize() (int, error) {
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
	case TypeDecimal, TypeGUID:
		return 16, nil
	case TypeString:
		if err := xbin.ValidateString(v.str); err != nil {
			return 0, err
		}
		return 1 + len(v.str), nil
	case TypeBinary:
		b, _ := v.AsBinary()
		return 1 + len(b), nil
	case TypeVector:
		f, _ := v.AsVector()

		return 2 + 4*len(f), nil
	case TypeDocument:
		d, _ := v.AsDocument()
		return d.encodedSize()
	case TypeArray:
		a, _ := v.AsArray()
		return a.encodedSize()
	default:
		return 0, fmt.Errorf("%w: %s", ErrIndexKeyUnsupported, v.t)
	}
}

// AppendIndexKey 把这个值编成索引键追加进 dst。
//
// 字符串与二进制走两字节头那条路（类型加 10 位长度），其余类型
// 首字节只放类型，后面跟定长载荷。
//
// 编码后的字节序**就是排序次序**：类型在最前，所以跨类型比较等于
// 比类型编号；同类型的定长载荷按小端存，所以不能直接按字节比，
// 比较仍要走 [Value.Compare]。
func (v *Value) AppendIndexKey(dst []byte) ([]byte, error) {
	if _, err := v.IndexKeySize(); err != nil {
		return nil, err
	}
	switch v.t {
	case TypeString, TypeBinary:
		var raw []byte
		if v.t == TypeString {
			if err := xbin.ValidateString(v.str); err != nil {
				return nil, err
			}
			raw = []byte(v.str)
		} else {
			raw, _ = v.AsBinary()
		}
		if len(raw) > maxIndexKeyPayload {
			return nil, fmt.Errorf("%w: payload %d exceeds %d",
				ErrIndexKeyTooLong, len(raw), maxIndexKeyPayload)
		}
		n := uint16(len(raw))

		dst = append(dst, byte(v.t)|byte((n&0b11_0000_0000)>>2), byte(n))
		return append(dst, raw...), nil
	}

	dst = append(dst, byte(v.t))
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
	case TypeDateTime:
		return appendUint64(dst, uint64(v.num)), nil
	case TypeDecimal:
		d, _ := v.AsDecimal()
		b := d.Bytes()
		return append(dst, b[:]...), nil
	case TypeObjectID:
		id, _ := v.AsObjectID()
		return append(dst, id[:]...), nil
	case TypeGUID:
		g, _ := v.AsGUID()
		b := g.Bytes()
		return append(dst, b[:]...), nil
	case TypeVector:
		f, _ := v.AsVector()
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
		return nil, fmt.Errorf("%w: %s", ErrIndexKeyUnsupported, v.t)
	}
}

// ReadIndexKey 从索引键读出一个值，返回用掉几个字节。
func ReadIndexKey(b []byte) (*Value, int, error) {
	v := new(Value)
	n, err := v.ReadIndexKeyInto(b)
	if err != nil {
		return nil, 0, err
	}
	return v, n, nil
}

// ReadIndexKeyInto 把索引键读进一个已有的值，避免每读一个键分配一次。
//
// 索引扫描一路要读成千上万个键，复用同一个值省下的就是那么多次分配。
// 所以**上一次读出的内容会被覆盖**：要留着就先拷一份。
//
// 日期在这里额外查一次范围：索引键里的计时单位是裸存的，
// 超范围的值后面转成时刻时才会报错，那时已经离出错的地方很远了。
func (v *Value) ReadIndexKeyInto(b []byte) (int, error) {
	if len(b) < 1 {
		return 0, fmt.Errorf("%w: empty index key", ErrCorrupt)
	}
	t := Type(b[0] & typeMask)

	switch t {
	case TypeString, TypeBinary:
		if len(b) < 2 {
			return 0, fmt.Errorf("%w: truncated index key length", ErrCorrupt)
		}
		n := int(b[0]&lengthHighMask)<<2 | int(b[1])
		if len(b) < 2+n {
			return 0, fmt.Errorf("%w: index key needs %d bytes, have %d", ErrCorrupt, 2+n, len(b))
		}
		raw := b[2 : 2+n]
		if t == TypeBinary {
			*v = Value{t: TypeBinary, ref: append([]byte(nil), raw...)}
			return 2 + n, nil
		}
		s := string(raw)
		if err := xbin.ValidateString(s); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		*v = Value{t: TypeString, str: s}
		return 2 + n, nil
	}

	p := b[1:]
	switch t {
	case TypeMinValue:
		*v = Value{t: TypeMinValue}
		return 1, nil
	case TypeNull:
		*v = Value{t: TypeNull}
		return 1, nil
	case TypeMaxValue:
		*v = Value{t: TypeMaxValue}
		return 1, nil
	case TypeBoolean:
		if err := need(p, 1); err != nil {
			return 0, err
		}
		var n int64
		if p[0] != 0 {
			n = 1
		}
		*v = Value{t: TypeBoolean, num: n}
		return 2, nil
	case TypeInt32:
		if err := need(p, 4); err != nil {
			return 0, err
		}
		*v = Value{t: TypeInt32, num: int64(int32(binary.LittleEndian.Uint32(p)))}
		return 5, nil
	case TypeInt64:
		if err := need(p, 8); err != nil {
			return 0, err
		}
		*v = Value{t: TypeInt64, num: int64(binary.LittleEndian.Uint64(p))}
		return 9, nil
	case TypeDouble:
		if err := need(p, 8); err != nil {
			return 0, err
		}
		*v = Value{t: TypeDouble, flt: math.Float64frombits(binary.LittleEndian.Uint64(p))}
		return 9, nil
	case TypeDateTime:
		if err := need(p, 8); err != nil {
			return 0, err
		}
		ticks := int64(binary.LittleEndian.Uint64(p))
		if ticks < xbin.MinTicks || ticks > xbin.MaxTicks {
			return 0, fmt.Errorf("%w: datetime ticks %d out of range", ErrCorrupt, ticks)
		}
		*v = Value{t: TypeDateTime, num: ticks}
		return 9, nil
	case TypeDecimal:
		if err := need(p, 16); err != nil {
			return 0, err
		}
		d, err := xbin.DecimalFromBytes(p[:16])
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		*v = Value{t: TypeDecimal, ref: d}
		return 17, nil
	case TypeObjectID:
		if err := need(p, 12); err != nil {
			return 0, err
		}
		id, err := ObjectIDFromBytes(p[:12])
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		*v = Value{t: TypeObjectID, ref: id}
		return 13, nil
	case TypeGUID:
		if err := need(p, 16); err != nil {
			return 0, err
		}
		g, err := xbin.GuidFromBytes(p[:16])
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		*v = Value{t: TypeGUID, ref: g}
		return 17, nil
	case TypeDocument:
		d, n, err := decoder{loc: time.UTC}.decodeDocument(p)
		if err != nil {
			return 0, err
		}
		*v = Value{t: TypeDocument, ref: d}
		return 1 + n, nil
	case TypeArray:
		a, n, err := decoder{loc: time.UTC}.decodeArray(p)
		if err != nil {
			return 0, err
		}
		*v = Value{t: TypeArray, ref: a}
		return 1 + n, nil
	default:
		return 0, fmt.Errorf("%w: unknown index key type %d", ErrCorrupt, byte(t))
	}
}
