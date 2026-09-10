// Package xsysfile 是集合与磁盘上 JSON、CSV 文件之间的导入导出。
//
// 两侧都是流式的：导出时一次只有一篇文档在内存里，导入时也一样，
// 所以一份很大的文件也导得动。
//
// 字符编码必须显式指定，写出的换行固定是 \n——同一次导出在任何系统上
// 产出同样的字节。
package xsysfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xjson"
)

// Options 是一次导入或导出的全部参数。
type Options struct {
	// Filename 是目标文件，Encoding 是它的字符编码（必填）。
	//
	// CSVHeader 给定时导入不再从首行读列名；Indent/Pretty 控制 JSON 排版；
	// Overwritten 允许写已存在的文件；Delimiter 是 CSV 分隔符；
	// WriteHeader 控制导出 CSV 时写不写列名行。
	Filename    string
	Encoding    string
	CSVHeader   []string
	Indent      int
	Pretty      bool
	Overwritten bool
	Delimiter   byte
	WriteHeader bool

	// OpenFile 打开 Filename，签名同 [os.OpenFile]；为 nil 时直接用 os.OpenFile。
	//
	// 调用方靠它把读写限定在某个目录之下。
	OpenFile func(name string, flag int, perm os.FileMode) (*os.File, error)
}

// open 用 [Options.OpenFile] 打开目标文件。
func (o Options) open(flag int, perm os.FileMode) (*os.File, error) {
	if o.OpenFile != nil {
		return o.OpenFile(o.Filename, flag, perm)
	}
	return os.OpenFile(o.Filename, flag, perm)
}

// ReadJSON 把一份 JSON 数组文件读成一串文档。
//
// 流式读：一次只有一篇文档在内存里。数组里出现非文档的元素时报错。
func (o Options) ReadJSON() iter.Seq2[*xbson.Document, error] {
	return func(yield func(*xbson.Document, error) bool) {
		enc, _, err := o.lookupEncoding()
		if err != nil {
			yield(nil, err)
			return
		}
		f, err := o.open(os.O_RDONLY, 0)
		if err != nil {
			yield(nil, err)
			return
		}
		defer f.Close()

		r := xjson.NewReader(decodeReader(bufio.NewReader(f), enc))
		for v, err := range r.Elements() {
			if err != nil {
				yield(nil, err)
				return
			}
			d, ok := v.AsDocument()
			if !ok {
				yield(nil, fmt.Errorf("xsysfile: %s: array element is a %s, not a document",
					o.Filename, v.Type()))
				return
			}
			if !yield(d, nil) {
				return
			}
		}
	}
}

// ReadCSV 把一份 CSV 文件读成一串文档，**每个字段都是字符串**。
//
// 列名取首行，除非 [Options.CSVHeader] 已经给了。比列名多出来的字段丢弃，
// 少的则那几个键不出现。
//
// **最后一行没有换行时会被丢掉**：一行是靠行尾的换行才算读完的，
// 而 [Options.WriteCSV] 写出来的文件末尾正好没有换行——
// 所以导出再导入会少最后一篇。
func (o Options) ReadCSV() iter.Seq2[*xbson.Document, error] {
	return func(yield func(*xbson.Document, error) bool) {
		enc, _, err := o.lookupEncoding()
		if err != nil {
			yield(nil, err)
			return
		}
		f, err := o.open(os.O_RDONLY, 0)
		if err != nil {
			yield(nil, err)
			return
		}
		defer f.Close()

		br := bufio.NewReader(decodeReader(bufio.NewReader(f), enc))
		cr := &csvReader{r: br, delim: rune(o.Delimiter)}

		header := o.CSVHeader
		if len(header) == 0 {
			for {
				key, newLine, err := cr.field()
				if err != nil {
					yield(nil, err)
					return
				}
				if key == nil {
					break
				}
				header = append(header, *key)
				if newLine {
					break
				}
			}
		}

		i := 0
		d := xbson.NewDocument()
		for {
			value, newLine, err := cr.field()
			if err != nil {
				yield(nil, err)
				return
			}
			if value == nil {
				return
			}
			if i < len(header) {
				d.Set(header[i], xbson.String(*value))
				i++
			}
			if newLine {
				if !yield(d, nil) {
					return
				}
				d = xbson.NewDocument()
				i = 0
			}
		}
	}
}

// csvReader 逐字段读 CSV，自己处理引号，不用标准库的实现。
type csvReader struct {
	r     *bufio.Reader
	delim rune
}

// errEOF 是内部用的读完标记。
//
// 与 [io.EOF] 分开，是为了在下面的多处判断里区分「读完了」与
// 「底层报了个恰好是 io.EOF 的错」。
var errEOF = errors.New("eof")

// read 读一个字符，读完返回 errEOF。
func (c *csvReader) read() (rune, error) {
	ch, _, err := c.r.ReadRune()
	if err == io.EOF {
		return 0, errEOF
	}
	return ch, err
}

// field 读出一个字段，第二个返回值说明这个字段是不是一行的最后一个。
//
// 返回 nil 表示文件读完了。
//
// 先跳过连续的换行：空行不产生记录。带引号的字段里，两个连续引号
// 表示一个字面引号。
func (c *csvReader) field() (*string, bool, error) {
	ch, err := c.read()

	for err == nil && (ch == '\n' || ch == '\r') {
		ch, err = c.read()
	}
	if err == errEOF {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	var sb []rune
	if ch == '"' {
		for err == nil {
			if ch, err = c.read(); err != nil {
				break
			}
			if ch == '"' {
				next, nerr := c.read()
				if nerr == nil && next == '"' {
					sb = append(sb, '"')
					continue
				}
				if nerr != nil && nerr != errEOF {
					return nil, false, nerr
				}
				ch = next
				if nerr == errEOF {
					ch = 0
				}
				break
			}
			sb = append(sb, ch)
		}
		if err != nil && err != errEOF {
			return nil, false, err
		}
	} else {
		for err == nil && ch != '\n' && ch != '\r' && ch != c.delim {
			sb = append(sb, ch)
			ch, err = c.read()
		}
		if err != nil && err != errEOF {
			return nil, false, err
		}
		if err == errEOF {
			ch = 0
		}
	}
	s := string(sb)
	return &s, ch == '\n' || ch == '\r', nil
}
