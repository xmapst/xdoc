package xbson

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
)

// ErrCorrupt 表示这段字节不是一篇合法的文档。
//
// 解码时的每一处检查都包在它下面，让上层能一次判定「这是数据坏了」
// 而不是「用法不对」。
var ErrCorrupt = errors.New("xbson: corrupt document")

// maxDocumentSize 是解码时接受的单篇文档上限。
//
// 有这个上限，一段坏字节里的长度字段就不会让解码器先分配几个 GB
// 再发现读不下去。
const maxDocumentSize = 16 << 20

// maxNestingDepth 是文档与数组最多嵌套几层，最外层的文档算第 1 层。
//
// 编解码都是递归的，而每层只占 7 个字节，16 MB 里塞得下两百多万层——
// 没有这个上限，一段坏字节就能让解码器栈溢出，那是 recover 接不住的崩溃。
// 编码卡同一个上限，保证写得进的一定读得出，自引用的文档也停在这里。
//
// 取 1024：不低于 JSON 解析允许的 1000 层，JSON 读得进来的文档都存得下；
// 映射默认 20 层、表达式 128 层，实际的文档远到不了这个深度。
const maxNestingDepth = 1024

// Decode 解出一篇文档，日期按 UTC。
func Decode(b []byte) (*Document, error) { return DecodeIn(b, time.UTC) }

// DecodeIn 解出一篇文档，日期落在指定时区。
//
// **整段字节必须刚好用完**：多出来的尾巴判成损坏，而不是静默忽略。
func DecodeIn(b []byte, loc *time.Location) (*Document, error) {
	d, n, err := decoder{loc: loc}.decodeDocument(b)
	if err != nil {
		return nil, err
	}
	if n != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, len(b)-n)
	}
	return d, nil
}

// DecodePrefix 解出开头的一篇文档，返回用掉几个字节。
//
// 一页里连着放好几篇文档时走这条。
func DecodePrefix(b []byte) (*Document, int, error) { return decoder{loc: time.UTC}.decodeDocument(b) }

// decoder 带着解码时的选项与状态：日期该落在哪个时区，当前嵌套了几层。
//
// 按值传递：往下走一层就复制一份改掉 depth，外层因此不受影响。
type decoder struct {
	loc   *time.Location
	depth int
}

// deeper 下探一层，超过 [maxNestingDepth] 就判成损坏。
func (dec decoder) deeper() (decoder, error) {
	if dec.depth >= maxNestingDepth {
		return dec, fmt.Errorf("%w: nested deeper than %d levels", ErrCorrupt, maxNestingDepth)
	}
	dec.depth++
	return dec, nil
}

// decodeDocument 解出一篇文档，返回用掉几个字节。
//
// **键重复判成损坏**：文档的键是唯一的，出现两个同名键说明这段字节
// 不是这个编码器写出来的，后面的解读也就不可信了。
func (dec decoder) decodeDocument(b []byte) (*Document, int, error) {
	dec, err := dec.deeper()
	if err != nil {
		return nil, 0, err
	}
	total, err := docHeader(b)
	if err != nil {
		return nil, 0, err
	}
	d := NewDocument()

	body := b[4 : total-1]
	for p := 0; p < len(body); {
		key, v, n, err := dec.decodeElement(body[p:])
		if err != nil {
			return nil, 0, err
		}
		if !d.add(key, v) {
			return nil, 0, fmt.Errorf("%w: duplicate key %q", ErrCorrupt, key)
		}
		p += n
	}
	if b[total-1] != 0 {
		return nil, 0, fmt.Errorf("%w: document not terminated", ErrCorrupt)
	}
	d.length = total
	return d, total, nil
}

