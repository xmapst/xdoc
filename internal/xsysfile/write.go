package xsysfile

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"iter"
	"os"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xjson"
)

// newline 是写文件时的换行。
//
// 固定用它，不跟着运行平台走：同一次导出在任何系统上都产出同样的字节。
const newline = "\n"

// output 是一次写文件用到的三层：底下的文件、中间的编码器、最外面的缓冲。
//
// 绑成一个类型是因为收尾时一层都不能少：缓冲里压着还没写出去的字节，
// 编码器可能压着一个多字节字符的前半截，而文件描述符无论成败都要关。
type output struct {
	// f 是文件本身，收尾时要关。
	f *os.File

	// w 是编码那一层，关掉它才会把压着的半个字符吐出来。
	w io.Writer

	// bw 是缓冲那一层，所有写入都走它。
	bw *bufio.Writer
}

// create 建输出文件，三层都套好。
//
// 目标已存在且没说要覆盖时报错。
//
// **说了覆盖时不截断**：只把游标放到 0 就开始写。新内容比旧内容短时，
// 旧文件超出的那一截原样留在后面——写出的 JSON 会因此不合法。
// 要覆盖又不想要残尾的话，先把目标删掉再导。
//
// BOM 直接写进原始字节流，不经过编码器：它本来就是那套编码的字节形态。
func (o Options) create() (*output, error) {
	enc, preamble, err := o.lookupEncoding()
	if err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if o.Overwritten {
		flag = os.O_WRONLY | os.O_CREATE
	}
	f, err := o.open(flag, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("the file '%s' already exists", o.Filename)
		}
		return nil, err
	}

	if len(preamble) > 0 {
		if _, err := f.Write(preamble); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	w := encodeWriter(f, enc)
	return &output{f: f, w: w, bw: bufio.NewWriter(w)}, nil
}

// WriteJSON 把一串文档写成一份 JSON 数组文件，返回写了几篇。
//
// **一篇都没有时文件根本不会建**，返回 0：一句筛不出东西的导出语句
// 不会留下空文件，也不会覆盖掉已有的同名文件。
//
// 排版：`[` 一行，每篇之间是 `,` 加换行，最后一篇之后换行再写 `]`，
// `]` 后面没有换行。
func (o Options) WriteJSON(docs iter.Seq2[*xbson.Document, error]) (int, error) {
	var (
		out *output

		jw  *xjson.Writer
		n   int
		err error
	)
	defer func() {
		if out != nil {
			_ = out.f.Close()
		}
	}()

	for d, derr := range docs {
		if derr != nil {
			return n, derr
		}
		if n == 0 {
			if out, err = o.create(); err != nil {
				return 0, err
			}
			if _, err = out.bw.WriteString("[" + newline); err != nil {
				return 0, err
			}
			jw = xjson.NewWriter(out.bw)
			jw.Pretty, jw.Indent = o.Pretty, o.Indent
		} else if _, err = out.bw.WriteString("," + newline); err != nil {
			return n, err
		}
		n++

		if err = jw.Write(d.Value()); err != nil {
			return n, err
		}
	}
	if n > 0 {
		if _, err = out.bw.WriteString(newline + "]"); err != nil {
			return n, err
		}
		if err = out.flush(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// flush 把缓冲与编码器里压着的字节都赶到文件里。
//
// 两层都要赶：缓冲攒着还没写的字节，而编码器可能压着一个多字节字符的
// 前半截，只有关掉它才会吐出来。少赶一层，文件末尾就会缺一小段。
func (t *output) flush() error {
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if c, ok := t.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// WriteCSV 把一串文档写成一份 CSV 文件，返回写了几篇。
//
// 列名取**第一篇文档的键序**，之后每篇都按这份列表取值：缺的字段写
// `null`，多出来的字段丢弃。所以一批字段不齐的文档写出来是一张齐整的表，
// 代价是后面文档里新出现的字段会静默消失。
//
// 行与行之间是换行，**最后一行之后没有换行**——[Options.ReadCSV] 因此
// 读不回最后一篇。
//
// 一篇都没有时文件不会建。
func (o Options) WriteCSV(docs iter.Seq2[*xbson.Document, error]) (int, error) {
	var (
		out    *output
		fields []string
		n      int
		err    error
	)
	defer func() {
		if out != nil {
			_ = out.f.Close()
		}
	}()

	for d, derr := range docs {
		if derr != nil {
			return n, derr
		}
		if n == 0 {
			if out, err = o.create(); err != nil {
				return 0, err
			}
			fields = d.Keys()
			if o.WriteHeader {
				for i, k := range fields {
					if i > 0 {
						_ = out.bw.WriteByte(o.Delimiter)
					}
					_, _ = out.bw.WriteString(k)
				}
				_, _ = out.bw.WriteString(newline)
			}
		} else {
			_, _ = out.bw.WriteString(newline)
		}
		n++

		for i, k := range fields {
			if i > 0 {
				_ = out.bw.WriteByte(o.Delimiter)
			}
			s, err := csvValue(d.Get(k))
			if err != nil {
				return n, err
			}
			_, _ = out.bw.WriteString(s)
		}
	}
	if n > 0 {
		if err = out.flush(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// csvValue 把一个值排成 CSV 里的一格。
//
// 空值、两个哨兵值、以及嵌套的文档与数组一律写成 `null`：一格里塞不下
// 结构化的东西。
//
// 标识、日期、二进制这些加引号，数字与其余类型走 JSON 那条路。
// **字段内容不做转义**：值里带分隔符或引号时写出来的表会错行。
func csvValue(v *xbson.Value) (string, error) {
	if v == nil {
		return "null", nil
	}
	switch v.Type() {
	case xbson.TypeMinValue, xbson.TypeNull, xbson.TypeDocument, xbson.TypeArray, xbson.TypeMaxValue:
		return "null", nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return strconv.FormatInt(n, 10), nil
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return d.String(), nil
	case xbson.TypeBinary:
		b, _ := v.AsBinary()
		return `"` + base64.StdEncoding.EncodeToString(b) + `"`, nil
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		return `"` + id.String() + `"`, nil
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		return `"` + g.String() + `"`, nil
	case xbson.TypeDateTime:
		t, ok := v.AsTime()
		if !ok {
			return "", fmt.Errorf("xsysfile: date value cannot be rendered as a time")
		}
		return `"` + t.UTC().Format("2006-01-02T15:04:05.0000000") + `Z"`, nil
	}
	b, err := xjson.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
