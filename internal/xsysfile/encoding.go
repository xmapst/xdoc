package xsysfile

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// lookupEncoding 按名字查字符编码，并给出它的 BOM。
//
// 编码名必填：默认成某一种会让导出的文件在另一台机器上读成乱码。
// 先按 IANA 的名字查，查不到再按 HTML 那套别名查。
func (o Options) lookupEncoding() (encoding.Encoding, []byte, error) {
	if strings.TrimSpace(o.Encoding) == "" {
		return nil, nil, fmt.Errorf("'%s' is not a supported encoding name", o.Encoding)
	}
	enc, err := ianaindex.IANA.Encoding(o.Encoding)
	if err != nil || enc == nil {
		if e2, err2 := htmlindex.Get(o.Encoding); err2 == nil && e2 != nil {
			enc = e2
		} else {
			return nil, nil, fmt.Errorf("'%s' is not a supported encoding name", o.Encoding)
		}
	}
	return enc, preambleOf(o.Encoding), nil
}

// preambleOf 返回某种编码要写在文件开头的 BOM。
//
// 只有 Unicode 那几种有；其余返回 nil。
func preambleOf(name string) []byte {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "utf-8", "utf8", "utf_8":
		return []byte{0xEF, 0xBB, 0xBF}
	case "utf-16", "utf16", "unicode", "utf-16le", "utf16le":
		return []byte{0xFF, 0xFE}
	case "utf-16be", "utf16be", "unicodefffe":
		return []byte{0xFE, 0xFF}
	case "utf-32", "utf32", "utf-32le", "utf32le":
		return []byte{0xFF, 0xFE, 0x00, 0x00}
	case "utf-32be", "utf32be":
		return []byte{0x00, 0x00, 0xFE, 0xFF}
	}
	return nil
}

// decodeReader 把字节流解码成 UTF-8，并吃掉开头的 BOM。
//
// 解码之后再去 BOM：各种编码的 BOM 字节形态不同，但解出来都是同一个字符。
func decodeReader(r io.Reader, enc encoding.Encoding) io.Reader {
	return transform.NewReader(r, transform.Chain(enc.NewDecoder(), &skipBOM{}))
}

// encodeWriter 把 UTF-8 编码成目标编码写出去。
func encodeWriter(w io.Writer, enc encoding.Encoding) io.Writer {
	return transform.NewWriter(w, enc.NewEncoder())
}

// skipBOM 只在流的最开头吃掉一个 BOM 字符。
type skipBOM struct{ done bool }

// Reset 让它重新开始，供转换链复用。
func (t *skipBOM) Reset() { t.done = false }

// Transform 检查开头有没有 BOM，之后原样转发。
//
// 手上的字节还不够判断且流没到头时要求补充：BOM 是三个字节，
// 不等齐就可能把它劈成两半。
func (t *skipBOM) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	if !t.done {
		const bom = "\ufeff"
		if len(src) < len(bom) && !atEOF {
			return 0, 0, transform.ErrShortSrc
		}
		if strings.HasPrefix(string(src), bom) {
			src = src[len(bom):]
			nSrc = len(bom)
		}
		t.done = true
	}
	n := copy(dst, src)
	if n < len(src) {
		return n, nSrc + n, transform.ErrShortDst
	}
	return n, nSrc + n, nil
}