// decodeArray 解出一个数组。
//
// 键名被忽略：数组在文件里存成键为下标串的文档，但读的时候只按出现
// 次序取值——键名与位置对不上的情形照样能读出来。
func (dec decoder) decodeArray(b []byte) (*Array, int, error) {
	dec, err := dec.deeper()
	if err != nil {
		return nil, 0, err
	}
	total, err := docHeader(b)
	if err != nil {
		return nil, 0, err
	}
	a := NewArray()
	body := b[4 : total-1]
	for p := 0; p < len(body); {
		_, v, n, err := dec.decodeElement(body[p:])
		if err != nil {
			return nil, 0, err
		}
		a.Append(v)
		p += n
	}
	if b[total-1] != 0 {
		return nil, 0, fmt.Errorf("%w: array not terminated", ErrCorrupt)
	}
	a.length = total
	return a, total, nil
}

// docHeader 读出并检查文档开头的长度字段。
//
// 四道检查缺一不可：够不够读长度、长度是否小于最小值、是否超过上限、
// 是否超过手上的字节数。少一道，一段坏字节就能让后面的切片越界。
func docHeader(b []byte) (int, error) {
	if len(b) < 5 {
		return 0, fmt.Errorf("%w: need 5 bytes for an empty document, have %d", ErrCorrupt, len(b))
	}
	total := int(int32(binary.LittleEndian.Uint32(b)))
	if total < 5 {
		return 0, fmt.Errorf("%w: declared length %d below minimum 5", ErrCorrupt, total)
	}
	if total > maxDocumentSize {
		return 0, fmt.Errorf("%w: declared length %d exceeds %d", ErrCorrupt, total, maxDocumentSize)
	}
	if total > len(b) {
		return 0, fmt.Errorf("%w: declared length %d exceeds %d available bytes", ErrCorrupt, total, len(b))
	}
	return total, nil
}

// decodeElement 解出一个「类型标记 + 键名 + 载荷」，错误里带上键名。
func (dec decoder) decodeElement(b []byte) (string, *Value, int, error) {
	if len(b) < 2 {
		return "", nil, 0, fmt.Errorf("%w: truncated element header", ErrCorrupt)
	}
	tag := b[0]
	key, n, err := readCString(b[1:])
	if err != nil {
		return "", nil, 0, err
	}
	p := 1 + n
	v, m, err := dec.decodePayload(tag, b[p:])
	if err != nil {
		return "", nil, 0, fmt.Errorf("field %q: %w", key, err)
	}
	return key, v, p + m, nil
}

// readCString 读出一个以 0 结尾的键名。
//
// 顺带查一次 UTF-8：键名要拿去比较与查找，不合法的字节会让
// 同一个键在不同路径上比出不同结果。
func readCString(b []byte) (string, int, error) {
	for i, c := range b {
		if c != 0 {
			continue
		}
		s := string(b[:i])
		if err := xbin.ValidateString(s); err != nil {
			return "", 0, fmt.Errorf("%w: element name: %w", ErrCorrupt, err)
		}
		return s, i + 1, nil
	}
	return "", 0, fmt.Errorf("%w: unterminated element name", ErrCorrupt)
}

// decodePayload 按类型标记解出载荷，返回用掉几个字节。
//
// 每一种都先查够不够字节再读，认不出的标记判成损坏。
//
// 字符串的长度字段含末尾那个 0，所以最小是 1；二进制的不含。
func (dec decoder) decodePayload(tag byte, b []byte) (*Value, int, error) {
	switch tag {
	case tagMinValue:
		return MinValue, 0, nil
	case tagNull:
		return Null, 0, nil
	case tagMaxValue:
		return MaxValue, 0, nil

	case tagBoolean:
		if err := need(b, 1); err != nil {
			return nil, 0, err
		}
		return Boolean(b[0] != 0), 1, nil

	case tagInt32:
		if err := need(b, 4); err != nil {
			return nil, 0, err
		}
		return Int32(int32(binary.LittleEndian.Uint32(b))), 4, nil

	case tagInt64:
		if err := need(b, 8); err != nil {
			return nil, 0, err
		}
		return Int64(int64(binary.LittleEndian.Uint64(b))), 8, nil

	case tagDouble:
		if err := need(b, 8); err != nil {
			return nil, 0, err
		}
		return Double(math.Float64frombits(binary.LittleEndian.Uint64(b))), 8, nil

	case tagDecimal:
		if err := need(b, 16); err != nil {
			return nil, 0, err
		}
		d, err := xbin.DecimalFromBytes(b[:16])
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return Decimal(d), 16, nil

	case tagObjectID:
		if err := need(b, 12); err != nil {
			return nil, 0, err
		}
		id, err := ObjectIDFromBytes(b[:12])
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return OID(id), 12, nil

	case tagDateTime:
		if err := need(b, 8); err != nil {
			return nil, 0, err
		}
		v, err := dec.dateTimeFromWire(int64(binary.LittleEndian.Uint64(b)))
		if err != nil {
			return nil, 0, err
		}
		return v, 8, nil

	case tagString:
		if err := need(b, 4); err != nil {
			return nil, 0, err
		}

		n := int(int32(binary.LittleEndian.Uint32(b)))
		if n < 1 {
			return nil, 0, fmt.Errorf("%w: string length %d below minimum 1", ErrCorrupt, n)
		}
		if err := need(b, 4+n); err != nil {
			return nil, 0, err
		}
		if b[4+n-1] != 0 {
			return nil, 0, fmt.Errorf("%w: unterminated string", ErrCorrupt)
		}
		s := string(b[4 : 4+n-1])
		if err := xbin.ValidateString(s); err != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return String(s), 4 + n, nil

	case tagBinary:
		return decodeBinary(b)

	case tagVector:
		if err := need(b, 2); err != nil {
			return nil, 0, err
		}
		n := int(binary.LittleEndian.Uint16(b))
		if err := need(b, 2+4*n); err != nil {
			return nil, 0, err
		}
		f := make([]float32, n)
		for i := range f {
			f[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[2+4*i:]))
		}
		return Vector(f), 2 + 4*n, nil

	case tagDocument:
		d, n, err := dec.decodeDocument(b)
		if err != nil {
			return nil, 0, err
		}
		return d.Value(), n, nil

	case tagArray:
		a, n, err := dec.decodeArray(b)
		if err != nil {
			return nil, 0, err
		}
		return a.Value(), n, nil

	default:
		return nil, 0, fmt.Errorf("%w: unknown type tag 0x%02X", ErrCorrupt, tag)
	}
}

// decodeBinary 解出二进制载荷，子类型是 UUID 时解成标识。
//
// 内容**拷一份**：原来那段字节可能是一页缓冲的一部分，
// 直接引用会让整页被这个值拖住不放。
func decodeBinary(b []byte) (*Value, int, error) {
	if err := need(b, 5); err != nil {
		return nil, 0, err
	}
	n := int(int32(binary.LittleEndian.Uint32(b)))
	if n < 0 {
		return nil, 0, fmt.Errorf("%w: negative binary length %d", ErrCorrupt, n)
	}
	if err := need(b, 5+n); err != nil {
		return nil, 0, err
	}
	sub, raw := b[4], b[5:5+n]
	if sub == subtypeUUID {
		if n != 16 {
			return nil, 0, fmt.Errorf("%w: uuid payload is %d bytes, want 16", ErrCorrupt, n)
		}
		g, err := xbin.GuidFromBytes(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return GUID(g), 5 + n, nil
	}

	return Binary(append([]byte(nil), raw...)), 5 + n, nil
}

// dateTimeFromWire 把文件里的 Unix 毫秒转成日期值。
//
// 两个端点特判，与写入侧对称。
func (dec decoder) dateTimeFromWire(ms int64) (*Value, error) {
	switch ms {
	case xbin.MinUnixMillis:
		return dateTimeTicks(xbin.MinTicks, dec.loc), nil
	case xbin.MaxUnixMillis:
		return dateTimeTicks(xbin.MaxTicks, dec.loc), nil
	}
	t, err := xbin.UnixMillisToTime(ms)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	v, err := DateTime(t)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return v.In(dec.loc), nil
}

// need 检查手上还有没有 n 个字节。
func need(b []byte, n int) error {
	if len(b) < n {
		return fmt.Errorf("%w: need %d bytes, have %d", ErrCorrupt, n, len(b))
	}
	return nil
}
